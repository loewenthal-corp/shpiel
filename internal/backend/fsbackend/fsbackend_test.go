package fsbackend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

const commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New("test", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func testManifest(repo string, files map[string][]byte) *backend.Manifest {
	id, _ := hfapi.ParseRepoID(repo)
	m := &backend.Manifest{
		Repo:      id,
		Kind:      hfapi.RepoKindModel,
		CommitSHA: commitA,
		FetchedAt: time.Now().UTC(),
	}
	for path, content := range files {
		m.Files = append(m.Files, backend.FileEntry{
			Path:   path,
			Size:   int64(len(content)),
			Digest: backend.SHA1Digest(fakehub.GitBlobOID(content)),
			OID:    fakehub.GitBlobOID(content),
		})
	}
	return m
}

func TestManifestAndRefRoundtrip(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	m := testManifest("org/repo", map[string][]byte{"config.json": []byte(`{}`)})

	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}

	sha, err := b.ResolveRef(ctx, hfapi.RepoKindModel, m.Repo, "main")
	if err != nil {
		t.Fatalf("ResolveRef(main): %v", err)
	}
	if sha != commitA {
		t.Fatalf("ResolveRef(main) = %q, want %q", sha, commitA)
	}

	// A commit SHA resolves to itself.
	sha, err = b.ResolveRef(ctx, hfapi.RepoKindModel, m.Repo, commitA)
	if err != nil {
		t.Fatalf("ResolveRef(sha): %v", err)
	}
	if sha != commitA {
		t.Fatalf("ResolveRef(sha) = %q, want %q", sha, commitA)
	}

	got, err := b.GetManifest(ctx, hfapi.RepoKindModel, m.Repo, commitA)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "config.json" {
		t.Fatalf("GetManifest files = %+v", got.Files)
	}
}

func TestNotFoundErrors(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("no/such")

	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main"); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Errorf("ResolveRef on missing repo = %v, want ErrRepoNotFound", err)
	}

	m := testManifest("org/repo", map[string][]byte{"a": []byte("x")})
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, m.Repo, "nope"); !errors.Is(err, backend.ErrRevisionNotFound) {
		t.Errorf("ResolveRef on missing ref = %v, want ErrRevisionNotFound", err)
	}
	if _, err := b.StatBlob(ctx, hfapi.RepoKindModel, m.Repo, backend.SHA256Digest(strings.Repeat("0", 64))); !errors.Is(err, backend.ErrBlobNotFound) {
		t.Errorf("StatBlob on missing blob = %v, want ErrBlobNotFound", err)
	}
}

func TestBlobRoundtripAndVerification(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/repo")
	content := []byte("hello weights")

	t.Run("sha256 ok", func(t *testing.T) {
		d := backend.SHA256Digest(fakehub.SHA256Hex(content))
		if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, d, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
		rc, err := b.OpenBlob(ctx, hfapi.RepoKindModel, repo, d)
		if err != nil {
			t.Fatalf("OpenBlob: %v", err)
		}
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if !bytes.Equal(got, content) {
			t.Fatalf("blob content = %q, want %q", got, content)
		}
	})

	t.Run("sha256 corrupt", func(t *testing.T) {
		d := backend.SHA256Digest(strings.Repeat("ab", 32))
		err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, d, bytes.NewReader(content), int64(len(content)))
		if !errors.Is(err, backend.ErrDigestMismatch) {
			t.Fatalf("PutBlob with wrong sha256 = %v, want ErrDigestMismatch", err)
		}
		if _, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, d); !errors.Is(err, backend.ErrBlobNotFound) {
			t.Fatalf("corrupt blob must not be committed, StatBlob = %v", err)
		}
	})

	t.Run("git sha1 known size", func(t *testing.T) {
		d := backend.SHA1Digest(fakehub.GitBlobOID(content))
		if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, d, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("PutBlob git-sha1: %v", err)
		}
	})

	t.Run("git sha1 unknown size", func(t *testing.T) {
		d := backend.SHA1Digest(fakehub.GitBlobOID(content))
		if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, d, bytes.NewReader(content), -1); err != nil {
			t.Fatalf("PutBlob git-sha1 unknown size: %v", err)
		}
	})

	t.Run("git sha1 corrupt", func(t *testing.T) {
		d := backend.SHA1Digest(strings.Repeat("cd", 20))
		err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, d, bytes.NewReader(content), int64(len(content)))
		if !errors.Is(err, backend.ErrDigestMismatch) {
			t.Fatalf("PutBlob with wrong sha1 = %v, want ErrDigestMismatch", err)
		}
	})

	t.Run("short write", func(t *testing.T) {
		d := backend.SHA256Digest(fakehub.SHA256Hex(content))
		repo2, _ := hfapi.ParseRepoID("org/short")
		err := b.PutBlob(ctx, hfapi.RepoKindModel, repo2, d, bytes.NewReader(content), int64(len(content))+5)
		if err == nil {
			t.Fatal("PutBlob with wrong size succeeded, want error")
		}
	})
}

// TestHFCacheLayout locks in the on-disk shape: a volume written by Shpiel
// must be directly consumable as a huggingface_hub cache (HF_HUB_OFFLINE=1).
func TestHFCacheLayout(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()

	config := []byte(`{"model_type":"test"}`)
	weights := []byte("massive tensor bytes")
	repo, _ := hfapi.ParseRepoID("org/name")

	m := &backend.Manifest{
		Repo: repo, Kind: hfapi.RepoKindModel, CommitSHA: commitA, FetchedAt: time.Now(),
		Files: []backend.FileEntry{
			{Path: "config.json", Size: int64(len(config)), Digest: backend.SHA1Digest(fakehub.GitBlobOID(config))},
			{Path: "vae/weights.safetensors", Size: int64(len(weights)), Digest: backend.SHA256Digest(fakehub.SHA256Hex(weights)),
				LFS: &hfapi.LFSInfo{SHA256: fakehub.SHA256Hex(weights), Size: int64(len(weights))}},
		},
	}
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	for i, content := range [][]byte{config, weights} {
		if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, m.Files[i].Digest, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("PutBlob %d: %v", i, err)
		}
	}

	root := b.Root()
	// refs/main holds the commit.
	refData, err := os.ReadFile(filepath.Join(root, "models--org--name", "refs", "main"))
	if err != nil || strings.TrimSpace(string(refData)) != commitA {
		t.Fatalf("refs/main = %q, %v; want %s", refData, err, commitA)
	}
	// blobs are named by bare digest hex.
	if _, err := os.Stat(filepath.Join(root, "models--org--name", "blobs", fakehub.SHA256Hex(weights))); err != nil {
		t.Fatalf("blob file missing: %v", err)
	}
	// snapshot paths materialize, including nested dirs, as symlinks to blobs.
	snapFile := filepath.Join(root, "models--org--name", "snapshots", commitA, "vae", "weights.safetensors")
	fi, err := os.Lstat(snapFile)
	if err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(snapFile)
		if filepath.IsAbs(target) {
			t.Errorf("snapshot symlink must be relative, got %q", target)
		}
	}
	got, err := os.ReadFile(snapFile)
	if err != nil || !bytes.Equal(got, weights) {
		t.Fatalf("snapshot content = %q, %v", got, err)
	}
}

func TestBlobBeforeManifestStillLinks(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/late")
	content := []byte("blob arrives first")
	d := backend.SHA256Digest(fakehub.SHA256Hex(content))

	m := testManifest("org/late", nil)
	m.Files = []backend.FileEntry{{Path: "w.bin", Size: int64(len(content)), Digest: d}}

	// Manifest first (no blob yet), then blob: snapshot link must appear.
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, d, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	snap := filepath.Join(b.Root(), "models--org--late", "snapshots", commitA, "w.bin")
	if got, err := os.ReadFile(snap); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("snapshot after late blob = %q, %v", got, err)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()

	for _, evil := range []string{"../evil", "a/../../evil", "/abs", "a/./b", ""} {
		m := testManifest("org/evil", nil)
		m.Files = []backend.FileEntry{{Path: evil, Size: 1, Digest: backend.SHA256Digest(strings.Repeat("a", 64))}}
		if err := b.PutManifest(ctx, m, nil); err == nil {
			t.Errorf("PutManifest accepted unsafe path %q", evil)
		}
	}

	repo, _ := hfapi.ParseRepoID("org/evil")
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "../../etc/passwd"); err == nil {
		t.Error("ResolveRef accepted traversal ref")
	}
}

func TestPutBlobIdempotent(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/idem")
	content := []byte("same bytes")
	d := backend.SHA256Digest(fakehub.SHA256Hex(content))

	for range 3 {
		if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, d, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
	}
	info, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, d)
	if err != nil || info.Size != int64(len(content)) {
		t.Fatalf("StatBlob = %+v, %v", info, err)
	}
}
