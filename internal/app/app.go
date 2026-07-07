// Package app assembles a runnable Shpiel from configuration: backends,
// router, upstream client, relay, replication queue, audit stream, and the
// HTTP server. The CLI and the test harnesses share this wiring so tests
// exercise the same object graph production runs.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"

	"github.com/loewenthal-corp/shpiel/internal/audit"
	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/backend/ocibackend"
	"github.com/loewenthal-corp/shpiel/internal/backend/s3backend"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/metrics"
	"github.com/loewenthal-corp/shpiel/internal/relay"
	"github.com/loewenthal-corp/shpiel/internal/replication"
	"github.com/loewenthal-corp/shpiel/internal/s3client"
	"github.com/loewenthal-corp/shpiel/internal/server"
	"github.com/loewenthal-corp/shpiel/internal/upstream"
	"github.com/loewenthal-corp/shpiel/internal/xet"
)

// App is an assembled Shpiel instance.
type App struct {
	Config      config.Config
	Server      *server.Server
	Relay       *relay.Relay
	Metrics     *metrics.Metrics
	Replication *replication.Queue
	Audit       *audit.Logger
	Log         *slog.Logger
}

// Run starts the replication workers (when configured) and serves until
// ctx is canceled.
func (a *App) Run(ctx context.Context) error {
	if a.Replication != nil {
		go a.Replication.Run(ctx)
	}
	defer func() { _ = a.Audit.Close() }()
	return a.Server.Run(ctx)
}

// Build validates cfg and wires up every component. It does not start
// listeners; call App.Server.Run for that.
func Build(cfg config.Config) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}

	log, err := newLogger(cfg.Log)
	if err != nil {
		return nil, err
	}

	backends := map[string]backend.Backend{}
	for name, bc := range cfg.Backends {
		switch bc.Type {
		case "fs":
			b, err := fsbackend.New(name, bc.Path)
			if err != nil {
				return nil, fmt.Errorf("backend %q: %w", name, err)
			}
			backends[name] = b
		case "oci":
			b, err := ocibackend.New(name, ocibackend.Options{
				URL:        bc.URL,
				Format:     bc.Format,
				RepoPrefix: bc.RepoPrefix,
				Username:   os.Getenv(bc.Auth.UsernameEnv),
				Password:   os.Getenv(bc.Auth.PasswordEnv),
			})
			if err != nil {
				return nil, fmt.Errorf("backend %q: %w", name, err)
			}
			backends[name] = b
		case "s3":
			creds := s3Credentials(bc.Auth)
			b, err := s3backend.New(name, s3backend.Options{
				Endpoint:        bc.Endpoint,
				Bucket:          bc.Bucket,
				Region:          bc.Region,
				Prefix:          bc.Prefix,
				AccessKeyID:     creds.AccessKeyID,
				SecretAccessKey: creds.SecretAccessKey,
				SessionToken:    creds.SessionToken,
			})
			if err != nil {
				return nil, fmt.Errorf("backend %q: %w", name, err)
			}
			backends[name] = b
		default:
			// Validate() already rejects these; belt and suspenders.
			return nil, fmt.Errorf("backend %q: unsupported type %q", name, bc.Type)
		}
	}

	router, err := relay.NewRouter(cfg.Routes, backends)
	if err != nil {
		return nil, err
	}

	// The upstream client exists whenever an endpoint is configured: the
	// relay uses it only when pull-through is on, while the server needs
	// it for whoami proxying and passthrough token validation regardless.
	var up *upstream.Client
	if cfg.Upstream.HuggingFace.Endpoint != "" {
		up = upstream.New(cfg.Upstream.HuggingFace.Endpoint, cfg.Upstream.HuggingFace.Token())
	}
	pullThrough := up
	if !cfg.Upstream.HuggingFace.PullThrough {
		pullThrough = nil
	}

	m := metrics.New()

	auditLog, err := audit.Open(cfg.Log.AuditPath)
	if err != nil {
		return nil, err
	}

	// The replication queue exists when any route declares replicas; it is
	// wired into the relay so successful primary writes fan out.
	var repl *replication.Queue
	for _, route := range cfg.Routes {
		if len(route.Replicas) > 0 {
			repl, err = replication.New(replication.Options{
				SpoolDir: cfg.Replication.SpoolDir,
				Backends: backends,
				Workers:  cfg.Replication.Workers,
				Log:      log,
			})
			if err != nil {
				return nil, err
			}
			repl.Instrument(
				func(depth int) { m.ReplicationQueueDepth.Set(float64(depth)) },
				func(target, outcome string) { m.ReplicationJobs.WithLabelValues(target, outcome).Inc() },
			)
			break
		}
	}

	rl := relay.New(relay.Options{
		Router:          router,
		Upstream:        pullThrough,
		RefreshInterval: cfg.Upstream.HuggingFace.RefreshInterval,
		Metrics:         m,
		Log:             log,
		Replicator:      replicatorOrNil(repl),
	})

	var xetSvc *xet.Service
	if cfg.Xet.Enabled {
		store, err := xetStore(cfg)
		if err != nil {
			return nil, err
		}
		xetSvc, err = xet.NewService(store, rl, m, log, auditLog)
		if err != nil {
			return nil, err
		}
	}

	return &App{
		Config: cfg,
		Server: server.New(cfg, rl, m, server.Options{
			Upstream: up,
			Xet:      xetSvc,
			Audit:    auditLog,
			Repl:     repl,
			Log:      log,
		}),
		Relay:       rl,
		Metrics:     m,
		Replication: repl,
		Audit:       auditLog,
		Log:         log,
	}, nil
}

// envOr returns name when set, else the conventional fallback env name.
func envOr(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// s3Credentials resolves static credentials from the configured env
// indirection, falling back to the standard AWS names.
func s3Credentials(auth config.BackendAuth) s3client.Credentials {
	return s3client.Credentials{
		AccessKeyID:     os.Getenv(envOr(auth.AccessKeyIDEnv, "AWS_ACCESS_KEY_ID")),
		SecretAccessKey: os.Getenv(envOr(auth.SecretAccessKeyEnv, "AWS_SECRET_ACCESS_KEY")),
		SessionToken:    os.Getenv(envOr(auth.SessionTokenEnv, "AWS_SESSION_TOKEN")),
	}
}

// xetStore builds the xorb store: a local directory, or — with
// xet.store_backend — the named s3 backend's bucket under <prefix>/xet/,
// so the archive bucket doubles as the xorb store.
func xetStore(cfg config.Config) (*xet.Store, error) {
	if cfg.Xet.StoreBackend == "" {
		return xet.NewStore(cfg.Xet.DataDir)
	}
	bc := cfg.Backends[cfg.Xet.StoreBackend] // Validate() guarantees presence and type
	client, err := s3client.New(s3client.Options{
		Endpoint:    bc.Endpoint,
		Bucket:      bc.Bucket,
		Region:      bc.Region,
		Credentials: s3Credentials(bc.Auth),
	})
	if err != nil {
		return nil, fmt.Errorf("xet.store_backend %q: %w", cfg.Xet.StoreBackend, err)
	}
	return xet.NewBucketStore(client, path.Join(strings.Trim(bc.Prefix, "/"), "xet")), nil
}

// replicatorOrNil avoids handing the relay a typed-nil interface.
func replicatorOrNil(q *replication.Queue) relay.Replicator {
	if q == nil {
		return nil
	}
	return q
}

func newLogger(cfg config.Log) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, fmt.Errorf("invalid log.level %q", cfg.Level)
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch cfg.Format {
	case "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler), nil
}
