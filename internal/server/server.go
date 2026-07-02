// Package server exposes the Hugging Face Hub API surface over the relay.
// Routing follows the Hub's URL shapes exactly, including repos with and
// without an owner namespace and multi-segment filenames.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/metrics"
	"github.com/loewenthal-corp/shpiel/internal/relay"
)

// Server hosts the HF API listener and the optional metrics listener.
type Server struct {
	cfg     config.Config
	relay   *relay.Relay
	metrics *metrics.Metrics
	log     *slog.Logger

	// downloadSem bounds concurrent content downloads
	// (limits.max_concurrent_downloads); nil means unlimited.
	downloadSem chan struct{}

	apiListener     net.Listener
	metricsListener net.Listener
}

// New assembles a Server.
func New(cfg config.Config, rl *relay.Relay, m *metrics.Metrics, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, relay: rl, metrics: m, log: log}
	if n := cfg.Limits.MaxConcurrentDownloads; n > 0 {
		s.downloadSem = make(chan struct{}, n)
	}
	return s
}

// acquireDownloadSlot blocks until a download slot frees up or the request
// is canceled.
func (s *Server) acquireDownloadSlot(ctx context.Context) error {
	if s.downloadSem == nil {
		return nil
	}
	select {
	case s.downloadSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) releaseDownloadSlot() {
	if s.downloadSem != nil {
		<-s.downloadSem
	}
}

// Handler builds the HF API handler. Exposed for tests, which mount it on
// httptest servers.
//
// Routing note: the Hub's URL grammar (a 1- or 2-segment repo id in the
// first position with keywords like "resolve" mid-path) has overlaps that
// net/http's ServeMux rejects as pattern conflicts, so everything HF-shaped
// is dispatched through hfapi.ParseRoute; only fixed operational endpoints
// live on the mux directly.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/whoami-v2", s.instrument("whoami", s.handleWhoAmI))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /{$}", s.instrument("root", s.handleRoot))
	mux.HandleFunc("/", s.dispatchHF)
	return mux
}

// dispatchHF routes HF API URLs parsed by hfapi.ParseRoute.
func (s *Server) dispatchHF(w http.ResponseWriter, r *http.Request) {
	route, ok := hfapi.ParseRoute(r.URL.EscapedPath())
	if !ok {
		s.instrument("unknown", func(w http.ResponseWriter, r *http.Request) {
			writeHFError(w, http.StatusNotFound, "", "Not found.")
		})(w, r)
		return
	}
	if route.RepoKind != hfapi.RepoKindModel {
		s.instrument("unknown", func(w http.ResponseWriter, r *http.Request) {
			writeHFError(w, http.StatusNotFound, hfapi.ErrorCodeRepoNotFound, "Dataset repos are not supported yet.")
		})(w, r)
		return
	}

	switch {
	case route.Kind == hfapi.RouteRepoInfo && r.Method == http.MethodGet:
		s.instrument("model_info", s.withRoute(route, s.handleModelInfo))(w, r)
	case route.Kind == hfapi.RouteTree && r.Method == http.MethodGet:
		s.instrument("tree", s.withRoute(route, s.handleTree))(w, r)
	case route.Kind == hfapi.RouteResolve && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.instrument("resolve", s.withRoute(route, s.handleResolve))(w, r)
	default:
		s.instrument("unknown", func(w http.ResponseWriter, r *http.Request) {
			writeHFError(w, http.StatusMethodNotAllowed, "", "Method not allowed.")
		})(w, r)
	}
}

type routeKey struct{}

// withRoute stashes the parsed route in the request context for handlers.
func (s *Server) withRoute(route hfapi.Route, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, route)))
	}
}

func routeFrom(r *http.Request) hfapi.Route {
	route, _ := r.Context().Value(routeKey{}).(hfapi.Route)
	return route
}

// Run serves until ctx is canceled, then drains gracefully.
func (s *Server) Run(ctx context.Context) error {
	var err error
	s.apiListener, err = net.Listen("tcp", s.cfg.Listen.API)
	if err != nil {
		return fmt.Errorf("server: listening on %s: %w", s.cfg.Listen.API, err)
	}
	api := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	servers := []*http.Server{api}
	errCh := make(chan error, 2)
	go func() { errCh <- api.Serve(s.apiListener) }()
	s.log.Info("api listening", "addr", s.apiListener.Addr().String())

	if s.cfg.Listen.Metrics != "" {
		s.metricsListener, err = net.Listen("tcp", s.cfg.Listen.Metrics)
		if err != nil {
			_ = api.Close()
			return fmt.Errorf("server: listening on %s: %w", s.cfg.Listen.Metrics, err)
		}
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{}))
		mSrv := &http.Server{Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, mSrv)
		go func() { errCh <- mSrv.Serve(s.metricsListener) }()
		s.log.Info("metrics listening", "addr", s.metricsListener.Addr().String())
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var errs []error
		for _, srv := range servers {
			if err := srv.Shutdown(shutdownCtx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// APIAddr returns the bound API address once Run has started listening.
func (s *Server) APIAddr() string {
	if s.apiListener == nil {
		return ""
	}
	return s.apiListener.Addr().String()
}
