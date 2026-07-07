package conformance

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"

	"github.com/loewenthal-corp/shpiel/internal/app"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/fakes3"
)

// newShpiel builds a full Shpiel (production wiring via app.Build) on a
// temp FS backend, optionally pointed at an upstream URL, mounted on an
// httptest server.
func newShpiel(t *testing.T, upstreamURL string) (*app.App, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen.Metrics = ""
	cfg.Log.Format = "text"
	cfg.Log.Level = "warn"
	cfg.Backends = map[string]config.BackendConfig{
		"fs": {Type: "fs", Path: t.TempDir()},
	}
	cfg.Routes = []config.Route{{Match: "*", Primary: "fs"}}
	if upstreamURL != "" {
		cfg.Upstream.HuggingFace.PullThrough = true
		cfg.Upstream.HuggingFace.Endpoint = upstreamURL
		cfg.Upstream.HuggingFace.RefreshInterval = time.Hour
	}

	a, err := app.Build(cfg)
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	srv := httptest.NewServer(a.Server.Handler())
	t.Cleanup(srv.Close)
	return a, srv.URL
}

// TestConformanceDirectSeeded runs the contract against a Shpiel whose
// backend already holds the fixture — the pure read path (air-gapped mode,
// or everything already pulled through).
func TestConformanceDirectSeeded(t *testing.T) {
	t.Parallel()
	a, url := newShpiel(t, "")
	fx, err := SeedBackend(a.Relay.Backends()[0], BasicFixture())
	if err != nil {
		t.Fatalf("SeedBackend: %v", err)
	}
	Run(t, url, fx)
}

// TestConformancePullThrough runs the identical contract against a Shpiel
// with an empty backend that must pull everything from a fakehub upstream.
// The two tests passing together prove cache-miss and cache-hit serving
// are indistinguishable to clients.
func TestConformancePullThrough(t *testing.T) {
	t.Parallel()
	hub := fakehub.New()
	hubSrv := httptest.NewServer(hub.Handler())
	t.Cleanup(hubSrv.Close)
	fx := SeedHub(hub, BasicFixture())

	_, url := newShpiel(t, hubSrv.URL)
	Run(t, url, fx)
}

// TestConformanceWriteThenRead pushes the fixture through the full HF
// write protocol (create repo, preupload, LFS batch + PUT, NDJSON commit)
// and then runs the read contract against what was written. Writes and
// reads proving each other is the core compatibility loop: what
// push_to_hub lands, from_pretrained must serve.
func TestConformanceWriteThenRead(t *testing.T) {
	t.Parallel()
	_, url := newShpiel(t, "")
	client := NewWriteClient(url, "")

	fx, err := client.PushFixture(BasicFixture())
	if err != nil {
		t.Fatalf("PushFixture: %v", err)
	}
	Run(t, url, fx)

	t.Run("RepushDedupsLFS", func(t *testing.T) {
		lfs := map[string][]byte{}
		for path, f := range fx.Files {
			if f.LFS {
				lfs[path] = f.Content
			}
		}
		uploaded, err := client.UploadLFS(fx.Repo, lfs)
		if err != nil {
			t.Fatal(err)
		}
		if uploaded != 0 {
			t.Errorf("re-push uploaded %d blobs, want 0 (batch dedup)", uploaded)
		}
	})

	t.Run("NoOpCommitKeepsSHA", func(t *testing.T) {
		inline, lfs := map[string][]byte{}, map[string][]byte{}
		for path, f := range fx.Files {
			if f.LFS {
				lfs[path] = f.Content
			} else {
				inline[path] = f.Content
			}
		}
		sha, err := client.Commit(fx.Repo, "main", "retry", inline, lfs, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sha != fx.CommitSHA {
			t.Errorf("no-op commit moved branch: %s, want %s", sha, fx.CommitSHA)
		}
	})

	t.Run("DeleteFileCommit", func(t *testing.T) {
		sha, err := client.Commit(fx.Repo, "main", "delete", nil, nil, []string{"tokenizer/merges.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if sha == fx.CommitSHA {
			t.Fatal("deletion commit did not advance the branch")
		}
		// The deleted file 404s with the entry error code; others serve.
		resp, err := http.Get(url + "/" + fx.Repo + "/resolve/main/tokenizer/merges.txt")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("deleted file resolve = %d, want 404", resp.StatusCode)
		}
		resp, err = http.Get(url + "/" + fx.Repo + "/resolve/main/config.json")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("surviving file resolve = %d, want 200", resp.StatusCode)
		}
	})
}

// newShpielOCI builds a Shpiel whose primary backend is an OCI registry
// (in-process go-containerregistry) in the given format.
func newShpielOCI(t *testing.T, format, upstreamURL string) string {
	t.Helper()
	reg := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(reg.Close)

	cfg := config.Default()
	cfg.Listen.Metrics = ""
	cfg.Log.Format = "text"
	cfg.Log.Level = "warn"
	cfg.Backends = map[string]config.BackendConfig{
		"zot": {Type: "oci", URL: reg.URL, Format: format},
	}
	cfg.Routes = []config.Route{{Match: "*", Primary: "zot"}}
	if upstreamURL != "" {
		cfg.Upstream.HuggingFace.PullThrough = true
		cfg.Upstream.HuggingFace.Endpoint = upstreamURL
		cfg.Upstream.HuggingFace.RefreshInterval = time.Hour
	}
	a, err := app.Build(cfg)
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	srv := httptest.NewServer(a.Server.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestConformanceWriteThenReadOCI proves the M1 core: the full HF write
// protocol lands a model in an OCI registry, and the identical read
// contract is served back from those OCI artifacts — in both formats.
func TestConformanceWriteThenReadOCI(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"modelpack", "tar-layers"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			url := newShpielOCI(t, format, "")
			fx, err := NewWriteClient(url, "").PushFixture(BasicFixture())
			if err != nil {
				t.Fatalf("PushFixture: %v", err)
			}
			Run(t, url, fx)
		})
	}
}

// TestConformancePullThroughOCI runs the read contract against a Shpiel
// that pulls through from fakehub straight into an OCI registry — staged
// manifests promoting as blobs arrive.
func TestConformancePullThroughOCI(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"modelpack", "tar-layers"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			hub := fakehub.New()
			hubSrv := httptest.NewServer(hub.Handler())
			t.Cleanup(hubSrv.Close)
			fx := SeedHub(hub, BasicFixture())

			url := newShpielOCI(t, format, hubSrv.URL)
			Run(t, url, fx)
		})
	}
}

// newShpielS3 builds a Shpiel whose primary backend is an S3 bucket
// (in-process fakes3 with SigV4 verification on). No t.Parallel in its
// callers: credentials flow through t.Setenv.
func newShpielS3(t *testing.T, upstreamURL string) string {
	t.Helper()
	fake := fakes3.New("models-archive", "AKIDCONFORMANCE", "conformance-secret")
	reg := httptest.NewServer(fake)
	t.Cleanup(reg.Close)
	t.Setenv("CONF_S3_ACCESS_KEY", "AKIDCONFORMANCE")
	t.Setenv("CONF_S3_SECRET_KEY", "conformance-secret")

	cfg := config.Default()
	cfg.Listen.Metrics = ""
	cfg.Log.Format = "text"
	cfg.Log.Level = "warn"
	cfg.Backends = map[string]config.BackendConfig{
		"archive": {
			Type:     "s3",
			Bucket:   "models-archive",
			Endpoint: reg.URL,
			Prefix:   "shpiel",
			Auth: config.BackendAuth{ // #nosec G101 -- env var names, not credentials
				AccessKeyIDEnv:     "CONF_S3_ACCESS_KEY",
				SecretAccessKeyEnv: "CONF_S3_SECRET_KEY",
			},
		},
	}
	cfg.Routes = []config.Route{{Match: "*", Primary: "archive"}}
	if upstreamURL != "" {
		cfg.Upstream.HuggingFace.PullThrough = true
		cfg.Upstream.HuggingFace.Endpoint = upstreamURL
		cfg.Upstream.HuggingFace.RefreshInterval = time.Hour
	}
	a, err := app.Build(cfg)
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	srv := httptest.NewServer(a.Server.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestConformanceWriteThenReadS3 proves the bucket backend end to end:
// the full HF write protocol lands a model in an S3 bucket (SigV4-signed
// all the way), and the identical read contract is served back from those
// objects.
func TestConformanceWriteThenReadS3(t *testing.T) {
	url := newShpielS3(t, "")
	fx, err := NewWriteClient(url, "").PushFixture(BasicFixture())
	if err != nil {
		t.Fatalf("PushFixture: %v", err)
	}
	Run(t, url, fx)
}

// TestConformancePullThroughS3 runs the read contract against a Shpiel
// that pulls through from fakehub straight into a bucket — manifests
// landing before their blobs, blobs arriving lazily.
func TestConformancePullThroughS3(t *testing.T) {
	hub := fakehub.New()
	hubSrv := httptest.NewServer(hub.Handler())
	t.Cleanup(hubSrv.Close)
	fx := SeedHub(hub, BasicFixture())

	url := newShpielS3(t, hubSrv.URL)
	Run(t, url, fx)
}

// TestReplicationFanOutAndAudit pushes through the full write protocol on
// a route with a replica and proves (a) the commit fans out asynchronously
// — the replica ends up serving the identical read contract — and (b) the
// audit stream recorded the writes.
func TestReplicationFanOutAndAudit(t *testing.T) {
	t.Parallel()
	replicaDir := t.TempDir()
	auditPath := filepath.Join(t.TempDir(), "audit.log")

	cfg := config.Default()
	cfg.Listen.Metrics = ""
	cfg.Log.Format = "text"
	cfg.Log.Level = "warn"
	cfg.Log.AuditPath = auditPath
	cfg.Backends = map[string]config.BackendConfig{
		"primary": {Type: "fs", Path: t.TempDir()},
		"replica": {Type: "fs", Path: replicaDir},
	}
	cfg.Routes = []config.Route{{Match: "*", Primary: "primary", Replicas: []string{"replica"}}}
	cfg.Replication.SpoolDir = t.TempDir()

	a, err := app.Build(cfg)
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Replication.Run(ctx)
	srv := httptest.NewServer(a.Server.Handler())
	t.Cleanup(srv.Close)

	fx, err := NewWriteClient(srv.URL, "").PushFixture(BasicFixture())
	if err != nil {
		t.Fatalf("PushFixture: %v", err)
	}

	// Wait for the queue to drain, then serve the read contract from a
	// SECOND shpiel whose only backend is the replica: bytes, headers, and
	// error semantics must be identical to the primary.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && a.Replication.Depth() > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if depth := a.Replication.Depth(); depth != 0 {
		t.Fatalf("replication queue depth = %d after deadline; jobs: %+v", depth, a.Replication.Snapshot())
	}

	replicaCfg := config.Default()
	replicaCfg.Listen.Metrics = ""
	replicaCfg.Log.Format = "text"
	replicaCfg.Log.Level = "warn"
	replicaCfg.Backends = map[string]config.BackendConfig{"replica": {Type: "fs", Path: replicaDir}}
	replicaCfg.Routes = []config.Route{{Match: "*", Primary: "replica"}}
	ra, err := app.Build(replicaCfg)
	if err != nil {
		t.Fatal(err)
	}
	replicaSrv := httptest.NewServer(ra.Server.Handler())
	t.Cleanup(replicaSrv.Close)
	Run(t, replicaSrv.URL, fx)

	// The audit stream saw the writes.
	auditData, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	for _, action := range []string{`"action":"repo_create"`, `"action":"commit"`, `"action":"lfs_upload"`} {
		if !strings.Contains(string(auditData), action) {
			t.Errorf("audit log missing %s", action)
		}
	}
}

// TestConformanceExternal runs the suite against a live HF-compatible
// endpoint named by SHPIEL_CONFORMANCE_URL. The fixture repo must already
// exist there (seed it by pushing or pulling through conformance/basic).
// Skipped unless the variable is set.
func TestConformanceExternal(t *testing.T) {
	url := os.Getenv("SHPIEL_CONFORMANCE_URL")
	if url == "" {
		t.Skip("SHPIEL_CONFORMANCE_URL not set")
	}
	fx := BasicFixture()
	if sha := os.Getenv("SHPIEL_CONFORMANCE_COMMIT"); sha != "" {
		fx.CommitSHA = sha
	}
	Run(t, url, fx)
}
