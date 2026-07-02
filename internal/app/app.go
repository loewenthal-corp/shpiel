// Package app assembles a runnable Shpiel from configuration: backends,
// router, upstream client, relay, metrics, and the HTTP server. The CLI and
// the test harnesses share this wiring so tests exercise the same object
// graph production runs.
package app

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/metrics"
	"github.com/loewenthal-corp/shpiel/internal/relay"
	"github.com/loewenthal-corp/shpiel/internal/server"
	"github.com/loewenthal-corp/shpiel/internal/upstream"
)

// App is an assembled Shpiel instance.
type App struct {
	Config  config.Config
	Server  *server.Server
	Relay   *relay.Relay
	Metrics *metrics.Metrics
	Log     *slog.Logger
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
		default:
			// Validate() already rejects these; belt and suspenders.
			return nil, fmt.Errorf("backend %q: unsupported type %q", name, bc.Type)
		}
	}

	router, err := relay.NewRouter(cfg.Routes, backends)
	if err != nil {
		return nil, err
	}

	var up *upstream.Client
	if cfg.Upstream.HuggingFace.PullThrough {
		up = upstream.New(cfg.Upstream.HuggingFace.Endpoint, cfg.Upstream.HuggingFace.Token())
	}

	m := metrics.New()
	rl := relay.New(relay.Options{
		Router:          router,
		Upstream:        up,
		RefreshInterval: cfg.Upstream.HuggingFace.RefreshInterval,
		Metrics:         m,
		Log:             log,
	})

	return &App{
		Config:  cfg,
		Server:  server.New(cfg, rl, m, log),
		Relay:   rl,
		Metrics: m,
		Log:     log,
	}, nil
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
