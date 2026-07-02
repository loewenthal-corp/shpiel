package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/audit"
	"gopkg.in/yaml.v3"
)

// adminHandler builds the admin API (spec §5.3): a separate listener,
// separately authenticated, off by default. Every request requires the
// bearer token from admin.token_env.
func (s *Server) adminHandler(token string) http.Handler {
	requireAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(token)) != 1 {
				writeHFError(w, http.StatusUnauthorized, "", "Invalid admin token.")
				return
			}
			next(w, r)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/backends", requireAuth(s.handleAdminBackends))
	mux.HandleFunc("GET /admin/v1/replication", requireAuth(s.handleAdminReplication))
	mux.HandleFunc("POST /admin/v1/replication/retry", requireAuth(s.handleAdminReplicationRetry))
	mux.HandleFunc("GET /admin/v1/config", requireAuth(s.handleAdminConfig))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// handleAdminBackends pings every routed backend.
func (s *Server) handleAdminBackends(w http.ResponseWriter, r *http.Request) {
	type status struct {
		Name    string `json:"name"`
		Healthy bool   `json:"healthy"`
		Error   string `json:"error,omitempty"`
	}
	var out []status
	for _, bk := range s.relay.Backends() {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		err := bk.Ping(ctx)
		cancel()
		st := status{Name: bk.Name(), Healthy: err == nil}
		if err != nil {
			st.Error = err.Error()
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": out})
}

// handleAdminReplication reports the retry queue.
func (s *Server) handleAdminReplication(w http.ResponseWriter, r *http.Request) {
	if s.repl == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "depth": 0, "jobs": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"depth":   s.repl.Depth(),
		"jobs":    s.repl.Snapshot(),
	})
}

// handleAdminReplicationRetry clears backoff on all pending jobs.
func (s *Server) handleAdminReplicationRetry(w http.ResponseWriter, r *http.Request) {
	if s.repl == nil {
		writeJSON(w, http.StatusOK, map[string]any{"kicked": 0})
		return
	}
	n := s.repl.RetryNow()
	s.audit.Record(audit.Event{Action: "admin_replication_retry", Actor: "admin", Detail: map[string]any{"kicked": n}})
	writeJSON(w, http.StatusOK, map[string]any{"kicked": n})
}

// handleAdminConfig returns the effective configuration. Secrets never
// live in config (only *_env indirection), so the whole document is safe
// to expose to admins.
func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	data, err := yaml.Marshal(s.cfg)
	if err != nil {
		writeHFError(w, http.StatusInternalServerError, "", "Rendering config failed.")
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}
