package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/metrics"
	"github.com/loewenthal-corp/shpiel/internal/relay"
	"github.com/loewenthal-corp/shpiel/internal/upstream"
)

func newTestServer(t *testing.T, mutate func(*config.Config), up *upstream.Client) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Backends = map[string]config.BackendConfig{"fs": {Type: "fs", Path: t.TempDir()}}
	cfg.Routes = []config.Route{{Match: "*", Primary: "fs"}}
	if mutate != nil {
		mutate(&cfg)
	}
	bk, err := fsbackend.New("fs", cfg.Backends["fs"].Path)
	if err != nil {
		t.Fatal(err)
	}
	router, err := relay.NewRouter(cfg.Routes, map[string]backend.Backend{"fs": bk})
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	rl := relay.New(relay.Options{Router: router, Upstream: up, Metrics: m})
	srv := httptest.NewServer(New(cfg, rl, m, Options{Upstream: up}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestWhoAmILocalMode(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, nil, nil)
	resp, err := http.Get(srv.URL + "/api/whoami-v2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var who struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatal(err)
	}
	if who.Type != "user" || who.Name == "" {
		t.Errorf("whoami = %+v", who)
	}
}

func TestWhoAmIPassthrough(t *testing.T) {
	t.Parallel()
	hub := fakehub.New()
	hubSrv := httptest.NewServer(hub.Handler())
	t.Cleanup(hubSrv.Close)
	up := upstream.New(hubSrv.URL, "")

	srv := newTestServer(t, func(c *config.Config) { c.Auth.Mode = "passthrough" }, up)

	// No token: 401 without touching upstream.
	resp, err := http.Get(srv.URL + "/api/whoami-v2")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous whoami = %d, want 401", resp.StatusCode)
	}

	// Token: proxied to upstream, which recognizes it.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/whoami-v2", nil)
	req.Header.Set("Authorization", "Bearer hf_testtoken")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authed whoami = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "fakeuser") {
		t.Errorf("whoami body not proxied from upstream: %s", body)
	}
}

func TestWriteAuthPassthrough(t *testing.T) {
	t.Parallel()
	hub := fakehub.New()
	hubSrv := httptest.NewServer(hub.Handler())
	t.Cleanup(hubSrv.Close)
	up := upstream.New(hubSrv.URL, "")

	srv := newTestServer(t, func(c *config.Config) { c.Auth.Mode = "passthrough" }, up)
	createBody := `{"name":"org/authed","type":"model"}`

	// Anonymous write: rejected without touching upstream.
	resp, err := http.Post(srv.URL+"/api/repos/create", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous create = %d, want 401", resp.StatusCode)
	}

	// Authenticated write: token validated against upstream whoami.
	post := func() int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/repos/create", strings.NewReader(createBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer hf_valid")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := post(); status != http.StatusOK {
		t.Fatalf("authed create = %d, want 200", status)
	}
	// Second call (409 conflict is fine) must be served from the token
	// cache: no extra whoami round-trip.
	before := hub.Requests("GET", "/api/whoami-v2")
	if status := post(); status != http.StatusConflict {
		t.Fatalf("second create = %d, want 409", status)
	}
	if after := hub.Requests("GET", "/api/whoami-v2"); after != before {
		t.Errorf("token validation not cached (%d -> %d whoami calls)", before, after)
	}
}

func TestWriteOpenWhenAuthNone(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, nil, nil)
	resp, err := http.Post(srv.URL+"/api/repos/create", "application/json",
		strings.NewReader(`{"name":"org/open","type":"model"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create with auth.mode=none = %d, want 200", resp.StatusCode)
	}
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, nil, nil)
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestRootBanner(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, nil, nil)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ = %d, want 200", resp.StatusCode)
	}
	var banner map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&banner); err != nil {
		t.Fatal(err)
	}
	if banner["name"] != "shpiel" {
		t.Errorf("banner = %v", banner)
	}
}

func TestUnknownPathIs404JSON(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, nil, nil)
	resp, err := http.Get(srv.URL + "/api/spaces/whatever")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error == "" {
		t.Errorf("404 body must be HF-style JSON, got err=%v", err)
	}
}
