// Package relay orchestrates reads across backends and the upstream Hub:
// serve from the routed backend when possible, otherwise pull through from
// upstream, persist, and serve. Concurrent misses for the same object are
// collapsed with singleflight so a fleet of nodes pulling one model incurs
// one upstream fetch.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/metrics"
	"github.com/loewenthal-corp/shpiel/internal/upstream"
)

// Errors the HTTP layer maps onto HF error codes.
var (
	ErrRepoNotFound     = errors.New("relay: repo not found")
	ErrRevisionNotFound = errors.New("relay: revision not found")
	ErrEntryNotFound    = errors.New("relay: entry not found")
	ErrNoRoute          = errors.New("relay: no route for repo")
)

// Relay serves manifests and blobs, pulling through from upstream on miss.
type Relay struct {
	router          *Router
	upstream        *upstream.Client // nil disables pull-through
	refreshInterval time.Duration
	metrics         *metrics.Metrics
	log             *slog.Logger

	manifestSF singleflight.Group
	blobSF     singleflight.Group
}

// Options configure a Relay.
type Options struct {
	Router *Router
	// Upstream enables pull-through when non-nil.
	Upstream *upstream.Client
	// RefreshInterval bounds how stale a cached branch/tag resolution may
	// be before revalidating upstream. Zero revalidates on every request.
	RefreshInterval time.Duration
	Metrics         *metrics.Metrics
	Log             *slog.Logger
}

// New creates a Relay.
func New(opts Options) *Relay {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Relay{
		router:          opts.Router,
		upstream:        opts.Upstream,
		refreshInterval: opts.RefreshInterval,
		metrics:         opts.Metrics,
		log:             log,
	}
}

// PullThroughEnabled reports whether an upstream is configured.
func (r *Relay) PullThroughEnabled() bool { return r.upstream != nil }

// Upstream returns the upstream client, or nil.
func (r *Relay) Upstream() *upstream.Client { return r.upstream }

// Backends returns the set of distinct backends reachable via routes.
func (r *Relay) Backends() []backend.Backend {
	seen := map[string]bool{}
	var out []backend.Backend
	for i := range r.router.routes {
		route := &r.router.routes[i]
		for _, b := range append([]backend.Backend{route.Primary}, route.Replicas...) {
			if !seen[b.Name()] {
				seen[b.Name()] = true
				out = append(out, b)
			}
		}
	}
	return out
}

// ResolveManifest resolves repo@revision to a manifest, consulting the
// routed backend first and pulling through from upstream on miss or when a
// cached moving ref (branch/tag) is stale.
func (r *Relay) ResolveManifest(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, revision, callerToken string) (*backend.Manifest, error) {
	route := r.router.For(repo)
	if route == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoRoute, repo)
	}
	bk := route.Primary

	local, localErr := r.localManifest(ctx, bk, kind, repo, revision)
	if localErr == nil {
		// Commit SHAs are immutable; moving refs are revalidated when the
		// cached copy outlives the refresh interval.
		if hfapi.IsCommitSHA(revision) || !r.stale(local) || r.upstream == nil {
			return local, nil
		}
	}
	if r.upstream == nil {
		return nil, localErr
	}

	fetched, err := r.fetchManifest(ctx, bk, kind, repo, revision, callerToken)
	if err != nil {
		if local != nil {
			// Upstream is down or denies us: serve the stale copy rather
			// than failing a read we can satisfy.
			r.log.WarnContext(ctx, "serving stale manifest, upstream revalidation failed",
				"repo", repo.String(), "revision", revision, "error", err)
			return local, nil
		}
		return nil, err
	}
	return fetched, nil
}

func (r *Relay) localManifest(ctx context.Context, bk backend.Backend, kind hfapi.RepoKind, repo hfapi.RepoID, revision string) (*backend.Manifest, error) {
	sha, err := bk.ResolveRef(ctx, kind, repo, revision)
	if err != nil {
		return nil, mapBackendErr(err)
	}
	m, err := bk.GetManifest(ctx, kind, repo, sha)
	if err != nil {
		return nil, mapBackendErr(err)
	}
	return m, nil
}

func (r *Relay) stale(m *backend.Manifest) bool {
	if r.refreshInterval <= 0 {
		return true
	}
	return time.Since(m.FetchedAt) > r.refreshInterval
}

// fetchManifest pulls repo@revision metadata from upstream and persists it,
// collapsing concurrent fetches for the same key.
func (r *Relay) fetchManifest(ctx context.Context, bk backend.Backend, kind hfapi.RepoKind, repo hfapi.RepoID, revision, callerToken string) (*backend.Manifest, error) {
	key := string(kind) + ":" + repo.String() + "@" + revision
	v, err, _ := r.manifestSF.Do(key, func() (any, error) {
		info, err := r.upstream.GetModelInfo(ctx, kind, repo, revision, callerToken)
		if err != nil {
			r.countPullThrough("manifest", "error")
			return nil, mapUpstreamErr(err)
		}
		m, err := upstream.ManifestFromModelInfo(kind, repo, info, time.Now().UTC())
		if err != nil {
			r.countPullThrough("manifest", "error")
			return nil, err
		}
		refs := map[string]string{}
		if !hfapi.IsCommitSHA(revision) {
			refs[revision] = m.CommitSHA
		}
		if err := bk.PutManifest(ctx, m, refs); err != nil {
			r.countPullThrough("manifest", "error")
			return nil, fmt.Errorf("relay: persisting manifest for %s@%s to %s: %w", repo, revision, bk.Name(), err)
		}
		r.countPullThrough("manifest", "ok")
		r.log.InfoContext(ctx, "pulled manifest from upstream",
			"repo", repo.String(), "revision", revision, "commit", m.CommitSHA, "files", len(m.Files), "backend", bk.Name())
		return m, nil
	})
	if err != nil {
		return nil, err
	}
	m, ok := v.(*backend.Manifest)
	if !ok {
		return nil, fmt.Errorf("relay: unexpected singleflight value %T", v)
	}
	return m, nil
}

// Content is a served blob plus the source it came from.
type Content struct {
	io.ReadSeekCloser
	Entry  *backend.FileEntry
	Source string // metrics.SourceCache or metrics.SourceUpstream
}

// EnsureEntry returns the manifest entry for path with its digest and size
// guaranteed present, backfilling from upstream when the original listing
// lacked blob details. HEAD requests use this without touching content.
func (r *Relay) EnsureEntry(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, m *backend.Manifest, path, callerToken string) (*backend.FileEntry, error) {
	route := r.router.For(repo)
	if route == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoRoute, repo)
	}
	entry := m.File(path)
	if entry == nil {
		return nil, ErrEntryNotFound
	}
	if entry.Digest.IsZero() {
		if err := r.backfillDigest(ctx, route.Primary, kind, repo, m, entry, callerToken); err != nil {
			return nil, err
		}
	}
	return entry, nil
}

// OpenFile opens the content for path within a resolved manifest, pulling
// the blob through from upstream on miss. The manifest must come from a
// prior ResolveManifest so pull-through fetches are pinned to an immutable
// commit.
func (r *Relay) OpenFile(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, m *backend.Manifest, path, callerToken string) (*Content, error) {
	route := r.router.For(repo)
	if route == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoRoute, repo)
	}
	bk := route.Primary

	entry, err := r.EnsureEntry(ctx, kind, repo, m, path, callerToken)
	if err != nil {
		return nil, err
	}

	if rc, err := bk.OpenBlob(ctx, kind, repo, entry.Digest); err == nil {
		return &Content{ReadSeekCloser: rc, Entry: entry, Source: metrics.SourceCache}, nil
	}
	if r.upstream == nil {
		return nil, ErrEntryNotFound
	}

	if err := r.fetchBlob(ctx, bk, kind, repo, m.CommitSHA, entry, callerToken); err != nil {
		return nil, err
	}
	rc, err := bk.OpenBlob(ctx, kind, repo, entry.Digest)
	if err != nil {
		return nil, fmt.Errorf("relay: blob %s vanished after pull-through: %w", entry.Digest, err)
	}
	return &Content{ReadSeekCloser: rc, Entry: entry, Source: metrics.SourceUpstream}, nil
}

// backfillDigest fills a manifest entry whose upstream listing lacked blob
// details by HEADing the file upstream.
func (r *Relay) backfillDigest(ctx context.Context, bk backend.Backend, kind hfapi.RepoKind, repo hfapi.RepoID, m *backend.Manifest, entry *backend.FileEntry, callerToken string) error {
	if r.upstream == nil {
		return ErrEntryNotFound
	}
	meta, err := r.upstream.StatFile(ctx, kind, repo, m.CommitSHA, entry.Path, callerToken)
	if err != nil {
		return mapUpstreamErr(err)
	}
	if meta.LinkedETag != "" {
		entry.Digest = backend.SHA256Digest(meta.LinkedETag)
		entry.LFS = &hfapi.LFSInfo{SHA256: meta.LinkedETag, OID: meta.LinkedETag, Size: meta.Size}
	} else if meta.ETag != "" {
		entry.Digest = backend.SHA1Digest(meta.ETag)
	} else {
		return fmt.Errorf("relay: upstream returned no etag for %s@%s/%s", repo, m.CommitSHA, entry.Path)
	}
	entry.Size = meta.Size
	// Persist the enriched manifest so the backfill happens once.
	if err := bk.PutManifest(ctx, m, nil); err != nil {
		return fmt.Errorf("relay: persisting backfilled manifest: %w", err)
	}
	return nil
}

// fetchBlob downloads one file at an immutable commit from upstream into
// the backend, collapsing concurrent fetches of the same blob.
func (r *Relay) fetchBlob(ctx context.Context, bk backend.Backend, kind hfapi.RepoKind, repo hfapi.RepoID, commitSHA string, entry *backend.FileEntry, callerToken string) error {
	key := string(kind) + ":" + repo.String() + ":" + entry.Digest.String()
	_, err, _ := r.blobSF.Do(key, func() (any, error) {
		start := time.Now()
		body, meta, err := r.upstream.OpenFile(ctx, kind, repo, commitSHA, entry.Path, callerToken)
		if err != nil {
			r.countPullThrough("blob", "error")
			return nil, mapUpstreamErr(err)
		}
		defer body.Close()

		size := entry.Size
		if meta.Size > 0 {
			size = meta.Size
		}
		if err := bk.PutBlob(ctx, kind, repo, entry.Digest, body, size); err != nil {
			r.countPullThrough("blob", "error")
			return nil, fmt.Errorf("relay: persisting blob %s to %s: %w", entry.Digest, bk.Name(), err)
		}
		r.countPullThrough("blob", "ok")
		if r.metrics != nil {
			r.metrics.UploadBytes.WithLabelValues(bk.Name()).Add(float64(size))
		}
		r.log.InfoContext(ctx, "pulled blob from upstream",
			"repo", repo.String(), "path", entry.Path, "digest", entry.Digest.String(),
			"bytes", size, "backend", bk.Name(), "took", time.Since(start).Round(time.Millisecond).String())
		return nil, nil
	})
	return err
}

// Ping checks every routed backend, and upstream when configured.
func (r *Relay) Ping(ctx context.Context) error {
	var errs []error
	for _, bk := range r.Backends() {
		if err := bk.Ping(ctx); err != nil {
			errs = append(errs, fmt.Errorf("backend %s: %w", bk.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func (r *Relay) countPullThrough(kind, outcome string) {
	if r.metrics != nil {
		r.metrics.PullThroughFetches.WithLabelValues(kind, outcome).Inc()
	}
}

func mapBackendErr(err error) error {
	switch {
	case errors.Is(err, backend.ErrRepoNotFound):
		return ErrRepoNotFound
	case errors.Is(err, backend.ErrRevisionNotFound):
		return ErrRevisionNotFound
	case errors.Is(err, backend.ErrBlobNotFound):
		return ErrEntryNotFound
	default:
		return err
	}
}

func mapUpstreamErr(err error) error {
	switch {
	case errors.Is(err, upstream.ErrRepoNotFound):
		return ErrRepoNotFound
	case errors.Is(err, upstream.ErrRevisionNotFound):
		return ErrRevisionNotFound
	case errors.Is(err, upstream.ErrEntryNotFound):
		return ErrEntryNotFound
	default:
		return err
	}
}
