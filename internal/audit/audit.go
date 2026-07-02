// Package audit emits the append-only record of every state-changing
// action: who did what to which repo, when, and with which digests. Table
// stakes for the regulated-org deployment story (spec §5.6).
package audit

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Logger writes structured audit records. A nil *Logger is a no-op, so
// call sites never need nil checks.
type Logger struct {
	log    *slog.Logger
	closer func() error
}

// Open creates a Logger appending JSON lines to path ("-" means stderr).
// An empty path returns nil (auditing disabled).
func Open(path string) (*Logger, error) {
	switch path {
	case "":
		return nil, nil
	case "-":
		return &Logger{log: slog.New(slog.NewJSONHandler(os.Stderr, nil))}, nil
	default:
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("audit: opening %s: %w", path, err)
		}
		return &Logger{
			log:    slog.New(slog.NewJSONHandler(f, nil)),
			closer: f.Close,
		}, nil
	}
}

// Close releases the underlying file, if any.
func (l *Logger) Close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	return l.closer()
}

// Event is one audit record.
type Event struct {
	// Action names what happened: repo_create, repo_delete, commit,
	// lfs_upload, xet_xorb, xet_shard, admin_replication_retry, ...
	Action string
	// Actor is the authenticated identity ("anonymous" when auth.mode is
	// none or the token was absent).
	Actor string
	Repo  string
	// Revision/Commit/Digest pin exactly what changed, where meaningful.
	Revision string
	Commit   string
	Digest   string
	// Detail carries action-specific context (file counts, sizes, ...).
	Detail map[string]any
}

// Record writes an event. Safe on a nil Logger.
func (l *Logger) Record(e Event) {
	if l == nil {
		return
	}
	if e.Actor == "" {
		e.Actor = "anonymous"
	}
	attrs := []slog.Attr{
		slog.Time("ts", time.Now().UTC()),
		slog.String("action", e.Action),
		slog.String("actor", e.Actor),
	}
	if e.Repo != "" {
		attrs = append(attrs, slog.String("repo", e.Repo))
	}
	if e.Revision != "" {
		attrs = append(attrs, slog.String("revision", e.Revision))
	}
	if e.Commit != "" {
		attrs = append(attrs, slog.String("commit", e.Commit))
	}
	if e.Digest != "" {
		attrs = append(attrs, slog.String("digest", e.Digest))
	}
	for k, v := range e.Detail {
		attrs = append(attrs, slog.Any(k, v))
	}
	l.log.LogAttrs(context.Background(), slog.LevelInfo, "audit", attrs...)
}
