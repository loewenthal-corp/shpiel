// Package backend defines the storage driver interface every Shpiel backend
// implements: repo metadata (refs and manifests) plus content-addressed
// blobs. The HF API surface is the stable front; backends are drivers behind
// this interface (OCI registries, object storage, filesystems, upstream HF).
package backend

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// Sentinel errors backends return so the HTTP layer can map them onto HF
// error codes (RepoNotFound, RevisionNotFound, EntryNotFound).
var (
	ErrRepoNotFound     = errors.New("repo not found")
	ErrRevisionNotFound = errors.New("revision not found")
	ErrBlobNotFound     = errors.New("blob not found")
	ErrDigestMismatch   = errors.New("blob digest mismatch")
)

// Manifest describes one immutable snapshot (commit) of a repo: the file
// list with sizes and content addresses. It is Shpiel's internal unit of
// repo metadata; every backend can store and retrieve it.
type Manifest struct {
	Repo      hfapi.RepoID   `json:"repo"`
	Kind      hfapi.RepoKind `json:"kind"`
	CommitSHA string         `json:"commitSha"`
	// FetchedAt records when this manifest was created or last revalidated;
	// the relay uses it to decide when to recheck a moving ref upstream.
	FetchedAt time.Time   `json:"fetchedAt"`
	CreatedAt time.Time   `json:"createdAt,omitempty"`
	Files     []FileEntry `json:"files"`
}

// File returns the entry for path, or nil if the manifest has no such file.
func (m *Manifest) File(path string) *FileEntry {
	for i := range m.Files {
		if m.Files[i].Path == path {
			return &m.Files[i]
		}
	}
	return nil
}

// FileEntry is one file within a manifest.
type FileEntry struct {
	// Path is the file path relative to the repo root ("config.json",
	// "vae/diffusion_pytorch_model.safetensors").
	Path string `json:"path"`
	// Size in bytes of the content this entry resolves to (for LFS files,
	// the real content, not the pointer).
	Size int64 `json:"size"`
	// Digest is the content address used as the blob storage key. For LFS
	// files this is "sha256:<hex>"; for regular files it is the git blob
	// OID as "sha1:<hex>" (matching the Hub's ETag for such files).
	Digest Digest `json:"digest"`
	// OID is the git object id of the entry as reported in tree listings.
	OID string `json:"oid,omitempty"`
	// LFS carries pointer metadata when the file is stored via LFS/Xet.
	LFS *hfapi.LFSInfo `json:"lfs,omitempty"`
}

// ETag returns the ETag value (unquoted) the HF contract expects for this
// file: the sha256 for LFS files, the git blob OID otherwise.
func (f *FileEntry) ETag() string {
	return f.Digest.Hex()
}

// BlobInfo describes a stored blob.
type BlobInfo struct {
	Digest Digest
	Size   int64
}

// Backend is the storage driver contract. Implementations must be safe for
// concurrent use. Blob operations are content-addressed: the same digest
// always refers to the same bytes, so writes are idempotent and reads are
// immutable.
type Backend interface {
	// Name identifies the backend instance (the key from the config file).
	Name() string

	// ResolveRef maps a ref (branch, tag) or commit SHA to a commit SHA.
	// Returns ErrRepoNotFound if the repo is unknown, ErrRevisionNotFound
	// if the repo exists but the ref does not.
	ResolveRef(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, ref string) (string, error)

	// GetManifest fetches the manifest for a resolved commit SHA.
	GetManifest(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, commitSHA string) (*Manifest, error)

	// PutManifest stores a manifest and points the given refs at its
	// commit. Overwrites any existing manifest for the same commit.
	PutManifest(ctx context.Context, m *Manifest, refs map[string]string) error

	// StatBlob reports whether a blob exists and its size.
	StatBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest Digest) (BlobInfo, error)

	// OpenBlob opens a blob for reading. The returned reader supports
	// seeking so the HTTP layer can serve Range requests.
	OpenBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest Digest) (io.ReadSeekCloser, error)

	// PutBlob stores a blob, verifying the content against digest when the
	// digest algorithm is known. size < 0 means unknown. Idempotent: if the
	// blob already exists the reader may be drained or discarded.
	PutBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest Digest, r io.Reader, size int64) error

	// Ping verifies the backend is reachable and writable enough to serve.
	Ping(ctx context.Context) error
}
