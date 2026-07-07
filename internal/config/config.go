// Package config defines Shpiel's configuration: one YAML file whose shape
// is the single mental model shared by the CLI flags, the Helm values, and
// the docs. Precedence is flags > env > config file > defaults.
//
// Secrets are never inlined: fields like token_env name an environment
// variable to read at use time, so config files stay committable.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration.
type Config struct {
	Listen      Listen                   `yaml:"listen"`
	Limits      Limits                   `yaml:"limits"`
	Upstream    Upstream                 `yaml:"upstream"`
	Backends    map[string]BackendConfig `yaml:"backends"`
	Routes      []Route                  `yaml:"routes"`
	Replication Replication              `yaml:"replication"`
	Auth        Auth                     `yaml:"auth"`
	Admin       Admin                    `yaml:"admin"`
	Xet         Xet                      `yaml:"xet"`
	Log         Log                      `yaml:"log"`
}

// Replication configures async fan-out to route replicas.
type Replication struct {
	// SpoolDir persists the retry queue (one JSON file per pending job);
	// required when any route declares replicas.
	SpoolDir string `yaml:"spool_dir,omitempty"`
	// Workers bounds concurrent replication jobs (default 4).
	Workers int `yaml:"workers,omitempty"`
}

// Admin configures the admin API listener (listen.admin).
type Admin struct {
	// TokenEnv names the environment variable holding the bearer token
	// required on every admin request; the admin listener refuses to
	// start without it.
	TokenEnv string `yaml:"token_env,omitempty"`
}

// Token reads the admin token from the configured environment variable.
func (a Admin) Token() string {
	if a.TokenEnv == "" {
		return ""
	}
	return os.Getenv(a.TokenEnv)
}

// Listen configures the server listeners. Admin and metrics are disabled
// unless set (admin additionally requires auth wiring, v1.x).
type Listen struct {
	API     string `yaml:"api"`
	Admin   string `yaml:"admin"`
	Metrics string `yaml:"metrics"`
}

// Limits bounds concurrency and buffering.
type Limits struct {
	MaxConcurrentUploads   int `yaml:"max_concurrent_uploads"`
	MaxConcurrentDownloads int `yaml:"max_concurrent_downloads"`
	PerConnBufferMB        int `yaml:"per_conn_buffer_mb"`
}

// Upstream configures pull-through / push-through targets.
type Upstream struct {
	HuggingFace HuggingFaceUpstream `yaml:"huggingface"`
}

// HuggingFaceUpstream is the huggingface.co (or compatible) upstream.
type HuggingFaceUpstream struct {
	Endpoint string `yaml:"endpoint"`
	// TokenEnv names an environment variable holding the org token used
	// for upstream fetches. Empty means anonymous.
	TokenEnv    string `yaml:"token_env"`
	PullThrough bool   `yaml:"pull_through"`
	// RefreshInterval is how long a cached branch/tag resolution is served
	// before revalidating against the upstream. 0 revalidates every
	// request; commit SHAs are immutable and never revalidated.
	RefreshInterval time.Duration `yaml:"refresh_interval"`
}

// Token reads the upstream token from the configured environment variable.
func (u HuggingFaceUpstream) Token() string {
	if u.TokenEnv == "" {
		return ""
	}
	return os.Getenv(u.TokenEnv)
}

// BackendConfig configures one named backend. Type selects the driver;
// the driver-specific fields are flattened alongside.
type BackendConfig struct {
	Type string `yaml:"type"` // "fs" | "oci" | "s3" | "huggingface"

	// fs
	Path string `yaml:"path,omitempty"`

	// oci
	URL string `yaml:"url,omitempty"`
	// Format is "modelpack" (raw per-file layers, default) or
	// "tar-layers" (image-volume-mountable tars).
	Format string `yaml:"format,omitempty"`
	// LayerPerFile is accepted for forward compatibility; v1 always maps
	// one file to one layer.
	LayerPerFile bool `yaml:"layer_per_file,omitempty"`
	// RepoPrefix prepends a path to every OCI repository name.
	RepoPrefix string      `yaml:"repo_prefix,omitempty"`
	Auth       BackendAuth `yaml:"auth,omitempty"`

	// s3
	Bucket string `yaml:"bucket,omitempty"`
	// Region is the SigV4 signing region; required for AWS, defaults to
	// us-east-1 when a custom endpoint is set.
	Region string `yaml:"region,omitempty"`
	// Endpoint overrides the AWS endpoint for S3-compatible services
	// (MinIO, GCS interop, Ceph, R2); scheme required, path-style
	// addressing.
	Endpoint string `yaml:"endpoint,omitempty"`
	// Prefix is prepended to every object key, for sharing a bucket.
	Prefix string `yaml:"prefix,omitempty"`
}

// BackendAuth holds env-indirect credentials for a backend.
type BackendAuth struct {
	// oci
	UsernameEnv string `yaml:"username_env,omitempty"`
	PasswordEnv string `yaml:"password_env,omitempty"`

	// s3: env vars holding static credentials. Unset fields fall back to
	// the standard AWS names (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY,
	// AWS_SESSION_TOKEN); requests go unsigned when those are empty too.
	AccessKeyIDEnv     string `yaml:"access_key_id_env,omitempty"`
	SecretAccessKeyEnv string `yaml:"secret_access_key_env,omitempty"`
	SessionTokenEnv    string `yaml:"session_token_env,omitempty"`
}

// Route maps repo-id globs to a primary backend and async replicas.
type Route struct {
	Match    string   `yaml:"match"`
	Primary  string   `yaml:"primary"`
	Replicas []string `yaml:"replicas,omitempty"`
}

// Auth configures frontend authentication.
type Auth struct {
	Mode     string        `yaml:"mode"` // "none" | "passthrough" | "local" | "oidc"
	CacheTTL time.Duration `yaml:"cache_ttl"`
}

// Xet configures the Xet protocol surface: the CAS API (xorb/shard ingest,
// chunk-level reconstruction) plus xet token endpoints. Uploaded files are
// always ALSO materialized into the routed backend, so non-xet clients and
// every backend serve them normally.
type Xet struct {
	Enabled bool `yaml:"enabled"`
	// DataDir is the local directory holding xorbs and reconstruction
	// records. Content-addressed and global (cross-repo dedup).
	DataDir string `yaml:"data_dir,omitempty"`
	// StoreBackend names a configured s3-type backend whose bucket also
	// holds the xorb store (keys under <backend prefix>/xet/). Exactly one
	// of data_dir and store_backend is set; a bucket store keeps no local
	// state, so N replicas can share it.
	StoreBackend string `yaml:"store_backend,omitempty"`
}

// Log configures structured logging.
type Log struct {
	Level  string `yaml:"level"`  // "debug" | "info" | "warn" | "error"
	Format string `yaml:"format"` // "json" | "text"
	// AuditPath appends JSON audit records (writes and admin actions:
	// who/what/when/digest) to a file; "-" writes them to stderr; empty
	// disables the audit stream.
	AuditPath string `yaml:"audit_path,omitempty"`
}

// Default returns the baseline configuration that YAML, env, and flags
// override.
func Default() Config {
	return Config{
		Listen: Listen{
			API:     ":8080",
			Metrics: ":9090",
		},
		Limits: Limits{
			MaxConcurrentUploads:   64,
			MaxConcurrentDownloads: 512,
			PerConnBufferMB:        8,
		},
		Upstream: Upstream{
			HuggingFace: HuggingFaceUpstream{
				Endpoint:        "https://huggingface.co",
				PullThrough:     false,
				RefreshInterval: 5 * time.Minute,
			},
		},
		Auth: Auth{
			Mode:     "none",
			CacheTTL: 5 * time.Minute,
		},
		Log: Log{
			Level:  "info",
			Format: "json",
		},
	}
}

// Local returns the zero-config laptop-mode configuration: localhost bind,
// filesystem backend under dataDir, pull-through enabled, Xet uploads on.
func Local(dataDir string) Config {
	cfg := Default()
	cfg.Listen.API = "127.0.0.1:8080"
	cfg.Listen.Metrics = ""
	cfg.Upstream.HuggingFace.PullThrough = true
	cfg.Backends = map[string]BackendConfig{
		"local": {Type: "fs", Path: dataDir},
	}
	cfg.Routes = []Route{{Match: "*", Primary: "local"}}
	cfg.Xet.Enabled = true
	// Dot-prefixed so HF cache tooling scanning dataDir ignores it.
	cfg.Xet.DataDir = filepath.Join(dataDir, ".xet")
	return cfg
}

// DefaultLocalDataDir is laptop-mode's storage root (~/.shpiel).
func DefaultLocalDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".shpiel"
	}
	return filepath.Join(home, ".shpiel")
}

// Load reads path (YAML) over the default configuration.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("config: reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	// A stray "---" mid-file starts a second YAML document that would
	// otherwise be ignored silently — everything after it dropped. Refuse.
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return cfg, fmt.Errorf("config: %s contains multiple YAML documents; remove the stray --- separator", path)
	}
	return cfg, nil
}

// Validate checks the configuration for consistency.
func (c *Config) Validate() error {
	var errs []error

	if c.Listen.API == "" {
		errs = append(errs, errors.New("listen.api is required"))
	}

	switch c.Auth.Mode {
	case "none", "passthrough":
	case "local", "oidc":
		errs = append(errs, fmt.Errorf("auth.mode %q is not implemented yet", c.Auth.Mode))
	default:
		errs = append(errs, fmt.Errorf("auth.mode %q is invalid (want none|passthrough)", c.Auth.Mode))
	}

	if len(c.Backends) == 0 {
		errs = append(errs, errors.New("at least one backend is required (or run with --local)"))
	}
	for name, b := range c.Backends {
		switch b.Type {
		case "fs":
			if b.Path == "" {
				errs = append(errs, fmt.Errorf("backend %q: fs backend requires path", name))
			}
		case "oci":
			if b.URL == "" {
				errs = append(errs, fmt.Errorf("backend %q: oci backend requires url", name))
			}
			switch b.Format {
			case "", "modelpack", "tar-layers":
			default:
				errs = append(errs, fmt.Errorf("backend %q: unknown oci format %q (want modelpack|tar-layers)", name, b.Format))
			}
		case "s3":
			if b.Bucket == "" {
				errs = append(errs, fmt.Errorf("backend %q: s3 backend requires bucket", name))
			}
			if b.Region == "" && b.Endpoint == "" {
				errs = append(errs, fmt.Errorf("backend %q: s3 backend requires region (or endpoint for S3-compatible services)", name))
			}
		case "huggingface":
			errs = append(errs, fmt.Errorf("backend %q: type %q is not implemented yet", name, b.Type))
		default:
			errs = append(errs, fmt.Errorf("backend %q: unknown type %q", name, b.Type))
		}
	}

	if len(c.Routes) == 0 && len(c.Backends) > 0 {
		errs = append(errs, errors.New("at least one route is required, e.g. {match: \"*\", primary: <backend>}"))
	}
	hasReplicas := false
	for i, r := range c.Routes {
		if r.Match == "" {
			errs = append(errs, fmt.Errorf("routes[%d]: match is required", i))
		}
		if _, ok := c.Backends[r.Primary]; !ok {
			errs = append(errs, fmt.Errorf("routes[%d]: primary %q is not a configured backend", i, r.Primary))
		}
		for _, rep := range r.Replicas {
			hasReplicas = true
			if _, ok := c.Backends[rep]; !ok {
				errs = append(errs, fmt.Errorf("routes[%d]: replica %q is not a configured backend", i, rep))
			}
		}
	}
	if hasReplicas && c.Replication.SpoolDir == "" {
		errs = append(errs, errors.New("routes declare replicas: replication.spool_dir is required"))
	}
	if c.Listen.Admin != "" && c.Admin.TokenEnv == "" {
		errs = append(errs, errors.New("listen.admin requires admin.token_env (the admin API refuses to run unauthenticated)"))
	}

	if c.Xet.Enabled {
		switch {
		case c.Xet.DataDir == "" && c.Xet.StoreBackend == "":
			errs = append(errs, errors.New("xet.enabled requires xet.data_dir or xet.store_backend"))
		case c.Xet.DataDir != "" && c.Xet.StoreBackend != "":
			errs = append(errs, errors.New("xet.data_dir and xet.store_backend are mutually exclusive"))
		}
	}
	if c.Xet.StoreBackend != "" {
		if b, ok := c.Backends[c.Xet.StoreBackend]; !ok {
			errs = append(errs, fmt.Errorf("xet.store_backend %q is not a configured backend", c.Xet.StoreBackend))
		} else if b.Type != "s3" {
			errs = append(errs, fmt.Errorf("xet.store_backend %q must be an s3 backend (is %q)", c.Xet.StoreBackend, b.Type))
		}
	}

	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format %q is invalid (want json|text)", c.Log.Format))
	}

	return errors.Join(errs...)
}
