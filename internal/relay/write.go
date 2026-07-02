package relay

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// Write-path errors mapped by the HTTP layer.
var (
	ErrRepoExists     = errors.New("relay: repo already exists")
	ErrParentMismatch = errors.New("relay: parent commit mismatch")
	ErrLFSBlobMissing = errors.New("relay: lfs blob not uploaded")
	ErrBadRevision    = errors.New("relay: commits must target a branch, not a commit")
)

// CreateRepo initializes a repo with an empty initial commit on main, so
// reads (model info, tree) work immediately after creation — mirroring the
// Hub, which seeds new repos with an initial commit.
func (r *Relay) CreateRepo(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID) error {
	route := r.router.For(repo)
	if route == nil {
		return fmt.Errorf("%w: %s", ErrNoRoute, repo)
	}
	if err := route.Primary.CreateRepo(ctx, kind, repo); err != nil {
		if errors.Is(err, backend.ErrRepoExists) {
			return ErrRepoExists
		}
		return err
	}
	now := time.Now().UTC()
	m := &backend.Manifest{
		Repo:      repo,
		Kind:      kind,
		CommitSHA: mintCommitSHA("", nil, "init:"+string(kind)+":"+repo.String()),
		FetchedAt: now,
		CreatedAt: now,
		Files:     []backend.FileEntry{},
	}
	if err := route.Primary.PutManifest(ctx, m, map[string]string{hfapi.DefaultRevision: m.CommitSHA}); err != nil {
		return fmt.Errorf("relay: writing initial commit for %s: %w", repo, err)
	}
	r.log.InfoContext(ctx, "created repo", "repo", repo.String(), "kind", string(kind), "backend", route.Primary.Name())
	r.fanOutCommit(ctx, kind, repo, route, m.CommitSHA, map[string]string{hfapi.DefaultRevision: m.CommitSHA})
	return nil
}

// fanOutCommit enqueues async replication of a commit to the route's
// replicas. Enqueue failures are logged, never surfaced: the primary write
// already succeeded and the admin API exposes replication health.
func (r *Relay) fanOutCommit(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, route *Route, commitSHA string, refs map[string]string) {
	if r.replicator == nil || len(route.Replicas) == 0 {
		return
	}
	targets := make([]string, 0, len(route.Replicas))
	for _, rep := range route.Replicas {
		targets = append(targets, rep.Name())
	}
	if err := r.replicator.EnqueueCommit(kind, repo, route.Primary.Name(), commitSHA, refs, targets); err != nil {
		r.log.ErrorContext(ctx, "enqueueing replication failed",
			"repo", repo.String(), "commit", commitSHA, "error", err)
	}
}

// DeleteRepo removes a repo from its routed backend.
func (r *Relay) DeleteRepo(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID) error {
	route := r.router.For(repo)
	if route == nil {
		return fmt.Errorf("%w: %s", ErrNoRoute, repo)
	}
	if err := route.Primary.DeleteRepo(ctx, kind, repo); err != nil {
		return mapBackendErr(err)
	}
	r.log.InfoContext(ctx, "deleted repo", "repo", repo.String(), "kind", string(kind), "backend", route.Primary.Name())
	if r.replicator != nil && len(route.Replicas) > 0 {
		targets := make([]string, 0, len(route.Replicas))
		for _, rep := range route.Replicas {
			targets = append(targets, rep.Name())
		}
		if err := r.replicator.EnqueueDeleteRepo(kind, repo, route.Primary.Name(), targets); err != nil {
			r.log.ErrorContext(ctx, "enqueueing replication delete failed", "repo", repo.String(), "error", err)
		}
	}
	return nil
}

// HasLFSBlob reports whether the routed backend already holds an LFS blob,
// powering upload dedup in the batch API.
func (r *Relay) HasLFSBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, oid string) bool {
	route := r.router.For(repo)
	if route == nil {
		return false
	}
	_, err := route.Primary.StatBlob(ctx, kind, repo, backend.SHA256Digest(oid))
	return err == nil
}

// PutLFSBlob streams an uploaded LFS object into the routed backend,
// verifying it against the declared sha256.
func (r *Relay) PutLFSBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, oid string, size int64, body io.Reader) error {
	route := r.router.For(repo)
	if route == nil {
		return fmt.Errorf("%w: %s", ErrNoRoute, repo)
	}
	bk := route.Primary
	if err := bk.PutBlob(ctx, kind, repo, backend.SHA256Digest(oid), body, size); err != nil {
		return err
	}
	if r.metrics != nil {
		r.metrics.UploadBytes.WithLabelValues(bk.Name()).Add(float64(size))
	}
	return nil
}

// CommitOps is a parsed commit payload.
type CommitOps struct {
	Summary      string
	ParentCommit string
	// Files are inline ("regular") uploads with content in hand.
	Files []CommitOpFile
	// LFSFiles reference blobs previously uploaded via the batch flow.
	LFSFiles []hfapi.CommitLFSFile
	// DeletedFiles and DeletedFolders remove paths from the parent tree.
	DeletedFiles   []string
	DeletedFolders []string
}

// CommitOpFile is one inline file with decoded content.
type CommitOpFile struct {
	Path    string
	Content []byte
}

// CommitResult reports the created commit.
type CommitResult struct {
	CommitSHA string
}

// Commit applies ops on top of revision (a branch) and advances the branch
// to the new commit. Commits to the same repo serialize on an in-process
// lock: v1 is single-writer by design (spec §6).
func (r *Relay) Commit(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, revision string, ops *CommitOps) (*CommitResult, error) {
	if hfapi.IsCommitSHA(revision) {
		return nil, ErrBadRevision
	}
	route := r.router.For(repo)
	if route == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoRoute, repo)
	}
	bk := route.Primary

	unlock := r.lockRepo(string(kind) + ":" + repo.String())
	defer unlock()

	parentSHA, err := bk.ResolveRef(ctx, kind, repo, revision)
	if err != nil {
		return nil, mapBackendErr(err)
	}
	if ops.ParentCommit != "" && ops.ParentCommit != parentSHA {
		return nil, fmt.Errorf("%w: branch %s is at %s, commit expects parent %s", ErrParentMismatch, revision, parentSHA, ops.ParentCommit)
	}
	parent, err := bk.GetManifest(ctx, kind, repo, parentSHA)
	if err != nil {
		return nil, mapBackendErr(err)
	}

	files := map[string]backend.FileEntry{}
	for _, f := range parent.Files {
		files[f.Path] = f
	}
	for _, del := range ops.DeletedFiles {
		delete(files, del)
	}
	for _, dir := range ops.DeletedFolders {
		prefix := strings.TrimSuffix(dir, "/") + "/"
		for path := range files {
			if strings.HasPrefix(path, prefix) {
				delete(files, path)
			}
		}
	}

	// Inline files: store content-addressed by sha256 (uniform across
	// backends, OCI included) and record the git blob OID, which is the
	// ETag the read contract serves for regular files.
	for _, f := range ops.Files {
		sum := sha256.Sum256(f.Content)
		digest := backend.SHA256Digest(hex.EncodeToString(sum[:]))
		if err := bk.PutBlob(ctx, kind, repo, digest, bytes.NewReader(f.Content), int64(len(f.Content))); err != nil {
			return nil, fmt.Errorf("relay: storing inline file %s: %w", f.Path, err)
		}
		files[f.Path] = backend.FileEntry{
			Path:   f.Path,
			Size:   int64(len(f.Content)),
			Digest: digest,
			OID:    gitBlobOID(f.Content),
		}
	}

	// LFS files: the blobs must already be in the backend via the batch
	// upload flow; a commit referencing a missing blob is a client bug.
	var missing []string
	for _, f := range ops.LFSFiles {
		digest := backend.SHA256Digest(f.OID)
		if _, err := bk.StatBlob(ctx, kind, repo, digest); err != nil {
			missing = append(missing, f.Path)
			continue
		}
		files[f.Path] = backend.FileEntry{
			Path:   f.Path,
			Size:   f.Size,
			Digest: digest,
			OID:    gitBlobOID(lfsPointerContent(f.OID, f.Size)),
			LFS: &hfapi.LFSInfo{
				SHA256:      f.OID,
				OID:         f.OID,
				Size:        f.Size,
				PointerSize: int64(len(lfsPointerContent(f.OID, f.Size))),
			},
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrLFSBlobMissing, strings.Join(missing, ", "))
	}

	sorted := make([]backend.FileEntry, 0, len(files))
	for _, f := range files {
		sorted = append(sorted, f)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	// No-op commits return the parent unchanged: a client retrying a
	// commit whose response was lost converges instead of forking, and
	// re-pushing identical content never advances the branch.
	if sameTree(parent.Files, sorted) {
		return &CommitResult{CommitSHA: parentSHA}, nil
	}

	now := time.Now().UTC()
	m := &backend.Manifest{
		Repo:      repo,
		Kind:      kind,
		CommitSHA: mintCommitSHA(parentSHA, sorted, ops.Summary),
		FetchedAt: now,
		CreatedAt: now,
		Files:     sorted,
	}
	if err := bk.PutManifest(ctx, m, map[string]string{revision: m.CommitSHA}); err != nil {
		return nil, fmt.Errorf("relay: writing commit: %w", err)
	}
	r.log.InfoContext(ctx, "commit",
		"repo", repo.String(), "revision", revision, "commit", m.CommitSHA,
		"parent", parentSHA, "files", len(sorted), "summary", ops.Summary, "backend", bk.Name())
	r.fanOutCommit(ctx, kind, repo, route, m.CommitSHA, map[string]string{revision: m.CommitSHA})
	return &CommitResult{CommitSHA: m.CommitSHA}, nil
}

// sameTree reports whether two sorted file lists describe identical trees.
func sameTree(a, b []backend.FileEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Digest != b[i].Digest || a[i].Size != b[i].Size {
			return false
		}
	}
	return true
}

// lockRepo serializes writers per repo.
func (r *Relay) lockRepo(key string) func() {
	v, _ := r.repoLocks.LoadOrStore(key, &sync.Mutex{})
	mu, ok := v.(*sync.Mutex)
	if !ok {
		panic("relay: repoLocks holds a non-mutex")
	}
	mu.Lock()
	return mu.Unlock
}

// mintCommitSHA derives a commit id from the parent, the tree, and the
// summary. Content-addressed: identical trees with identical parents get
// identical commits, making retried commits idempotent.
func mintCommitSHA(parent string, files []backend.FileEntry, summary string) string {
	h := sha1.New()
	fmt.Fprintf(h, "parent %s\n", parent)
	fmt.Fprintf(h, "summary %s\n", summary)
	for _, f := range files {
		fmt.Fprintf(h, "%s %s %d\n", f.Path, f.Digest, f.Size)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// gitBlobOID computes the git object id of content — the Hub's ETag for
// regular files and its blobId for LFS pointers.
func gitBlobOID(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// lfsPointerContent renders the canonical git-lfs pointer file.
func lfsPointerContent(oid string, size int64) []byte {
	return fmt.Appendf(nil, "version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size)
}
