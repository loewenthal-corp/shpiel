package fsbackend

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

const testSHA = "1111111111111111111111111111111111111111"

func newEdgeBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New("edge", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func manifestFor(repo hfapi.RepoID, kind hfapi.RepoKind, path string, content []byte) *backend.Manifest {
	return &backend.Manifest{
		Repo:      repo,
		Kind:      kind,
		CommitSHA: testSHA,
		FetchedAt: time.Now(),
		Files: []backend.FileEntry{{
			Path:   path,
			Size:   int64(len(content)),
			Digest: backend.SHA256Digest(fakehub.SHA256Hex(content)),
		}},
	}
}

// TestPutManifestDefaultsKind: a manifest without a kind lands in the model
// namespace, where model reads find it.
func TestPutManifestDefaultsKind(t *testing.T) {
	t.Parallel()
	b := newEdgeBackend(t)
	repo, _ := hfapi.ParseRepoID("org/kindless")
	m := manifestFor(repo, "", "a.txt", []byte("x"))
	if err := b.PutManifest(context.Background(), m, map[string]string{"main": testSHA}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetManifest(context.Background(), hfapi.RepoKindModel, repo, testSHA); err != nil {
		t.Fatalf("manifest not under the models namespace: %v", err)
	}
	if sha, err := b.ResolveRef(context.Background(), hfapi.RepoKindModel, repo, "main"); err != nil || sha != testSHA {
		t.Fatalf("ref not under the models namespace: %s, %v", sha, err)
	}
}

// TestSnapshotLinkedWhenBlobPrecedesManifest: with the blob already stored,
// PutManifest itself must materialize the snapshot file — and re-putting
// the manifest repairs (not breaks) the link.
func TestSnapshotLinkedWhenBlobPrecedesManifest(t *testing.T) {
	t.Parallel()
	b := newEdgeBackend(t)
	repo, _ := hfapi.ParseRepoID("org/linked")
	content := []byte("weights")
	m := manifestFor(repo, hfapi.RepoKindModel, "w.bin", content)
	ctx := context.Background()

	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, m.Files[0].Digest, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	for range 2 { // second round repairs over the existing link
		if err := b.PutManifest(ctx, m, nil); err != nil {
			t.Fatal(err)
		}
		snap := filepath.Join(b.Root(), "models--org--linked", "snapshots", testSHA, "w.bin")
		got, err := os.ReadFile(snap)
		if err != nil {
			t.Fatalf("snapshot file unreadable: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("snapshot content = %q", got)
		}
	}
}

// TestPutManifestRefWriteFailure: a ref that cannot be written fails the
// PutManifest rather than silently dropping the ref update.
func TestPutManifestRefWriteFailure(t *testing.T) {
	t.Parallel()
	b := newEdgeBackend(t)
	repo, _ := hfapi.ParseRepoID("org/reffail")
	ctx := context.Background()
	if err := b.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	refs := filepath.Join(b.Root(), "models--org--reffail", "refs")
	if err := os.Chmod(refs, 0o500); err != nil { //nolint:gosec // dir needs the x bit
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(refs, 0o700) }) //nolint:gosec // restoring a traversable test dir

	m := manifestFor(repo, hfapi.RepoKindModel, "a.txt", []byte("x"))
	if err := b.PutManifest(ctx, m, map[string]string{"main": testSHA}); err == nil {
		t.Fatal("ref write failure swallowed")
	}
}

// TestPutBlobSizeMismatch: a declared size of zero with a non-empty body is
// a short-write error even when the digest matches the bytes.
func TestPutBlobSizeMismatch(t *testing.T) {
	t.Parallel()
	b := newEdgeBackend(t)
	repo, _ := hfapi.ParseRepoID("org/short")
	content := []byte("x")
	d := backend.SHA256Digest(fakehub.SHA256Hex(content))
	err := b.PutBlob(context.Background(), hfapi.RepoKindModel, repo, d, bytes.NewReader(content), 0)
	if err == nil || !strings.Contains(err.Error(), "short write") {
		t.Fatalf("err = %v, want short-write error", err)
	}
	// An empty blob with size 0 is fine.
	empty := backend.SHA256Digest(fakehub.SHA256Hex(nil))
	if err := b.PutBlob(context.Background(), hfapi.RepoKindModel, repo, empty, bytes.NewReader(nil), 0); err != nil {
		t.Fatalf("empty blob rejected: %v", err)
	}
}

// TestPutBlobSHA1UnknownSize: git-OID content with unknown size verifies by
// re-reading the temp file; valid content must land.
func TestPutBlobSHA1UnknownSize(t *testing.T) {
	t.Parallel()
	b := newEdgeBackend(t)
	repo, _ := hfapi.ParseRepoID("org/sha1")
	content := []byte("regular file content")
	d := backend.SHA1Digest(fakehub.GitBlobOID(content))
	ctx := context.Background()
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, d, bytes.NewReader(content), -1); err != nil {
		t.Fatalf("sha1 unknown-size put: %v", err)
	}
	if info, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, d); err != nil || info.Size != int64(len(content)) {
		t.Fatalf("stat after put = %+v, %v", info, err)
	}
	// Corrupt content with unknown size is still caught.
	bad := backend.SHA1Digest(fakehub.GitBlobOID([]byte("something else")))
	err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, bad, bytes.NewReader(content), -1)
	if !errors.Is(err, backend.ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
}

// TestPutBlobLinkFailureSurfaces: when the blob lands but its snapshot
// links cannot be created, PutBlob reports the failure.
func TestPutBlobLinkFailureSurfaces(t *testing.T) {
	t.Parallel()
	b := newEdgeBackend(t)
	repo, _ := hfapi.ParseRepoID("org/linkfail")
	content := []byte("payload")
	m := manifestFor(repo, hfapi.RepoKindModel, "f.bin", content)
	ctx := context.Background()
	if err := b.PutManifest(ctx, m, nil); err != nil {
		t.Fatal(err)
	}
	// Pre-create blobs/ writable, then freeze the repo dir so snapshots/
	// cannot be created.
	repoDir := filepath.Join(b.Root(), "models--org--linkfail")
	if err := os.MkdirAll(filepath.Join(repoDir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(repoDir, 0o500); err != nil { //nolint:gosec // dir needs the x bit
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(repoDir, 0o700) }) //nolint:gosec // restoring a traversable test dir

	err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, m.Files[0].Digest, bytes.NewReader(content), int64(len(content)))
	if err == nil {
		t.Fatal("snapshot link failure swallowed")
	}
}

// TestPutManifestLinkFailureSurfaces: when the blob is present but the
// snapshot link cannot be materialized, PutManifest reports it.
func TestPutManifestLinkFailureSurfaces(t *testing.T) {
	t.Parallel()
	b := newEdgeBackend(t)
	repo, _ := hfapi.ParseRepoID("org/manifestlink")
	content := []byte("payload")
	m := manifestFor(repo, hfapi.RepoKindModel, "f.bin", content)
	ctx := context.Background()
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, m.Files[0].Digest, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(b.Root(), "models--org--manifestlink")
	if err := os.Chmod(repoDir, 0o500); err != nil { //nolint:gosec // dir needs the x bit
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(repoDir, 0o700) }) //nolint:gosec // restoring a traversable test dir

	if err := b.PutManifest(ctx, m, nil); err == nil {
		t.Fatal("snapshot link failure swallowed by PutManifest")
	}
}

func TestPing(t *testing.T) {
	t.Parallel()
	b := newEdgeBackend(t)
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on writable root: %v", err)
	}
	if err := os.Chmod(b.Root(), 0o500); err != nil { //nolint:gosec // dir needs the x bit
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(b.Root(), 0o700) }) //nolint:gosec // restoring a traversable test dir
	if err := b.Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded on read-only root")
	}
}

func TestCreateDeleteRepo(t *testing.T) {
	t.Parallel()
	b := newEdgeBackend(t)
	repo, _ := hfapi.ParseRepoID("org/lifecycle")
	ctx := context.Background()
	if err := b.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateRepo(ctx, hfapi.RepoKindModel, repo); !errors.Is(err, backend.ErrRepoExists) {
		t.Fatalf("second create = %v, want ErrRepoExists", err)
	}
	if err := b.DeleteRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteRepo(ctx, hfapi.RepoKindModel, repo); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Fatalf("second delete = %v, want ErrRepoNotFound", err)
	}
	if _, err := New("bad", ""); err == nil {
		t.Fatal("empty root accepted")
	}
}
