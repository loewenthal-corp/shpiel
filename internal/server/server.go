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
	"github.com/loewenthal-corp/shpiel/internal/upstream"
)

// Server hosts the HF API listener and the optional metrics listener.
type Server struct {
	cfg     config.Config
	relay   *relay.Relay
	metrics *metrics.Metrics
	log     *slog.Logger

	// upstream is used for whoami proxying and passthrough token
	// validation; it is set even when pull-through is disabled.
	upstream *upstream.Client
	tokens   *tokenValidator

	// downloadSem / uploadSem bound concurrent content transfers
	// (limits.max_concurrent_*); nil means unlimited.
	downloadSem chan struct{}
	uploadSem   chan struct{}

	apiListener     net.Listener
	metricsListener net.Listener
}

// New assembles a Server. up may be nil (no upstream configured).
func New(cfg config.Config, rl *relay.Relay, up *upstream.Client, m *metrics.Metrics, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:      cfg,
		relay:    rl,
		metrics:  m,
		log:      log,
		upstream: up,
		tokens:   newTokenValidator(cfg.Auth.CacheTTL),
	}
	if n := cfg.Limits.MaxConcurrentDownloads; n > 0 {
		s.downloadSem = make(chan struct{}, n)
	}
	if n := cfg.Limits.MaxConcurrentUploads; n > 0 {
		s.uploadSem = make(chan struct{}, n)
	}
	return s
}

// acquireDownloadSlot blocks until a download slot frees up or the request
// is canceled.
func (s *Server) acquireDownloadSlot(ctx context.Context) error {
	return acquireSlot(ctx, s.downloadSem)
}

func (s *Server) releaseDownloadSlot() { releaseSlot(s.downloadSem) }

// acquireUploadSlot blocks until an upload slot frees up or the request is
// canceled.
func (s *Server) acquireUploadSlot(ctx context.Context) error {
	return acquireSlot(ctx, s.uploadSem)
}

func (s *Server) releaseUploadSlot() { releaseSlot(s.uploadSem) }

func acquireSlot(ctx context.Context, sem chan struct{}) error {
	if sem == nil {
		return nil
	}
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseSlot(sem chan struct{}) {
	if sem != nil {
		<-sem
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
	mux.HandleFunc("POST /api/repos/create", s.instrument("repo_create", s.handleCreateRepo))
	mux.HandleFunc("DELETE /api/repos/delete", s.instrument("repo_delete", s.handleDeleteRepo))
	// The LFS upload href minted by the batch API; "shpiel-lfs" is a
	// reserved first segment (not a valid place for a repo owner clash to
	// matter: hrefs are always server-generated).
	mux.HandleFunc("PUT /shpiel-lfs/{kind}/{rest...}", s.instrument("lfs_upload", s.handleLFSUpload))
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
	case route.Kind == hfapi.RoutePreupload && r.Method == http.MethodPost:
		s.instrument("preupload", s.withRoute(route, s.handlePreupload))(w, r)
	case route.Kind == hfapi.RouteCommit && r.Method == http.MethodPost:
		s.instrument("commit", s.withRoute(route, s.handleCommit))(w, r)
	case route.Kind == hfapi.RouteLFSBatch && r.Method == http.MethodPost:
		s.instrument("lfs_batch", s.withRoute(route, s.handleLFSBatch))(w, r)
	case route.Kind == hfapi.RouteXetToken:
		s.instrument("xet_token", func(w http.ResponseWriter, r *http.Request) {
			// huggingface_hub >= 1.x uploads via Xet by default and does
			// not fall back to LFS on failure; until Shpiel ships Xet
			// support (spec M3/M4), clients must disable it explicitly.
			writeHFError(w, http.StatusNotFound, "",
				"Xet is not supported by this endpoint yet. Set HF_HUB_DISABLE_XET=1 to upload via LFS.")
		})(w, r)
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
