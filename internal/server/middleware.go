package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// statusWriter records the status code and bytes written for logging and
// metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// instrument wraps a handler with request logging and metrics, labeled by a
// stable handler name (never the raw path, to keep cardinality bounded).
func (s *Server) instrument(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		s.metrics.InflightRequests.Inc()
		defer s.metrics.InflightRequests.Dec()

		next(sw, r)

		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		elapsed := time.Since(start)
		s.metrics.HTTPRequests.WithLabelValues(name, r.Method, strconv.Itoa(sw.status)).Inc()
		s.metrics.HTTPDuration.WithLabelValues(name, r.Method).Observe(elapsed.Seconds())
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("handler", name),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Int64("bytes", sw.bytes),
			slog.Duration("took", elapsed),
		)
	}
}

// bearerToken extracts the caller's token from the Authorization header.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}
	return ""
}
