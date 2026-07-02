package conformance

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// SeedBackend writes fx directly into a backend (no upstream involved) and
// returns the fixture with CommitSHA populated. This exercises the pure
// read path: what an air-gapped or pre-pushed deployment serves.
func SeedBackend(bk backend.Backend, fx Fixture) (Fixture, error) {
	repo, err := hfapi.ParseRepoID(fx.Repo)
	if err != nil {
		return fx, err
	}
	if fx.CommitSHA == "" {
		// Any stable 40-hex works; derive one from the repo id.
		fx.CommitSHA = fmt.Sprintf("%040x", []byte(fx.Repo)[:min(20, len(fx.Repo))])
	}

	m := &backend.Manifest{
		Repo:      repo,
		Kind:      hfapi.RepoKindModel,
		CommitSHA: fx.CommitSHA,
		FetchedAt: time.Now().UTC(),
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for path, f := range fx.Files {
		// Storage keys are always the content sha256 (the relay's
		// invariant); the git OID rides along as the regular-file ETag.
		entry := backend.FileEntry{
			Path:   path,
			Size:   int64(len(f.Content)),
			Digest: backend.SHA256Digest(fakehub.SHA256Hex(f.Content)),
			OID:    fakehub.GitBlobOID(f.Content),
		}
		if f.LFS {
			sha := fakehub.SHA256Hex(f.Content)
			entry.LFS = &hfapi.LFSInfo{SHA256: sha, OID: sha, Size: entry.Size, PointerSize: 134}
		}
		m.Files = append(m.Files, entry)
	}

	ctx := context.Background()
	if err := bk.PutManifest(ctx, m, map[string]string{"main": fx.CommitSHA}); err != nil {
		return fx, err
	}
	for path, f := range fx.Files {
		entry := m.File(path)
		if err := bk.PutBlob(ctx, hfapi.RepoKindModel, repo, entry.Digest, bytes.NewReader(f.Content), entry.Size); err != nil {
			return fx, fmt.Errorf("seeding blob %s: %w", path, err)
		}
	}
	return fx, nil
}

// SeedHub loads fx into a fakehub and returns the fixture with CommitSHA
// set to the hub's deterministic commit. Serving through Shpiel with this
// hub as upstream exercises the pull-through path.
func SeedHub(hub *fakehub.Hub, fx Fixture) Fixture {
	files := map[string][]byte{}
	var lfsPaths []string
	for p, f := range fx.Files {
		files[p] = f.Content
		if f.LFS {
			lfsPaths = append(lfsPaths, p)
		}
	}
	fx.CommitSHA = hub.AddModel(fx.Repo, files, lfsPaths...)
	return fx
}
