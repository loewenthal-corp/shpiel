// Package s3backend implements the Shpiel backend interface on an
// S3-compatible bucket: AWS S3, GCS in interop mode, MinIO, Ceph RGW,
// Cloudflare R2. This is the archive/replica target from the spec — blobs
// keyed by content hash, repo metadata as small objects, no database.
//
// Key layout (under an optional configured prefix):
//
//	models/org/name/
//	  .repo                    # existence marker (CreateRepo)
//	  refs/main                # object containing the commit SHA
//	  manifests/<sha>.json     # manifest JSON, one per commit
//	  blobs/<etag-hex>         # content-addressed blobs (sha1 40-hex for
//	                           # regular files, sha256 64-hex for LFS)
//
// Manifests are self-contained, so commits whose blobs have not all
// arrived yet (pull-through fills blobs lazily) need no staging machinery:
// the manifest object serves immediately and blobs land independently,
// keyed by digest. A repo exists when its marker or any of its objects
// does, keeping the backend restart-safe with no local state.
package s3backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/s3client"
)

// markerKey is the repo existence marker object name. The leading dot
// keeps it out of any real file namespace (refs/, manifests/, blobs/).
const markerKey = ".repo"

// Options configure a bucket backend instance.
type Options struct {
	// Endpoint overrides the AWS endpoint for S3-compatible services
	// (scheme required); empty means AWS S3.
	Endpoint string
	Bucket   string
	// Region is the SigV4 signing region (default us-east-1).
	Region string
	// Prefix is prepended to every key, for sharing a bucket.
	Prefix string
	// Credentials supplies request credentials (static or rotating web
	// identity); nil means anonymous.
	Credentials s3client.CredentialsProvider
}

// Backend implements backend.Backend on a bucket.
type Backend struct {
	name   string
	client *s3client.Client
	prefix string
}

var _ backend.Backend = (*Backend)(nil)

// New creates a bucket backend.
func New(name string, opts Options) (*Backend, error) {
	client, err := s3client.New(s3client.Options{
		Endpoint: opts.Endpoint,
		Bucket:   opts.Bucket,
		Region:   opts.Region,
		Provider: opts.Credentials,
	})
	if err != nil {
		return nil, fmt.Errorf("s3backend: %w", err)
	}
	prefix := strings.Trim(opts.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &Backend{name: name, client: client, prefix: prefix}, nil
}

// Name implements backend.Backend.
func (b *Backend) Name() string { return b.name }

// repoPrefix returns the key prefix all of a repo's objects live under,
// e.g. "models/org/name/". Repo id segments are already S3-key-safe
// (hfapi restricts them to [A-Za-z0-9._-]).
func (b *Backend) repoPrefix(kind hfapi.RepoKind, repo hfapi.RepoID) string {
	return b.prefix + path.Join(kind.APIPrefix(), repo.String()) + "/"
}

func (b *Backend) manifestKey(kind hfapi.RepoKind, repo hfapi.RepoID, commitSHA string) string {
	return b.repoPrefix(kind, repo) + "manifests/" + commitSHA + ".json"
}

func (b *Backend) blobKey(kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest) string {
	// Blobs are named by bare etag hex (sha1 40-hex for regular files,
	// sha256 64-hex for LFS); the lengths keep the namespaces disjoint.
	return b.repoPrefix(kind, repo) + "blobs/" + digest.Hex()
}

// refKey validates a ref name and maps it to its object key. Refs may
// contain slashes (refs/pr/1); traversal shapes and other garbage are
// rejected.
func (b *Backend) refKey(kind hfapi.RepoKind, repo hfapi.RepoID, ref string) (string, error) {
	if !validRelPath(ref) {
		return "", fmt.Errorf("s3backend: invalid ref name %q", ref)
	}
	return b.repoPrefix(kind, repo) + "refs/" + ref, nil
}

// repoExists reports whether any trace of the repo is in the bucket: the
// CreateRepo marker or any object written by PutManifest/PutBlob (repos
// materialize implicitly on pull-through and replication).
func (b *Backend) repoExists(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID) (bool, error) {
	keys, _, err := b.client.List(ctx, b.repoPrefix(kind, repo), "", 1)
	if err != nil {
		return false, fmt.Errorf("s3backend: checking repo %s: %w", repo, err)
	}
	return len(keys) > 0, nil
}

// missErr maps a metadata miss onto RepoNotFound or RevisionNotFound by
// whether the repo has any objects at all.
func (b *Backend) missErr(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID) error {
	exists, err := b.repoExists(ctx, kind, repo)
	if err != nil {
		return err
	}
	if !exists {
		return backend.ErrRepoNotFound
	}
	return backend.ErrRevisionNotFound
}

// CreateRepo implements backend.Backend by writing the marker object.
func (b *Backend) CreateRepo(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID) error {
	exists, err := b.repoExists(ctx, kind, repo)
	if err != nil {
		return err
	}
	if exists {
		return backend.ErrRepoExists
	}
	if err := b.putBytes(ctx, b.repoPrefix(kind, repo)+markerKey, nil); err != nil {
		return fmt.Errorf("s3backend: creating repo: %w", err)
	}
	return nil
}

// DeleteRepo implements backend.Backend, removing every object under the
// repo's prefix.
func (b *Backend) DeleteRepo(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID) error {
	found := false
	token := ""
	for {
		keys, next, err := b.client.List(ctx, b.repoPrefix(kind, repo), token, 0)
		if err != nil {
			return fmt.Errorf("s3backend: listing repo %s: %w", repo, err)
		}
		for _, key := range keys {
			if err := b.client.Delete(ctx, key); err != nil {
				return fmt.Errorf("s3backend: deleting %s: %w", key, err)
			}
			found = true
		}
		if next == "" {
			break
		}
		token = next
	}
	if !found {
		return backend.ErrRepoNotFound
	}
	return nil
}

// ResolveRef implements backend.Backend.
func (b *Backend) ResolveRef(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, ref string) (string, error) {
	// A full commit SHA resolves to itself if we have its manifest.
	if hfapi.IsCommitSHA(ref) {
		if _, err := b.client.Head(ctx, b.manifestKey(kind, repo, ref)); err != nil {
			if errors.Is(err, s3client.ErrNotFound) {
				return "", b.missErr(ctx, kind, repo)
			}
			return "", fmt.Errorf("s3backend: resolving %s@%s: %w", repo, ref, err)
		}
		return ref, nil
	}
	key, err := b.refKey(kind, repo, ref)
	if err != nil {
		return "", b.missErr(ctx, kind, repo)
	}
	rc, err := b.client.Get(ctx, key, 0)
	if err != nil {
		if errors.Is(err, s3client.ErrNotFound) {
			return "", b.missErr(ctx, kind, repo)
		}
		return "", fmt.Errorf("s3backend: resolving %s@%s: %w", repo, ref, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 1<<10))
	if err != nil {
		return "", fmt.Errorf("s3backend: reading ref %s/%s: %w", repo, ref, err)
	}
	sha := strings.TrimSpace(string(data))
	if !hfapi.IsCommitSHA(sha) {
		return "", fmt.Errorf("s3backend: ref %s/%s contains invalid commit %q", repo, ref, sha)
	}
	return sha, nil
}

// GetManifest implements backend.Backend.
func (b *Backend) GetManifest(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, commitSHA string) (*backend.Manifest, error) {
	rc, err := b.client.Get(ctx, b.manifestKey(kind, repo, commitSHA), 0)
	if err != nil {
		if errors.Is(err, s3client.ErrNotFound) {
			return nil, b.missErr(ctx, kind, repo)
		}
		return nil, fmt.Errorf("s3backend: fetching manifest %s@%s: %w", repo, commitSHA, err)
	}
	defer rc.Close()
	var m backend.Manifest
	if err := json.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("s3backend: decoding manifest for %s@%s: %w", repo, commitSHA, err)
	}
	return &m, nil
}

// PutManifest implements backend.Backend: the manifest object first, then
// the refs pointing at it (readers never see a ref without its manifest).
func (b *Backend) PutManifest(ctx context.Context, m *backend.Manifest, refs map[string]string) error {
	if m.Repo.IsZero() || m.CommitSHA == "" {
		return errors.New("s3backend: manifest requires repo and commit SHA")
	}
	kind := m.Kind
	if kind == "" {
		kind = hfapi.RepoKindModel
	}
	for i := range m.Files {
		if !validRelPath(m.Files[i].Path) {
			return fmt.Errorf("s3backend: manifest file path %q is not a safe relative path", m.Files[i].Path)
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("s3backend: encoding manifest: %w", err)
	}
	if err := b.putBytes(ctx, b.manifestKey(kind, m.Repo, m.CommitSHA), data); err != nil {
		return fmt.Errorf("s3backend: writing manifest %s@%s: %w", m.Repo, m.CommitSHA, err)
	}
	for ref, sha := range refs {
		if !hfapi.IsCommitSHA(sha) {
			return fmt.Errorf("s3backend: ref %q points at invalid commit %q", ref, sha)
		}
		key, err := b.refKey(kind, m.Repo, ref)
		if err != nil {
			return err
		}
		if err := b.putBytes(ctx, key, []byte(sha)); err != nil {
			return fmt.Errorf("s3backend: writing ref %s: %w", ref, err)
		}
	}
	return nil
}

// StatBlob implements backend.Backend.
func (b *Backend) StatBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest) (backend.BlobInfo, error) {
	size, err := b.client.Head(ctx, b.blobKey(kind, repo, digest))
	if err != nil {
		if errors.Is(err, s3client.ErrNotFound) {
			return backend.BlobInfo{}, backend.ErrBlobNotFound
		}
		return backend.BlobInfo{}, fmt.Errorf("s3backend: stat blob %s: %w", digest, err)
	}
	return backend.BlobInfo{Digest: digest, Size: size}, nil
}

// OpenBlob implements backend.Backend with a lazy ranged reader, so HTTP
// Range requests against Shpiel translate to ranged object GETs.
func (b *Backend) OpenBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest) (io.ReadSeekCloser, error) {
	rc, err := b.client.OpenRanged(ctx, b.blobKey(kind, repo, digest))
	if err != nil {
		if errors.Is(err, s3client.ErrNotFound) {
			return nil, backend.ErrBlobNotFound
		}
		return nil, fmt.Errorf("s3backend: opening blob %s: %w", digest, err)
	}
	return rc, nil
}

// PutBlob implements backend.Backend. Content is spooled to a temp file
// and verified against the digest before anything is written to the
// bucket, so a corrupt upload never becomes a visible object; the spool
// also supplies the length and payload hash S3 PUTs require.
func (b *Backend) PutBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest, r io.Reader, size int64) error {
	key := b.blobKey(kind, repo, digest)
	if _, err := b.client.Head(ctx, key); err == nil {
		return nil // Content-addressed: already have it.
	}
	spooled, err := spoolAndVerify(r, digest, size)
	if err != nil {
		return err
	}
	defer spooled.cleanup()
	if err := b.client.Put(ctx, key, spooled.file, spooled.size, spooled.payloadSHA256); err != nil {
		return fmt.Errorf("s3backend: writing blob %s: %w", digest, err)
	}
	return nil
}

// Ping implements backend.Backend by listing the bucket.
func (b *Backend) Ping(ctx context.Context) error {
	return b.client.Ping(ctx)
}

// putBytes uploads a small metadata object with its exact payload hash.
func (b *Backend) putBytes(ctx context.Context, key string, data []byte) error {
	return b.client.Put(ctx, key, strings.NewReader(string(data)), int64(len(data)), sha256Hex(data))
}

// validRelPath reports whether p is a safe relative path for use in an
// object key (file paths within manifests, ref names).
func validRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
