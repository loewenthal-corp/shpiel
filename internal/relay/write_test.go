package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// newWriteRelay builds a relay with no upstream (pure write path).
func newWriteRelay(t *testing.T) *Relay {
	t.Helper()
	bk, err := fsbackend.New("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter([]config.Route{{Match: "*", Primary: "test"}}, map[string]backend.Backend{"test": bk})
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{Router: router})
}

func TestCreateRepoAndInitialCommit(t *testing.T) {
	t.Parallel()
	rl := newWriteRelay(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/new")

	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	// Creating again conflicts.
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); !errors.Is(err, ErrRepoExists) {
		t.Fatalf("second CreateRepo = %v, want ErrRepoExists", err)
	}
	// The initial commit makes reads work immediately.
	m, err := rl.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatalf("ResolveManifest after create: %v", err)
	}
	if len(m.Files) != 0 {
		t.Fatalf("initial commit files = %d, want 0", len(m.Files))
	}
	if !hfapi.IsCommitSHA(m.CommitSHA) {
		t.Fatalf("initial commit sha %q invalid", m.CommitSHA)
	}
}

func TestCommitInlineAndLFS(t *testing.T) {
	t.Parallel()
	rl := newWriteRelay(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/write")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}

	weights := bytes.Repeat([]byte{7, 13, 42}, 4096)
	oid := fakehub.SHA256Hex(weights)
	if err := rl.PutLFSBlob(ctx, hfapi.RepoKindModel, repo, oid, int64(len(weights)), bytes.NewReader(weights)); err != nil {
		t.Fatalf("PutLFSBlob: %v", err)
	}

	config := []byte(`{"model_type":"w"}`)
	res, err := rl.Commit(ctx, hfapi.RepoKindModel, repo, "main", &CommitOps{
		Summary: "add model",
		Files:   []CommitOpFile{{Path: "config.json", Content: config}},
		LFSFiles: []hfapi.CommitLFSFile{
			{Path: "model.safetensors", OID: oid, Size: int64(len(weights))},
		},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	m, err := rl.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.CommitSHA != res.CommitSHA {
		t.Fatalf("main = %s, want %s", m.CommitSHA, res.CommitSHA)
	}

	// Inline file: ETag must be the git blob OID even though storage is
	// sha256-keyed.
	cfgEntry := m.File("config.json")
	if cfgEntry == nil {
		t.Fatal("config.json missing from manifest")
	}
	if got, want := cfgEntry.ETag(), fakehub.GitBlobOID(config); got != want {
		t.Errorf("config.json ETag = %s, want git oid %s", got, want)
	}
	if cfgEntry.Digest.Algo() != "sha256" {
		t.Errorf("config.json storage digest = %s, want sha256 keying", cfgEntry.Digest)
	}

	// LFS file: ETag is the content sha256.
	w := m.File("model.safetensors")
	if w == nil || w.LFS == nil {
		t.Fatalf("model.safetensors = %+v, want LFS entry", w)
	}
	if w.ETag() != oid {
		t.Errorf("lfs ETag = %s, want %s", w.ETag(), oid)
	}

	// Content reads back through the normal read path.
	content, err := rl.OpenFile(ctx, hfapi.RepoKindModel, repo, m, "model.safetensors", "")
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close()
	got, _ := io.ReadAll(content)
	if !bytes.Equal(got, weights) {
		t.Fatal("weights read back differ")
	}
}

func TestCommitParentMismatch(t *testing.T) {
	t.Parallel()
	rl := newWriteRelay(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/race")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	_, err := rl.Commit(ctx, hfapi.RepoKindModel, repo, "main", &CommitOps{
		Summary:      "stale parent",
		ParentCommit: "0000000000000000000000000000000000000000",
	})
	if !errors.Is(err, ErrParentMismatch) {
		t.Fatalf("Commit with wrong parent = %v, want ErrParentMismatch", err)
	}
}

func TestCommitMissingLFSBlobRejected(t *testing.T) {
	t.Parallel()
	rl := newWriteRelay(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/missing-blob")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	_, err := rl.Commit(ctx, hfapi.RepoKindModel, repo, "main", &CommitOps{
		Summary: "dangling pointer",
		LFSFiles: []hfapi.CommitLFSFile{
			{Path: "w.bin", OID: fakehub.SHA256Hex([]byte("never uploaded")), Size: 14},
		},
	})
	if !errors.Is(err, ErrLFSBlobMissing) {
		t.Fatalf("Commit with missing blob = %v, want ErrLFSBlobMissing", err)
	}
}

func TestCommitDeletions(t *testing.T) {
	t.Parallel()
	rl := newWriteRelay(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/del")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	_, err := rl.Commit(ctx, hfapi.RepoKindModel, repo, "main", &CommitOps{
		Summary: "seed",
		Files: []CommitOpFile{
			{Path: "keep.txt", Content: []byte("keep")},
			{Path: "drop.txt", Content: []byte("drop")},
			{Path: "dir/a.txt", Content: []byte("a")},
			{Path: "dir/b.txt", Content: []byte("b")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rl.Commit(ctx, hfapi.RepoKindModel, repo, "main", &CommitOps{
		Summary:        "prune",
		DeletedFiles:   []string{"drop.txt"},
		DeletedFolders: []string{"dir"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := rl.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 || m.Files[0].Path != "keep.txt" {
		t.Fatalf("after deletions files = %+v, want only keep.txt", m.Files)
	}
}

func TestCommitToCommitSHARejected(t *testing.T) {
	t.Parallel()
	rl := newWriteRelay(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/pinned")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	m, _ := rl.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	_, err := rl.Commit(ctx, hfapi.RepoKindModel, repo, m.CommitSHA, &CommitOps{Summary: "detached"})
	if !errors.Is(err, ErrBadRevision) {
		t.Fatalf("Commit to sha = %v, want ErrBadRevision", err)
	}
}

func TestCommitIdempotent(t *testing.T) {
	t.Parallel()
	rl := newWriteRelay(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/idem")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	ops := func() *CommitOps {
		return &CommitOps{Summary: "same", Files: []CommitOpFile{{Path: "a.txt", Content: []byte("a")}}}
	}
	r1, err := rl.Commit(ctx, hfapi.RepoKindModel, repo, "main", ops())
	if err != nil {
		t.Fatal(err)
	}
	// Re-pushing identical content is a no-op: the branch stays at the
	// same commit (retried commits converge instead of forking).
	r2, err := rl.Commit(ctx, hfapi.RepoKindModel, repo, "main", ops())
	if err != nil {
		t.Fatal(err)
	}
	if r1.CommitSHA != r2.CommitSHA {
		t.Fatalf("identical commits minted different shas: %s vs %s", r1.CommitSHA, r2.CommitSHA)
	}
	m, err := rl.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.CommitSHA != r1.CommitSHA {
		t.Fatalf("branch moved on no-op commit: %s, want %s", m.CommitSHA, r1.CommitSHA)
	}
}

func TestDeleteRepo(t *testing.T) {
	t.Parallel()
	rl := newWriteRelay(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/gone")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	if err := rl.DeleteRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := rl.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", ""); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("after delete = %v, want ErrRepoNotFound", err)
	}
	if err := rl.DeleteRepo(ctx, hfapi.RepoKindModel, repo); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("double delete = %v, want ErrRepoNotFound", err)
	}
}
