package conformance

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"

	"github.com/loewenthal-corp/shpiel/internal/app"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
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
