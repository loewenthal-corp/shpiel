package config

import (
	"strings"
	"testing"
	"time"
)

// TestDefaults pins the shipped default values; deployments rely on these
// exact numbers when fields are omitted.
func TestDefaults(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Upstream.HuggingFace.RefreshInterval != 5*time.Minute {
		t.Errorf("default refresh_interval = %v, want 5m", cfg.Upstream.HuggingFace.RefreshInterval)
	}
	if cfg.Auth.CacheTTL != 5*time.Minute {
		t.Errorf("default auth cache_ttl = %v, want 5m", cfg.Auth.CacheTTL)
	}
	// A backend-less config complains about backends — but not about
	// routes, which are only demanded once a backend exists.
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "backend is required") {
		t.Errorf("Default().Validate() = %v, want backend-required error", err)
	}
	if err != nil && strings.Contains(err.Error(), "route is required") {
		t.Errorf("route demanded without any backend: %v", err)
	}
}

func validBase() Config {
	cfg := Default()
	cfg.Backends = map[string]BackendConfig{
		"cache":  {Type: "fs", Path: "/var/lib/shpiel"},
		"mirror": {Type: "fs", Path: "/var/lib/mirror"},
	}
	cfg.Routes = []Route{{Match: "*", Primary: "cache"}}
	return cfg
}

// TestValidateRouteAndReplicaRules covers the coupling rules: backends
// demand routes, replicas demand a spool dir, admin listeners demand a
// token env.
func TestValidateRouteAndReplicaRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // "" means valid
	}{
		{
			name:   "valid base",
			mutate: func(c *Config) {},
		},
		{
			name:    "backends without routes",
			mutate:  func(c *Config) { c.Routes = nil },
			wantErr: "at least one route",
		},
		{
			name: "replicas without spool dir",
			mutate: func(c *Config) {
				c.Routes[0].Replicas = []string{"mirror"}
			},
			wantErr: "spool_dir",
		},
		{
			name: "replicas with spool dir",
			mutate: func(c *Config) {
				c.Routes[0].Replicas = []string{"mirror"}
				c.Replication.SpoolDir = "/var/spool/shpiel"
			},
		},
		{
			name: "unknown replica backend",
			mutate: func(c *Config) {
				c.Routes[0].Replicas = []string{"ghost"}
				c.Replication.SpoolDir = "/var/spool/shpiel"
			},
			wantErr: "not a configured backend",
		},
		{
			name:    "admin listener without token env",
			mutate:  func(c *Config) { c.Listen.Admin = ":9099" },
			wantErr: "admin.token_env",
		},
		{
			name: "admin listener with token env",
			mutate: func(c *Config) {
				c.Listen.Admin = ":9099"
				c.Admin.TokenEnv = "SHPIEL_ADMIN_TOKEN"
			},
		},
		{
			name:    "xet without data dir",
			mutate:  func(c *Config) { c.Xet.Enabled = true },
			wantErr: "xet.data_dir",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validBase()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
