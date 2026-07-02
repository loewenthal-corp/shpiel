// Package fsbackend implements the Shpiel backend interface on a local
// filesystem (or NFS/PVC mount) using the Hugging Face cache layout:
//
//	<root>/
//	  models--org--name/
//	    refs/main                     # file containing the commit SHA
//	    blobs/<etag-hex>              # content-addressed blob store
//	    snapshots/<sha>/<path>        # relative symlinks into blobs/
//	  .shpiel/
//	    models--org--name/manifests/<sha>.json
//
// The layout is byte-compatible with huggingface_hub's cache, so a volume
// written by Shpiel is directly consumable by from_pretrained with
// HF_HUB_OFFLINE=1 and HF_HUB_CACHE pointed at the root. Shpiel's own repo
// metadata (manifests) lives under a .shpiel/ prefix that HF tooling
// ignores.
package fsbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// Backend is a filesystem-backed store rooted at a directory.
type Backend struct {
	name string
	root string
}

var _ backend.Backend = (*Backend)(nil)

// New creates (and if necessary initializes) a filesystem backend rooted at
// root.
func New(name, root string) (*Backend, error) {
	if root == "" {
		return nil, errors.New("fsbackend: root path is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fsbackend: resolving root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("fsbackend: creating root: %w", err)
	}
	return &Backend{name: name, root: abs}, nil
}

// Name implements backend.Backend.
func (b *Backend) Name() string { return b.name }

// Root returns the backend's root directory.
func (b *Backend) Root() string { return b.root }

// repoDirName converts a repo id to the HF cache directory name, e.g.
// ("model", "org/name") -> "models--org--name".
func repoDirName(kind hfapi.RepoKind, repo hfapi.RepoID) string {
	return kind.APIPrefix() + "--" + strings.ReplaceAll(repo.String(), "/", "--")
}

func (b *Backend) repoDir(kind hfapi.RepoKind, repo hfapi.RepoID) string {
	return filepath.Join(b.root, repoDirName(kind, repo))
}

func (b *Backend) manifestPath(kind hfapi.RepoKind, repo hfapi.RepoID, commitSHA string) string {
	return filepath.Join(b.root, ".shpiel", repoDirName(kind, repo), "manifests", commitSHA+".json")
}

// ResolveRef implements backend.Backend.
func (b *Backend) ResolveRef(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, ref string) (string, error) {
	repoDir := b.repoDir(kind, repo)
	if _, err := os.Stat(repoDir); err != nil {
		return "", backend.ErrRepoNotFound
	}
	// A full commit SHA resolves to itself if we have its manifest.
	if hfapi.IsCommitSHA(ref) {
		if _, err := os.Stat(b.manifestPath(kind, repo, ref)); err == nil {
			return ref, nil
		}
		return "", backend.ErrRevisionNotFound
	}
	refPath, err := safeJoin(filepath.Join(repoDir, "refs"), ref)
	if err != nil {
		return "", backend.ErrRevisionNotFound
	}
	data, err := os.ReadFile(refPath)
	if err != nil {
		return "", backend.ErrRevisionNotFound
	}
	sha := strings.TrimSpace(string(data))
	if !hfapi.IsCommitSHA(sha) {
		return "", fmt.Errorf("fsbackend: ref %s/%s contains invalid commit %q", repo, ref, sha)
	}
	return sha, nil
}

// GetManifest implements backend.Backend.
func (b *Backend) GetManifest(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, commitSHA string) (*backend.Manifest, error) {
	data, err := os.ReadFile(b.manifestPath(kind, repo, commitSHA))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, statErr := os.Stat(b.repoDir(kind, repo)); statErr != nil {
				return nil, backend.ErrRepoNotFound
			}
			return nil, backend.ErrRevisionNotFound
		}
		return nil, fmt.Errorf("fsbackend: reading manifest: %w", err)
	}
	var m backend.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("fsbackend: decoding manifest for %s@%s: %w", repo, commitSHA, err)
	}
	return &m, nil
}

// PutManifest implements backend.Backend. It writes the manifest sidecar,
// materializes the snapshot directory (symlinks for blobs already present),
// and updates refs atomically.
func (b *Backend) PutManifest(ctx context.Context, m *backend.Manifest, refs map[string]string) error {
	if m.Repo.IsZero() || m.CommitSHA == "" {
		return errors.New("fsbackend: manifest requires repo and commit SHA")
	}
	kind := m.Kind
	if kind == "" {
		kind = hfapi.RepoKindModel
	}
	for i := range m.Files {
		if !validRelPath(m.Files[i].Path) {
			return fmt.Errorf("fsbackend: manifest file path %q is not a safe relative path", m.Files[i].Path)
		}
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("fsbackend: encoding manifest: %w", err)
	}
	if err := writeFileAtomic(b.manifestPath(kind, m.Repo, m.CommitSHA), data, 0o644); err != nil {
		return err
	}

	// Materialize snapshot symlinks for any blobs we already hold; blobs
	// arriving later are linked by PutBlob.
	for i := range m.Files {
		f := &m.Files[i]
		if f.Digest.IsZero() {
			continue
		}
		if _, err := os.Stat(b.blobPath(kind, m.Repo, f.Digest)); err == nil {
			if err := b.linkSnapshotFile(kind, m.Repo, m.CommitSHA, f.Path, f.Digest); err != nil {
				return err
			}
		}
	}

	for ref, sha := range refs {
		if !hfapi.IsCommitSHA(sha) {
			return fmt.Errorf("fsbackend: ref %q points at invalid commit %q", ref, sha)
		}
		refPath, err := safeJoin(filepath.Join(b.repoDir(kind, m.Repo), "refs"), ref)
		if err != nil {
			return fmt.Errorf("fsbackend: invalid ref name %q", ref)
		}
		if err := writeFileAtomic(refPath, []byte(sha), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) blobPath(kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest) string {
	// HF cache names blobs by bare etag hex (sha1 40-hex for regular files,
	// sha256 64-hex for LFS); the lengths keep the namespaces disjoint.
	return filepath.Join(b.repoDir(kind, repo), "blobs", digest.Hex())
}

// StatBlob implements backend.Backend.
func (b *Backend) StatBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest) (backend.BlobInfo, error) {
	fi, err := os.Stat(b.blobPath(kind, repo, digest))
	if err != nil {
		return backend.BlobInfo{}, backend.ErrBlobNotFound
	}
	return backend.BlobInfo{Digest: digest, Size: fi.Size()}, nil
}

// OpenBlob implements backend.Backend.
func (b *Backend) OpenBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest) (io.ReadSeekCloser, error) {
	f, err := os.Open(b.blobPath(kind, repo, digest))
	if err != nil {
		return nil, backend.ErrBlobNotFound
	}
	return f, nil
}

// PutBlob implements backend.Backend. Content is streamed to a temp file,
// verified against the digest, and renamed into place atomically. After the
// blob lands, any snapshot entries referencing it (across all manifests of
// the repo) are linked.
func (b *Backend) PutBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest, r io.Reader, size int64) error {
	target := b.blobPath(kind, repo, digest)
	if _, err := os.Stat(target); err == nil {
		return nil // Content-addressed: already have it.
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("fsbackend: creating blobs dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".tmp-"+digest.Hex()[:min(12, len(digest.Hex()))]+"-*")
	if err != nil {
		return fmt.Errorf("fsbackend: creating temp blob: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after successful rename

	written, verifyErr := writeAndVerify(tmp, r, digest, size)
	closeErr := tmp.Close()
	if verifyErr != nil {
		return verifyErr
	}
	if closeErr != nil {
		return fmt.Errorf("fsbackend: closing temp blob: %w", closeErr)
	}
	if size >= 0 && written != size {
		return fmt.Errorf("fsbackend: short write for %s: got %d bytes, want %d", digest, written, size)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("fsbackend: committing blob: %w", err)
	}
	return b.linkBlobIntoSnapshots(kind, repo, digest)
}

// Ping implements backend.Backend by verifying the root is writable.
func (b *Backend) Ping(ctx context.Context) error {
	probe, err := os.CreateTemp(b.root, ".ping-*")
	if err != nil {
		return fmt.Errorf("fsbackend: root not writable: %w", err)
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

// linkSnapshotFile creates snapshots/<sha>/<path> as a relative symlink to
// blobs/<hex>, falling back to a hard link (then copy) on filesystems
// without symlink support.
func (b *Backend) linkSnapshotFile(kind hfapi.RepoKind, repo hfapi.RepoID, commitSHA, path string, digest backend.Digest) error {
	snapDir := filepath.Join(b.repoDir(kind, repo), "snapshots", commitSHA)
	linkPath, err := safeJoin(snapDir, path)
	if err != nil {
		return fmt.Errorf("fsbackend: unsafe snapshot path %q", path)
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return fmt.Errorf("fsbackend: creating snapshot dir: %w", err)
	}
	blobPath := b.blobPath(kind, repo, digest)
	relTarget, err := filepath.Rel(filepath.Dir(linkPath), blobPath)
	if err != nil {
		return fmt.Errorf("fsbackend: computing symlink target: %w", err)
	}
	// Replace whatever is there; snapshots are immutable so this only
	// repairs partial state.
	if err := os.Remove(linkPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fsbackend: replacing snapshot link: %w", err)
	}
	if err := os.Symlink(relTarget, linkPath); err == nil {
		return nil
	}
	if err := os.Link(blobPath, linkPath); err == nil {
		return nil
	}
	return copyFile(blobPath, linkPath)
}

// linkBlobIntoSnapshots scans the repo's manifests for entries referencing
// digest and links them into their snapshots.
func (b *Backend) linkBlobIntoSnapshots(kind hfapi.RepoKind, repo hfapi.RepoID, digest backend.Digest) error {
	manifestDir := filepath.Join(b.root, ".shpiel", repoDirName(kind, repo), "manifests")
	entries, err := os.ReadDir(manifestDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // No manifests yet; PutManifest will link later.
	}
	if err != nil {
		return fmt.Errorf("fsbackend: scanning manifests: %w", err)
	}
	for _, e := range entries {
		sha := strings.TrimSuffix(e.Name(), ".json")
		if !hfapi.IsCommitSHA(sha) {
			continue
		}
		m, err := b.GetManifest(context.Background(), kind, repo, sha)
		if err != nil {
			continue
		}
		for i := range m.Files {
			if m.Files[i].Digest == digest {
				if err := b.linkSnapshotFile(kind, repo, sha, m.Files[i].Path, digest); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// writeFileAtomic writes data to path via a temp file + rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("fsbackend: creating dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("fsbackend: creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsbackend: writing temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsbackend: chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fsbackend: closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("fsbackend: renaming into place: %w", err)
	}
	return nil
}

// validRelPath reports whether p is a safe repo-relative file path.
func validRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean != p {
		return false
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// safeJoin joins rel onto base and guarantees the result stays inside base.
func safeJoin(base, rel string) (string, error) {
	if !validRelPath(rel) {
		return "", fmt.Errorf("unsafe path %q", rel)
	}
	return filepath.Join(base, filepath.FromSlash(rel)), nil
}
