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
	"github.com/loewenthal-corp/shpiel/internal/metrics"
	"github.com/loewenthal-corp/shpiel/internal/relay"
)

func newAdminServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Backends = map[string]config.BackendConfig{"fs": {Type: "fs", Path: t.TempDir()}}
	cfg.Routes = []config.Route{{Match: "*", Primary: "fs"}}
	bk, err := fsbackend.New("fs", cfg.Backends["fs"].Path)
	if err != nil {
		t.Fatal(err)
	}
	router, err := relay.NewRouter(cfg.Routes, map[string]backend.Backend{"fs": bk})
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	rl := relay.New(relay.Options{Router: router, Metrics: m})
	s := New(cfg, rl, m, Options{})
	srv := httptest.NewServer(s.adminHandler("sekrit"))
	t.Cleanup(srv.Close)
	return srv
}

func adminGet(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestAdminRequiresToken(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	for _, token := range []string{"", "wrong"} {
		status, _ := adminGet(t, srv.URL+"/admin/v1/backends", token)
		if status != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", token, status)
		}
	}
}

func TestAdminBackends(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	status, body := adminGet(t, srv.URL+"/admin/v1/backends", "sekrit")
	if status != http.StatusOK {
		t.Fatalf("status = %d; body: %s", status, body)
	}
	var resp struct {
		Backends []struct {
			Name    string `json:"name"`
			Healthy bool   `json:"healthy"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Backends) != 1 || resp.Backends[0].Name != "fs" || !resp.Backends[0].Healthy {
		t.Fatalf("backends = %+v", resp.Backends)
	}
}

func TestAdminReplicationWithoutQueue(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	status, body := adminGet(t, srv.URL+"/admin/v1/replication", "sekrit")
	if status != http.StatusOK || !strings.Contains(string(body), `"enabled":false`) {
		t.Fatalf("status = %d, body = %s", status, body)
	}
}

func TestAdminConfigRendered(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	status, body := adminGet(t, srv.URL+"/admin/v1/config", "sekrit")
	if status != http.StatusOK || !strings.Contains(string(body), "listen:") {
		t.Fatalf("status = %d, body starts: %.80s", status, body)
	}
}
