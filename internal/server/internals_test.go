package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/loewenthal-corp/shpiel/internal/audit"
	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/metrics"
	"github.com/loewenthal-corp/shpiel/internal/relay"
	"github.com/loewenthal-corp/shpiel/internal/upstream"
	"github.com/loewenthal-corp/shpiel/internal/xet"
)

// TestInstrumentRecordsRealStatus: the metrics label must carry the status
// the handler wrote — including error statuses with JSON bodies — and
// default to 200 only for handlers that never write one.
func TestInstrumentRecordsRealStatus(t *testing.T) {
	t.Parallel()
	h := newServerHarness(t, nil, Options{})

	resp, _ := h.do(t, http.MethodGet, "/api/models/org/ghost", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	if got := testutil.ToFloat64(h.metrics.HTTPRequests.WithLabelValues("model_info", "GET", "404")); got != 1 {
		t.Errorf("404 counter = %v, want 1", got)
	}

	resp, _ = h.do(t, http.MethodGet, "/", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("root: %d", resp.StatusCode)
	}
	if got := testutil.ToFloat64(h.metrics.HTTPRequests.WithLabelValues("root", "GET", "200")); got != 1 {
		t.Errorf("200 counter = %v, want 1", got)
	}
}

func TestTokenValidatorCache(t *testing.T) {
	t.Parallel()
	if v := newTokenValidator(0); v.ttl != 5*time.Minute {
		t.Errorf("default ttl = %v, want 5m", v.ttl)
	}
	if v := newTokenValidator(30 * time.Second); v.ttl != 30*time.Second {
		t.Errorf("explicit ttl = %v", v.ttl)
	}

	v := newTokenValidator(time.Minute)
	v.put("tok-a", true, "alice")
	v.put("tok-b", false, "")
	if verdict, found := v.get("tok-a"); !found || !verdict.ok || verdict.name != "alice" {
		t.Fatalf("tok-a verdict = %+v found=%v", verdict, found)
	}
	if verdict, found := v.get("tok-b"); !found || verdict.ok {
		t.Fatalf("tok-b verdict = %+v found=%v", verdict, found)
	}

	// Expired entries evict on read.
	fast := newTokenValidator(time.Nanosecond)
	fast.put("gone", true, "x")
	time.Sleep(time.Millisecond)
	if _, found := fast.get("gone"); found {
		t.Fatal("expired verdict served")
	}

	// The cache holds 10_001 entries and wipes only past the cap.
	big := newTokenValidator(time.Hour)
	for i := range 10_001 {
		big.put(fmt.Sprintf("t%d", i), true, "")
	}
	if n := len(big.entries); n != 10_001 {
		t.Fatalf("entries after 10001 puts = %d, want 10001", n)
	}
	big.put("overflow", true, "")
	if n := len(big.entries); n != 1 {
		t.Fatalf("entries after overflow = %d, want 1 (wiped)", n)
	}
}

// TestConcurrencyLimits: limits build bounded semaphores, zero means
// unlimited (nil), and acquisition respects context cancellation.
func TestConcurrencyLimits(t *testing.T) {
	t.Parallel()
	h := newServerHarness(t, func(c *config.Config) {
		c.Limits.MaxConcurrentDownloads = 2
		c.Limits.MaxConcurrentUploads = 3
	}, Options{})
	if cap(h.server.downloadSem) != 2 || cap(h.server.uploadSem) != 3 {
		t.Fatalf("sem caps = %d, %d", cap(h.server.downloadSem), cap(h.server.uploadSem))
	}

	unlimited := newServerHarness(t, func(c *config.Config) {
		c.Limits.MaxConcurrentDownloads = 0
		c.Limits.MaxConcurrentUploads = 0
	}, Options{})
	if unlimited.server.downloadSem != nil || unlimited.server.uploadSem != nil {
		t.Fatal("zero limits built semaphores")
	}

	// A full semaphore blocks until the context dies.
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := acquireSlot(ctx, sem); err == nil {
		t.Fatal("acquire on full semaphore succeeded")
	}
	releaseSlot(sem)
	if err := acquireSlot(context.Background(), sem); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	// nil semaphores are free.
	if err := acquireSlot(ctx, nil); err != nil {
		t.Fatalf("nil sem acquire: %v", err)
	}
	releaseSlot(nil)
}

// TestXetRoutesOnlyWhenEnabled: the CAS namespace exists exactly when a
// xet service is wired.
func TestXetRoutesOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	store, err := xet.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := xet.NewService(store, nil, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	withXet := newServerHarness(t, nil, Options{Xet: svc})
	resp, _ := withXet.do(t, http.MethodPost, "/xet/v1/shards", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("xet shards with service = %d, want 401", resp.StatusCode)
	}
	// The xet token endpoint mints connection info.
	resp, _ = withXet.do(t, http.MethodGet, "/api/models/org/name/xet-read-token/main", nil, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get(xet.HeaderAccessToken) == "" {
		t.Fatalf("xet read token = %d, token %q", resp.StatusCode, resp.Header.Get(xet.HeaderAccessToken))
	}

	without := newServerHarness(t, nil, Options{})
	resp, _ = without.do(t, http.MethodPost, "/xet/v1/shards", nil, "")
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("xet shards without service answered as CAS: %d", resp.StatusCode)
	}
}

// slowPingBackend takes a beat to answer Ping, so aggressive admin
// timeouts show up as false unhealthiness.
type slowPingBackend struct {
	backend.Backend
}

func (s *slowPingBackend) Ping(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

// TestAdminBackendsAllowsSlowPing: the health probe gives backends a real
// timeout budget; a 50ms ping is healthy, not deadline-exceeded.
func TestAdminBackendsAllowsSlowPing(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Backends = map[string]config.BackendConfig{"fs": {Type: "fs", Path: t.TempDir()}}
	cfg.Routes = []config.Route{{Match: "*", Primary: "fs"}}
	inner, err := fsbackend.New("slow", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	slow := &slowPingBackend{Backend: inner}
	router, err := relay.NewRouter(cfg.Routes, map[string]backend.Backend{"fs": slow})
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	s := New(cfg, relay.New(relay.Options{Router: router, Metrics: m}), m, Options{})
	srv := httptest.NewServer(s.adminHandler("secret"))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/v1/backends", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out struct {
		Backends []struct {
			Name    string `json:"name"`
			Healthy bool   `json:"healthy"`
			Error   string `json:"error"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Backends) != 1 || !out.Backends[0].Healthy {
		t.Fatalf("backends = %s", data)
	}
	if strings.Contains(out.Backends[0].Error, "deadline") {
		t.Fatalf("slow ping hit the timeout: %s", data)
	}
}

// TestRunLifecycle: Run binds the API (and metrics and admin listeners
// when configured), serves, and drains cleanly on cancellation. An admin
// listener without a resolvable token refuses to start.
func TestRunLifecycle(t *testing.T) {
	t.Setenv("SHPIEL_TEST_ADMIN_TOKEN", "sekrit") // Setenv forbids t.Parallel
	h := newServerHarness(t, func(c *config.Config) {
		c.Listen.API = "127.0.0.1:0"
		c.Listen.Metrics = "127.0.0.1:0"
		c.Listen.Admin = "127.0.0.1:0"
		c.Admin.TokenEnv = "SHPIEL_TEST_ADMIN_TOKEN"
	}, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.server.Run(ctx) }()

	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for addr == "" && time.Now().Before(deadline) {
		addr = h.server.APIAddr()
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("API listener never bound")
	}
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz over Run = %d", resp.StatusCode)
	}

	// The metrics listener answers on its own port.
	if h.server.MetricsAddr() == "" {
		t.Fatal("metrics listener never bound")
	}
	resp, err = http.Get("http://" + h.server.MetricsAddr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics over Run = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not drain")
	}

	// An admin listener without a token is a startup error, not a silent
	// unauthenticated API.
	noToken := newServerHarness(t, func(c *config.Config) {
		c.Listen.API = "127.0.0.1:0"
		c.Listen.Metrics = ""
		c.Listen.Admin = "127.0.0.1:0"
		c.Admin.TokenEnv = "SHPIEL_TEST_ABSENT_TOKEN"
	}, Options{})
	if err := noToken.server.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "admin token") {
		t.Fatalf("Run without admin token = %v, want startup error", err)
	}
}

// TestAuditTrail: state-changing endpoints append audit records with the
// right actor and per-action detail counts.
func TestAuditTrail(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	auditLog, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h := newServerHarness(t, nil, Options{Audit: auditLog})

	if resp, _ := h.do(t, http.MethodPost, "/api/repos/create", nil, `{"name":"org/audited","type":"model"}`); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	// Asymmetric counts (2 inline + 1 LFS, 2 files + 1 folder deleted) so
	// the audit totals cannot be reproduced by any other combination.
	weights := []byte("weights bytes")
	oid := fakehub.SHA256Hex(weights)
	req, _ := http.NewRequest(http.MethodPut, h.http.URL+"/shpiel-lfs/models/org/audited/"+oid+fmt.Sprintf("?size=%d", len(weights)), strings.NewReader(string(weights)))
	upResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		t.Fatalf("lfs upload = %d", upResp.StatusCode)
	}
	commit := ndjson(t,
		`{"key":"header","value":{"summary":"s"}}`,
		`{"key":"file","value":{"path":"a.txt","content":"`+base64.StdEncoding.EncodeToString([]byte("a"))+`"}}`,
		`{"key":"file","value":{"path":"dir/b.txt","content":"`+base64.StdEncoding.EncodeToString([]byte("b"))+`"}}`,
		fmt.Sprintf(`{"key":"lfsFile","value":{"path":"w.bin","oid":"%s","size":%d}}`, oid, len(weights)),
	)
	if resp, _ := h.do(t, http.MethodPost, "/api/models/org/audited/commit/main", nil, commit); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	del := ndjson(t,
		`{"key":"header","value":{"summary":"d"}}`,
		`{"key":"deletedFile","value":{"path":"a.txt"}}`,
		`{"key":"deletedFile","value":{"path":"w.bin"}}`,
		`{"key":"deletedFolder","value":{"path":"dir"}}`,
	)
	if resp, _ := h.do(t, http.MethodPost, "/api/models/org/audited/commit/main", nil, del); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if err := auditLog.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		records = append(records, rec)
	}
	if len(records) != 4 {
		t.Fatalf("audit records = %d: %s", len(records), data)
	}
	if records[0]["action"] != "repo_create" || records[0]["actor"] != "anonymous" || records[0]["repo"] != "org/audited" {
		t.Fatalf("create record = %v", records[0])
	}
	if records[1]["action"] != "lfs_upload" {
		t.Fatalf("upload record = %v", records[1])
	}
	if records[2]["action"] != "commit" || records[2]["files"] != float64(3) || records[2]["deleted"] != float64(0) {
		t.Fatalf("commit record = %v", records[2])
	}
	if records[3]["files"] != float64(0) || records[3]["deleted"] != float64(3) {
		t.Fatalf("delete-commit record = %v", records[3])
	}
}

// TestXetTokenAuthScopes: with passthrough auth, write tokens demand an
// upstream-vouched identity and read tokens validate the caller too.
func TestXetTokenAuthScopes(t *testing.T) {
	t.Parallel()
	hub := fakehub.New()
	hubSrv := httptest.NewServer(hub.Handler())
	t.Cleanup(hubSrv.Close)
	up := upstream.New(hubSrv.URL, "")

	store, err := xet.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := xet.NewService(store, nil, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newServerHarness(t, func(c *config.Config) { c.Auth.Mode = "passthrough" }, Options{Xet: svc, Upstream: up})

	for _, tc := range []struct {
		scope, token string
		want         int
	}{
		{"write", "", http.StatusUnauthorized},
		{"write", "hf_testtoken", http.StatusOK},
		{"read", "", http.StatusUnauthorized},
		{"read", "hf_testtoken", http.StatusOK},
	} {
		headers := map[string]string{}
		if tc.token != "" {
			headers["Authorization"] = "Bearer " + tc.token
		}
		resp, body := h.do(t, http.MethodGet, "/api/models/org/x/xet-"+tc.scope+"-token/main", headers, "")
		if resp.StatusCode != tc.want {
			t.Errorf("%s token with token=%q = %d, want %d (%s)", tc.scope, tc.token, resp.StatusCode, tc.want, body)
		}
		if tc.want == http.StatusOK && resp.Header.Get(xet.HeaderAccessToken) == "" {
			t.Errorf("%s token: no CAS token issued", tc.scope)
		}
	}

	// validateToken carries the upstream identity for audit records.
	ok, name, err := h.server.validateToken(context.Background(), "hf_testtoken")
	if err != nil || !ok || name != "fakeuser" {
		t.Fatalf("validateToken = %v, %q, %v; want vouched fakeuser", ok, name, err)
	}
	// Without an upstream, passthrough fails closed.
	noUp := newServerHarness(t, func(c *config.Config) { c.Auth.Mode = "passthrough" }, Options{})
	ok, _, err = noUp.server.validateToken(context.Background(), "hf_testtoken")
	if err != nil || ok {
		t.Fatalf("validateToken without upstream = %v, %v; want false", ok, err)
	}
}

// TestRepoKindFromType pins the payload dialects.
func TestRepoKindFromType(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]hfapi.RepoKind{"": hfapi.RepoKindModel, "model": hfapi.RepoKindModel, "dataset": hfapi.RepoKindDataset} {
		got, ok := repoKindFromType(in)
		if !ok || got != want {
			t.Errorf("repoKindFromType(%q) = %v, %v", in, got, ok)
		}
	}
	if _, ok := repoKindFromType("space"); ok {
		t.Error("space accepted")
	}
}
