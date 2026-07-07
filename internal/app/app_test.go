package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/s3client"
)

func validConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Listen.API = "127.0.0.1:0"
	cfg.Listen.Metrics = "" // no second listener in tests
	cfg.Backends = map[string]config.BackendConfig{
		"cache":  {Type: "fs", Path: t.TempDir()},
		"mirror": {Type: "fs", Path: t.TempDir()},
	}
	cfg.Routes = []config.Route{{Match: "*", Primary: "cache"}}
	return cfg
}

func TestBuildWiresComponents(t *testing.T) {
	t.Parallel()
	app, err := Build(validConfig(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if app.Server == nil || app.Relay == nil || app.Metrics == nil || app.Log == nil {
		t.Fatalf("app = %+v", app)
	}
	if app.Replication != nil {
		t.Fatal("replication queue built without replicas")
	}
	if app.Audit != nil {
		t.Fatal("audit logger built without audit path")
	}
}

// TestBuildWiresS3Backend proves the s3 driver participates in production
// wiring: credentials resolve through the configured env indirection and
// the backend lands in the router.
func TestBuildWiresS3Backend(t *testing.T) {
	t.Setenv("TEST_S3_ACCESS", "AKIDAPP")
	t.Setenv("TEST_S3_SECRET", "app-secret")
	cfg := validConfig(t)
	cfg.Backends["archive"] = config.BackendConfig{
		Type:     "s3",
		Bucket:   "models",
		Endpoint: "http://127.0.0.1:1",
		Auth: config.BackendAuth{
			AccessKeyIDEnv:     "TEST_S3_ACCESS",
			SecretAccessKeyEnv: "TEST_S3_SECRET",
		},
	}
	cfg.Routes = []config.Route{{Match: "*", Primary: "archive"}}
	app, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	backends := app.Relay.Backends()
	if len(backends) != 1 || backends[0].Name() != "archive" {
		t.Fatalf("routed backends = %v", backends)
	}
}

// TestS3CredentialsProviderChain pins the resolution order: explicit
// static keys, then ambient web identity (IRSA), then anonymous.
func TestS3CredentialsProviderChain(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME", "AWS_ENDPOINT_URL_STS",
	} {
		t.Setenv(env, "")
	}

	// Nothing set: anonymous.
	p, err := s3CredentialsProvider(config.BackendAuth{}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if creds, _ := p.Credentials(context.Background()); !creds.IsZero() {
		t.Errorf("empty environment yielded credentials %+v", creds)
	}

	// Web identity env vars present: the IRSA provider.
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123:role/shpiel")
	p, err = s3CredentialsProvider(config.BackendAuth{}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*s3client.WebIdentityProvider); !ok {
		t.Errorf("web identity env yielded %T, want *s3client.WebIdentityProvider", p)
	}

	// Static keys win over web identity.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDSTATIC")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sekrit")
	p, err = s3CredentialsProvider(config.BackendAuth{}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	creds, _ := p.Credentials(context.Background())
	if creds.AccessKeyID != "AKIDSTATIC" {
		t.Errorf("static keys did not win: %+v", creds)
	}

	// Configured env names take precedence over the AWS defaults.
	t.Setenv("CUSTOM_KEY", "AKIDCUSTOM")
	t.Setenv("CUSTOM_SECRET", "custom-secret")
	p, err = s3CredentialsProvider(config.BackendAuth{AccessKeyIDEnv: "CUSTOM_KEY", SecretAccessKeyEnv: "CUSTOM_SECRET"}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	creds, _ = p.Credentials(context.Background())
	if creds.AccessKeyID != "AKIDCUSTOM" {
		t.Errorf("configured env names did not win: %+v", creds)
	}
}

// TestBuildWiresXetBucketStore proves xet.store_backend flows through
// production wiring: the named s3 backend's bucket becomes the xorb store.
func TestBuildWiresXetBucketStore(t *testing.T) {
	t.Parallel()
	cfg := validConfig(t)
	cfg.Backends["archive"] = config.BackendConfig{
		Type: "s3", Bucket: "models", Endpoint: "http://127.0.0.1:1", Prefix: "shpiel",
	}
	cfg.Xet = config.Xet{Enabled: true, StoreBackend: "archive"}
	if _, err := Build(cfg); err != nil {
		t.Fatalf("Build with xet.store_backend: %v", err)
	}

	store, err := xetStore(cfg)
	if err != nil || store == nil {
		t.Fatalf("xetStore = %v, %v", store, err)
	}
	// Local data_dir still works, and a broken endpoint surfaces.
	if _, err := xetStore(config.Config{Xet: config.Xet{DataDir: t.TempDir()}}); err != nil {
		t.Errorf("xetStore(data_dir) = %v", err)
	}
	bad := cfg
	bad.Backends = map[string]config.BackendConfig{"archive": {Type: "s3", Bucket: "models", Endpoint: "://bad"}}
	if _, err := xetStore(bad); err == nil {
		t.Error("xetStore accepted a broken endpoint")
	}
}

func TestBuildReplicationAndAudit(t *testing.T) {
	t.Parallel()
	cfg := validConfig(t)
	cfg.Routes[0].Replicas = []string{"mirror"}
	cfg.Replication.SpoolDir = t.TempDir()
	cfg.Log.AuditPath = "-"
	cfg.Xet.Enabled = true
	cfg.Xet.DataDir = t.TempDir()

	app, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if app.Replication == nil {
		t.Fatal("no replication queue despite replicas")
	}
	if app.Audit == nil {
		t.Fatal("no audit logger despite audit path")
	}

	// The queue is actually wired into the relay: a write enqueues a
	// replication job (nobody consumes it here — Run was never called).
	repo, err := hfapi.ParseRepoID("org/wired")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Relay.CreateRepo(context.Background(), hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	if depth := app.Replication.Depth(); depth != 1 {
		t.Fatalf("replication depth after create = %d, want 1", depth)
	}
}

// TestBuildWiresUpstream: with an endpoint configured, passthrough whoami
// proxies to it instead of answering with the synthetic local identity.
func TestBuildWiresUpstream(t *testing.T) {
	t.Parallel()
	hub := fakehub.New()
	hubSrv := httptest.NewServer(hub.Handler())
	t.Cleanup(hubSrv.Close)

	cfg := validConfig(t)
	cfg.Auth.Mode = "passthrough"
	cfg.Upstream.HuggingFace.Endpoint = hubSrv.URL

	app, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(ctx) }()

	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for addr == "" && time.Now().Before(deadline) {
		addr = app.Server.APIAddr()
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("server never bound")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/whoami-v2", nil)
	req.Header.Set("Authorization", "Bearer hf_testtoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "fakeuser") {
		t.Fatalf("whoami = %d %s, want proxied fakeuser", resp.StatusCode, body)
	}
}

func TestBuildRejectsBadConfigs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"invalid config", func(c *config.Config) { c.Backends = nil; c.Routes = nil }},
		{"bad log level", func(c *config.Config) { c.Log.Level = "loud" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig(t)
			tc.mutate(&cfg)
			if _, err := Build(cfg); err == nil {
				t.Fatal("Build accepted a broken config")
			}
		})
	}
}

// TestRunServesUntilCanceled: the assembled app listens, answers, and
// shuts down cleanly on context cancellation.
func TestRunServesUntilCanceled(t *testing.T) {
	t.Parallel()
	app, err := Build(validConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for addr == "" && time.Now().Before(deadline) {
		addr = app.Server.APIAddr()
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("server never bound")
	}
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit on cancel")
	}
}
