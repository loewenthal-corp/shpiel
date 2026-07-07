//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// TestXetUploadAndChunkDownload proves the Xet milestone with the real
// hf_xet client, defaults untouched: uploads flow through Shpiel's CAS API
// (xorbs + shard), files materialize into the backend for regular HTTP
// consumers, and downloads reconstruct chunk-level through the
// reconstruction API.
//
// The backend is a real Zot in tar-layers format — the cluster
// configuration where >8 MiB materializations once died in the OCI
// chunked-commit path (416 BLOB_UPLOAD_INVALID); the fs backend cannot
// see that class of failure.
func TestXetUploadAndChunkDownload(t *testing.T) {
	requireDocker(t)
	repoRoot := repoRoot(t)

	zotPort := freePort(t)
	startZot(t, zotPort)

	sh := startShpielWithConfig(t, repoRoot, func(apiPort, metricsPort int) string {
		return fmt.Sprintf(`---
listen:
  api: "0.0.0.0:%d"
  metrics: "127.0.0.1:%d"
backends:
  zot:
    type: oci
    url: http://127.0.0.1:%d
    format: tar-layers
routes:
  - match: "*"
    primary: zot
xet:
  enabled: true
  data_dir: %s
log:
  level: debug
  format: text
`, apiPort, metricsPort, zotPort, t.TempDir())
	})
	buildClientImage(t, repoRoot)

	out := runClient(t, repoRoot, []string{
		"-e", fmt.Sprintf("HF_ENDPOINT=http://host.docker.internal:%d", sh.apiPort),
		"-e", "E2E_REPO=fixtures/xet-model",
		"-e", "HF_HUB_DISABLE_TELEMETRY=1",
		// Deliberately NOT setting HF_HUB_DISABLE_XET: this test exists to
		// prove unmodified hub 1.x works against Shpiel.
	}, "/client/xet_verify.py")
	if !strings.Contains(out, "E2E_XET_OK") {
		t.Fatalf("xet verification did not reach E2E_XET_OK:\n%s", out)
	}

	// The flows really went through the CAS API: every xet handler shows
	// up in the request metrics.
	metrics := httpGetBody(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", sh.metricsPort))
	for _, handler := range []string{"xet_token", "xet_xorb", "xet_shard", "xet_reconstruction", "xet_data"} {
		if !strings.Contains(metrics, `handler="`+handler+`"`) {
			t.Errorf("metrics show no requests through %s — the client bypassed the xet path", handler)
		}
	}

	// Global dedup: the real client probed /xet/v1/chunks and accepted the
	// answer. Eligibility is a ~1/1024-per-chunk sampled property of the
	// content, but the fixture is fully deterministic, so it reliably
	// carries an eligible chunk — if you change the fixture bytes and this
	// fails, regenerate content that still probes. Hit-vs-miss is not
	// pinned (the client dedups from its local shard cache within one
	// session); the hermetic protocol tests in internal/xet pin the hit
	// path deterministically.
	probed := false
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "shpiel_xet_dedup_events_total") {
			t.Logf("dedup metric: %s", line)
			if strings.Contains(line, `event="query_hit"`) || strings.Contains(line, `event="query_miss"`) {
				probed = true
			}
		}
	}
	if !probed {
		t.Error("the client never probed the global-dedup endpoint (fixture lost its eligible chunk?)")
	}
}
