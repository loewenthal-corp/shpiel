//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/ociclient"
)

// TestUploadLandsInZot proves the M1 exit criterion: `hf upload` /
// upload_folder against Shpiel lands a real OCI artifact in a real Zot
// registry, in both formats — and tar-layers artifacts have the standard
// image shape (OCI image config + tar layers) that Kubernetes image
// volumes mount.
func TestUploadLandsInZot(t *testing.T) {
	requireDocker(t)
	repoRoot := repoRoot(t)

	zotPort := freePort(t)
	startZot(t, zotPort)
	buildClientImage(t, repoRoot)

	for _, tc := range []struct {
		format string
		repo   string
	}{
		{format: "modelpack", repo: "e2e/zot-modelpack"},
		{format: "tar-layers", repo: "e2e/zot-tarlayers"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			sh := startShpielWithConfig(t, repoRoot, func(apiPort, metricsPort int) string {
				return fmt.Sprintf(`---
listen:
  api: "0.0.0.0:%d"
  metrics: "127.0.0.1:%d"
backends:
  zot:
    type: oci
    url: http://127.0.0.1:%d
    format: %s
routes:
  - match: "*"
    primary: zot
log:
  level: debug
  format: text
`, apiPort, metricsPort, zotPort, tc.format)
			})

			out := runClient(t, repoRoot, []string{
				"-e", fmt.Sprintf("HF_ENDPOINT=http://host.docker.internal:%d", sh.apiPort),
				"-e", "E2E_REPO=" + tc.repo,
				"-e", "HF_HUB_DISABLE_TELEMETRY=1",
				"-e", "HF_HUB_DISABLE_XET=1",
			}, "/client/upload.py")
			if !strings.Contains(out, "E2E_UPLOAD_OK") {
				t.Fatalf("upload against zot-backed shpiel failed:\n%s", out)
			}

			// upload.py ends with: config.json + model.safetensors kept,
			// vae/config.json deleted, extra.safetensors added via hf CLI.
			verifyZotArtifact(t,
				fmt.Sprintf("http://127.0.0.1:%d", zotPort),
				"models/"+tc.repo, tc.format,
				map[string]bool{
					"config.json":        true,
					"model.safetensors":  true,
					"extra.safetensors":  true,
				})
		})
	}
}

// startZot runs a Zot registry container bound to port.
func startZot(t *testing.T, port int) {
	t.Helper()
	name := fmt.Sprintf("shpiel-e2e-zot-%d", port)
	cmd := exec.Command("docker", "run", "-d", "--rm",
		"--name", name,
		"-p", fmt.Sprintf("%d:5000", port),
		"ghcr.io/project-zot/zot:v2.1.8")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("starting zot: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	deadline := time.Now().Add(60 * time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d/v2/", port)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("zot never became ready at %s", url)
}

// verifyZotArtifact asserts the artifact Shpiel pushed is present and
// well-formed straight from Zot's registry API.
func verifyZotArtifact(t *testing.T, zotURL, ociRepo, format string, wantFiles map[string]bool) {
	t.Helper()
	client, err := ociclient.New(zotURL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	tags, err := client.ListTags(ctx, ociRepo)
	if err != nil {
		t.Fatalf("listing tags in zot: %v", err)
	}
	if !contains(tags, "main") {
		t.Fatalf("zot %s tags = %v, want main", ociRepo, tags)
	}

	m, err := client.GetManifest(ctx, ociRepo, "main")
	if err != nil {
		t.Fatalf("fetching manifest from zot: %v", err)
	}
	commit := m.Annotations["org.shpiel.commit"]
	if len(commit) != 40 {
		t.Errorf("manifest commit annotation = %q", commit)
	}
	if !contains(tags, commit) {
		t.Errorf("commit tag %s missing from zot (tags: %v)", commit, tags)
	}

	titles := map[string]bool{}
	for _, layer := range m.Layers {
		titles[layer.Annotations["org.opencontainers.image.title"]] = true
		switch format {
		case "modelpack":
			if layer.MediaType != "application/vnd.cncf.model.weight.v1.raw" {
				t.Errorf("modelpack layer mediaType = %s", layer.MediaType)
			}
		case "tar-layers":
			if layer.MediaType != ociclient.MediaTypeOCILayerTar {
				t.Errorf("tar layer mediaType = %s", layer.MediaType)
			}
		}
		if size, err := client.HeadBlob(ctx, ociRepo, layer.Digest); err != nil || size != layer.Size {
			t.Errorf("layer %s blob missing or size mismatch: %d, %v", layer.Digest, size, err)
		}
	}
	for path := range wantFiles {
		if !titles[path] {
			t.Errorf("no layer annotated with file %q (layers: %v)", path, titles)
		}
	}

	switch format {
	case "modelpack":
		if m.ArtifactType != "application/vnd.cncf.model.manifest.v1+json" {
			t.Errorf("modelpack artifactType = %s", m.ArtifactType)
		}
	case "tar-layers":
		// The mountability contract: a plain OCI image config and tar
		// layers, nothing containerd would refuse.
		if m.Config.MediaType != ociclient.MediaTypeOCIConfig {
			t.Errorf("tar-layers config mediaType = %s, want %s", m.Config.MediaType, ociclient.MediaTypeOCIConfig)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
