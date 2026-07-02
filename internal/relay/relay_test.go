package relay

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/upstream"
)

// harness wires a relay to a temp FS backend and a fakehub upstream.
type harness struct {
	hub   *fakehub.Hub
	relay *Relay
	bk    backend.Backend
}

func newHarness(t *testing.T, refresh time.Duration) *harness {
	t.Helper()
	hub := fakehub.New()
	hubSrv := httptest.NewServer(hub.Handler())
	t.Cleanup(hubSrv.Close)

	bk, err := fsbackend.New("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(
		[]config.Route{{Match: "*", Primary: "test"}},
		map[string]backend.Backend{"test": bk},
	)
	if err != nil {
		t.Fatal(err)
	}
	rl := New(Options{
		Router:          router,
		Upstream:        upstream.New(hubSrv.URL, ""),
		RefreshInterval: refresh,
	})
	return &harness{hub: hub, relay: rl, bk: bk}
}

var testFiles = map[string][]byte{
	"config.json":           []byte(`{"model_type":"tiny"}`),
	"model.safetensors":     make([]byte, 4096), // >=1KiB => LFS in fakehub
	"nested/tokenizer.json": []byte(`{"tok":true}`),
}

func TestPullThroughManifestAndBlob(t *testing.T) {
	t.Parallel()
	h := newHarness(t, time.Hour)
	commit := h.hub.AddModel("org/tiny", testFiles)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/tiny")

	m, err := h.relay.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatalf("ResolveManifest: %v", err)
	}
	if m.CommitSHA != commit {
		t.Fatalf("commit = %s, want %s", m.CommitSHA, commit)
	}
	if len(m.Files) != len(testFiles) {
		t.Fatalf("files = %d, want %d", len(m.Files), len(testFiles))
	}

	// LFS detection came through from upstream metadata.
	weights := m.File("model.safetensors")
	if weights == nil || weights.LFS == nil || weights.LFS.SHA256 == "" {
		t.Fatalf("model.safetensors entry not LFS: %+v", weights)
	}

	content, err := h.relay.OpenFile(ctx, hfapi.RepoKindModel, repo, m, "model.safetensors", "")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	data, _ := io.ReadAll(content)
	content.Close()
	if len(data) != 4096 {
		t.Fatalf("blob size = %d, want 4096", len(data))
	}
	if content.Source != "upstream" {
		t.Fatalf("first read source = %s, want upstream", content.Source)
	}

	// Second read is served from the backend without upstream traffic.
	before := h.hub.Requests("GET", "/org/tiny/resolve")
	content2, err := h.relay.OpenFile(ctx, hfapi.RepoKindModel, repo, m, "model.safetensors", "")
	if err != nil {
		t.Fatalf("OpenFile(2): %v", err)
	}
	content2.Close()
	if content2.Source != "cache" {
		t.Fatalf("second read source = %s, want cache", content2.Source)
	}
	if after := h.hub.Requests("GET", "/org/tiny/resolve"); after != before {
		t.Fatalf("second read hit upstream (%d -> %d requests)", before, after)
	}
}

func TestManifestCachedWithinRefreshInterval(t *testing.T) {
	t.Parallel()
	h := newHarness(t, time.Hour)
	h.hub.AddModel("org/cached", testFiles)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/cached")

	for range 3 {
		if _, err := h.relay.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.hub.Requests("GET", "/api/models/org/cached"); n != 1 {
		t.Fatalf("upstream manifest fetches = %d, want 1 (fresh ref must be served locally)", n)
	}
}

func TestStaleRefRevalidates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1) // 1ns: everything is instantly stale
	commit1 := h.hub.AddModel("org/moving", map[string][]byte{"a.txt": []byte("v1")})
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/moving")

	m1, err := h.relay.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if m1.CommitSHA != commit1 {
		t.Fatalf("commit = %s, want %s", m1.CommitSHA, commit1)
	}

	// Branch moves upstream; the relay must pick it up.
	commit2 := h.hub.AddModel("org/moving", map[string][]byte{"a.txt": []byte("v2")})
	if commit1 == commit2 {
		t.Fatal("fixture bug: commits identical")
	}
	m2, err := h.relay.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if m2.CommitSHA != commit2 {
		t.Fatalf("after branch move commit = %s, want %s", m2.CommitSHA, commit2)
	}

	// Pinning the old commit SHA still serves the old snapshot.
	mOld, err := h.relay.ResolveManifest(ctx, hfapi.RepoKindModel, repo, commit1, "")
	if err != nil {
		t.Fatalf("ResolveManifest(old sha): %v", err)
	}
	if mOld.CommitSHA != commit1 {
		t.Fatalf("pinned commit = %s, want %s", mOld.CommitSHA, commit1)
	}
}

func TestUpstreamDownServesStale(t *testing.T) {
	t.Parallel()
	hub := fakehub.New()
	hubSrv := httptest.NewServer(hub.Handler())

	bk, _ := fsbackend.New("test", t.TempDir())
	router, _ := NewRouter([]config.Route{{Match: "*", Primary: "test"}}, map[string]backend.Backend{"test": bk})
	rl := New(Options{Router: router, Upstream: upstream.New(hubSrv.URL, ""), RefreshInterval: 1})

	hub.AddModel("org/resilient", map[string][]byte{"a.txt": []byte("v1")})
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/resilient")

	m1, err := rl.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatal(err)
	}

	hubSrv.Close() // upstream goes away

	m2, err := rl.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatalf("stale ref with dead upstream must serve, got %v", err)
	}
	if m2.CommitSHA != m1.CommitSHA {
		t.Fatalf("stale serve commit = %s, want %s", m2.CommitSHA, m1.CommitSHA)
	}
}

func TestSingleflightCollapsesBlobFetches(t *testing.T) {
	t.Parallel()
	h := newHarness(t, time.Hour)
	h.hub.AddModel("org/flock", testFiles)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/flock")

	m, err := h.relay.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatal(err)
	}

	// A fleet of nodes pulls the same file at once.
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := h.relay.OpenFile(ctx, hfapi.RepoKindModel, repo, m, "model.safetensors", "")
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = io.Copy(io.Discard, c)
			c.Close()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
	}
	// Singleflight must collapse concurrent upstream fetches; allow a tiny
	// bit of slack for goroutines that miss the flight window.
	if got := h.hub.Requests("GET", "/cdn/"); got > 2 {
		t.Fatalf("upstream blob fetches = %d, want <= 2 (singleflight)", got)
	}
}

func TestNotFoundMapping(t *testing.T) {
	t.Parallel()
	h := newHarness(t, time.Hour)
	h.hub.AddModel("org/exists", map[string][]byte{"a.txt": []byte("hi")})
	ctx := context.Background()

	repo, _ := hfapi.ParseRepoID("org/missing")
	if _, err := h.relay.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", ""); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("missing repo = %v, want ErrRepoNotFound", err)
	}

	exists, _ := hfapi.ParseRepoID("org/exists")
	if _, err := h.relay.ResolveManifest(ctx, hfapi.RepoKindModel, exists, "no-such-branch", ""); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("missing revision = %v, want ErrRevisionNotFound", err)
	}

	m, err := h.relay.ResolveManifest(ctx, hfapi.RepoKindModel, exists, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.relay.OpenFile(ctx, hfapi.RepoKindModel, exists, m, "no-such-file", ""); !errors.Is(err, ErrEntryNotFound) {
		t.Errorf("missing file = %v, want ErrEntryNotFound", err)
	}
}

func TestNoPullThroughWithoutUpstream(t *testing.T) {
	t.Parallel()
	bk, _ := fsbackend.New("test", t.TempDir())
	router, _ := NewRouter([]config.Route{{Match: "*", Primary: "test"}}, map[string]backend.Backend{"test": bk})
	rl := New(Options{Router: router}) // no upstream

	repo, _ := hfapi.ParseRepoID("org/anything")
	_, err := rl.ResolveManifest(context.Background(), hfapi.RepoKindModel, repo, "main", "")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("air-gapped miss = %v, want ErrRepoNotFound", err)
	}
}

func TestRouterMatching(t *testing.T) {
	t.Parallel()
	bkA, _ := fsbackend.New("a", t.TempDir())
	bkB, _ := fsbackend.New("b", t.TempDir())
	router, err := NewRouter(
		[]config.Route{
			{Match: "exigence/*", Primary: "a"},
			{Match: "*", Primary: "b"},
		},
		map[string]backend.Backend{"a": bkA, "b": bkB},
	)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		repo string
		want string
	}{
		{"exigence/gemma-ft", "a"},
		{"other/model", "b"},
		{"gpt2", "b"}, // bare name hits the catch-all
	}
	for _, tc := range cases {
		id, _ := hfapi.ParseRepoID(tc.repo)
		route := router.For(id)
		if route == nil {
			t.Fatalf("no route for %s", tc.repo)
		}
		if route.Primary.Name() != tc.want {
			t.Errorf("route for %s = %s, want %s", tc.repo, route.Primary.Name(), tc.want)
		}
	}
}
