package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const exampleYAML = `---
listen:
  api: ":8080"
  metrics: ":9090"

limits:
  max_concurrent_uploads: 32
  max_concurrent_downloads: 256

upstream:
  huggingface:
    endpoint: https://huggingface.co
    token_env: HF_ORG_TOKEN
    pull_through: true
    refresh_interval: 10m

backends:
  cache:
    type: fs
    path: /var/lib/shpiel

routes:
  - match: "exigence/*"
    primary: cache
  - match: "*"
    primary: cache

auth:
  mode: passthrough
  cache_ttl: 5m
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAndValidate(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeTemp(t, exampleYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.Limits.MaxConcurrentUploads != 32 {
		t.Errorf("max_concurrent_uploads = %d, want 32", cfg.Limits.MaxConcurrentUploads)
	}
	// Unset fields keep defaults.
	if cfg.Limits.PerConnBufferMB != 8 {
		t.Errorf("per_conn_buffer_mb default = %d, want 8", cfg.Limits.PerConnBufferMB)
	}
	if cfg.Upstream.HuggingFace.RefreshInterval != 10*time.Minute {
		t.Errorf("refresh_interval = %v, want 10m", cfg.Upstream.HuggingFace.RefreshInterval)
	}
	if !cfg.Upstream.HuggingFace.PullThrough {
		t.Error("pull_through = false, want true")
	}
	if len(cfg.Routes) != 2 || cfg.Routes[0].Match != "exigence/*" {
		t.Errorf("routes = %+v", cfg.Routes)
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	t.Parallel()
	_, err := Load(writeTemp(t, "listen:\n  api: ':1'\ntypo_key: true\n"))
	if err == nil || !strings.Contains(err.Error(), "typo_key") {
		t.Fatalf("unknown key must fail loudly, got %v", err)
	}
}

func TestMultiDocumentRejected(t *testing.T) {
	t.Parallel()
	// A stray --- would silently discard everything after it.
	_, err := Load(writeTemp(t, "listen:\n  api: ':1'\n---\nupstream:\n  huggingface:\n    pull_through: true\n"))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("multi-document config must fail loudly, got %v", err)
	}
}

func TestValidateCatchesMistakes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"no backends", func(c *Config) { c.Backends = nil }, "at least one backend"},
		{"bad backend type", func(c *Config) { c.Backends = map[string]BackendConfig{"x": {Type: "floppy"}} }, "unknown type"},
		{"fs without path", func(c *Config) { c.Backends = map[string]BackendConfig{"x": {Type: "fs"}} }, "requires path"},
		{"route to unknown backend", func(c *Config) { c.Routes = []Route{{Match: "*", Primary: "ghost"}} }, "not a configured backend"},
		{"bad auth mode", func(c *Config) { c.Auth.Mode = "vibes" }, "auth.mode"},
		{"xet without data dir", func(c *Config) { c.Xet.Enabled = true; c.Xet.DataDir = "" }, "xet.data_dir"},
		{"s3 without bucket", func(c *Config) { c.Backends = map[string]BackendConfig{"x": {Type: "s3", Region: "us-east-1"}} }, "requires bucket"},
		{"s3 without region or endpoint", func(c *Config) { c.Backends = map[string]BackendConfig{"x": {Type: "s3", Bucket: "b"}} }, "requires region"},
		{"huggingface unimplemented", func(c *Config) { c.Backends = map[string]BackendConfig{"x": {Type: "huggingface"}} }, "not implemented"},
		{"xet without any store", func(c *Config) { c.Xet.Enabled = true; c.Xet.DataDir = ""; c.Xet.StoreBackend = "" }, "xet.data_dir or xet.store_backend"},
		{"xet with both stores", func(c *Config) { c.Xet.Enabled = true; c.Xet.StoreBackend = "local" }, "mutually exclusive"},
		{"xet store backend unknown", func(c *Config) { c.Xet.DataDir = ""; c.Xet.StoreBackend = "ghost" }, "not a configured backend"},
		{"xet store backend not s3", func(c *Config) { c.Xet.DataDir = ""; c.Xet.StoreBackend = "local" }, "must be an s3 backend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Local(t.TempDir())
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate = %v, want error containing %q", err, tc.wantSub)
			}
		})
	}
}

// TestValidS3Backends pins the two legal s3 shapes: AWS (region, no
// endpoint) and S3-compatible (endpoint, region optional).
func TestValidS3Backends(t *testing.T) {
	t.Parallel()
	for name, b := range map[string]BackendConfig{
		"aws":    {Type: "s3", Bucket: "models", Region: "eu-west-2"},
		"compat": {Type: "s3", Bucket: "models", Endpoint: "http://minio:9000", Prefix: "shpiel"},
	} {
		cfg := Default()
		cfg.Backends = map[string]BackendConfig{"archive": b}
		cfg.Routes = []Route{{Match: "*", Primary: "archive"}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s s3 config rejected: %v", name, err)
		}
		// The same s3 backend can double as the xet xorb store.
		cfg.Xet = Xet{Enabled: true, StoreBackend: "archive"}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s with xet.store_backend rejected: %v", name, err)
		}
	}
}

func TestLocalMode(t *testing.T) {
	t.Parallel()
	cfg := Local("/tmp/data")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("local config invalid: %v", err)
	}
	if !strings.HasPrefix(cfg.Listen.API, "127.0.0.1") {
		t.Errorf("local mode must bind localhost, got %s", cfg.Listen.API)
	}
	if !cfg.Upstream.HuggingFace.PullThrough {
		t.Error("local mode must enable pull-through")
	}
}
