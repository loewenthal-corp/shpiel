package conformance

import (
	"net/http/httptest"
	"os"
	"testing"
	"time"

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
