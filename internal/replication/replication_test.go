package replication

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

const commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const commitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// seedCommit writes a commit (manifest + blobs + ref) into a backend.
func seedCommit(t *testing.T, bk backend.Backend, repo string, commit string, files map[string][]byte, refs map[string]string) *backend.Manifest {
	t.Helper()
	id, _ := hfapi.ParseRepoID(repo)
	m := &backend.Manifest{Repo: id, Kind: hfapi.RepoKindModel, CommitSHA: commit, FetchedAt: time.Now()}
	ctx := context.Background()
	for path, content := range files {
		d := backend.SHA256Digest(fakehub.SHA256Hex(content))
		m.Files = append(m.Files, backend.FileEntry{Path: path, Size: int64(len(content)), Digest: d, OID: fakehub.GitBlobOID(content)})
		if err := bk.PutBlob(ctx, hfapi.RepoKindModel, id, d, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatal(err)
		}
	}
	if err := bk.PutManifest(ctx, m, refs); err != nil {
		t.Fatal(err)
	}
	return m
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestReplicatesCommitToTarget(t *testing.T) {
	t.Parallel()
	primary, _ := fsbackend.New("primary", t.TempDir())
	replica, _ := fsbackend.New("replica", t.TempDir())

	q, err := New(Options{
		SpoolDir: t.TempDir(),
		Backends: map[string]backend.Backend{"primary": primary, "replica": replica},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	files := map[string][]byte{
		"config.json":       []byte(`{"a":1}`),
		"model.safetensors": bytes.Repeat([]byte{5}, 4096),
	}
	seedCommit(t, primary, "org/rep", commitA, files, map[string]string{"main": commitA})
	repo, _ := hfapi.ParseRepoID("org/rep")

	if err := q.EnqueueCommit(hfapi.RepoKindModel, repo, "primary", commitA, map[string]string{"main": commitA}, []string{"replica"}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 5*time.Second, func() bool {
		sha, err := replica.ResolveRef(context.Background(), hfapi.RepoKindModel, repo, "main")
		return err == nil && sha == commitA
	}, "replica ref")

	// Blobs and manifest arrived intact.
	m, err := replica.GetManifest(context.Background(), hfapi.RepoKindModel, repo, commitA)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m.Files {
		rc, err := replica.OpenBlob(context.Background(), hfapi.RepoKindModel, repo, f.Digest)
		if err != nil {
			t.Fatalf("replica missing blob %s: %v", f.Digest, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, files[f.Path]) {
			t.Fatalf("replica blob %s content mismatch", f.Path)
		}
	}
	if q.Depth() != 0 {
		t.Fatalf("queue depth = %d, want 0", q.Depth())
	}
}

// flakyBackend fails writes until healed.
type flakyBackend struct {
	backend.Backend
	down atomic.Bool
}

var errDown = errors.New("backend down")

func (f *flakyBackend) PutManifest(ctx context.Context, m *backend.Manifest, refs map[string]string) error {
	if f.down.Load() {
		return errDown
	}
	return f.Backend.PutManifest(ctx, m, refs)
}

func (f *flakyBackend) PutBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, d backend.Digest, r io.Reader, size int64) error {
	if f.down.Load() {
		return errDown
	}
	return f.Backend.PutBlob(ctx, kind, repo, d, r, size)
}

func TestRetriesUntilReplicaHeals(t *testing.T) {
	t.Parallel()
	primary, _ := fsbackend.New("primary", t.TempDir())
	inner, _ := fsbackend.New("replica", t.TempDir())
	replica := &flakyBackend{Backend: inner}
	replica.down.Store(true)

	q, err := New(Options{
		SpoolDir:   t.TempDir(),
		Backends:   map[string]backend.Backend{"primary": primary, "replica": replica},
		MaxBackoff: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	seedCommit(t, primary, "org/heal", commitA, map[string][]byte{"a": []byte("x")}, map[string]string{"main": commitA})
	repo, _ := hfapi.ParseRepoID("org/heal")
	if err := q.EnqueueCommit(hfapi.RepoKindModel, repo, "primary", commitA, map[string]string{"main": commitA}, []string{"replica"}); err != nil {
		t.Fatal(err)
	}

	// It fails at least once and stays queued with an error recorded.
	waitFor(t, 5*time.Second, func() bool {
		jobs := q.Snapshot()
		return len(jobs) == 1 && jobs[0].Attempts > 0 && jobs[0].LastError != ""
	}, "failed attempt recorded")

	replica.down.Store(false)
	q.RetryNow()

	waitFor(t, 5*time.Second, func() bool {
		sha, err := inner.ResolveRef(context.Background(), hfapi.RepoKindModel, repo, "main")
		return err == nil && sha == commitA && q.Depth() == 0
	}, "replication after heal")
}

func TestSpoolSurvivesRestart(t *testing.T) {
	t.Parallel()
	spool := t.TempDir()
	primary, _ := fsbackend.New("primary", t.TempDir())
	replica, _ := fsbackend.New("replica", t.TempDir())
	backends := map[string]backend.Backend{"primary": primary, "replica": replica}

	// First process: enqueue but never run.
	q1, err := New(Options{SpoolDir: spool, Backends: backends})
	if err != nil {
		t.Fatal(err)
	}
	seedCommit(t, primary, "org/restart", commitA, map[string][]byte{"a": []byte("x")}, map[string]string{"main": commitA})
	repo, _ := hfapi.ParseRepoID("org/restart")
	if err := q1.EnqueueCommit(hfapi.RepoKindModel, repo, "primary", commitA, map[string]string{"main": commitA}, []string{"replica"}); err != nil {
		t.Fatal(err)
	}

	// Second process over the same spool picks the job up and runs it.
	q2, err := New(Options{SpoolDir: spool, Backends: backends})
	if err != nil {
		t.Fatal(err)
	}
	if q2.Depth() != 1 {
		t.Fatalf("recovered depth = %d, want 1", q2.Depth())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q2.Run(ctx)

	waitFor(t, 5*time.Second, func() bool {
		sha, err := replica.ResolveRef(context.Background(), hfapi.RepoKindModel, repo, "main")
		return err == nil && sha == commitA
	}, "replication after restart")
}

func TestSequentialCommitsLandInOrder(t *testing.T) {
	t.Parallel()
	primary, _ := fsbackend.New("primary", t.TempDir())
	replica, _ := fsbackend.New("replica", t.TempDir())
	q, err := New(Options{
		SpoolDir: t.TempDir(),
		Backends: map[string]backend.Backend{"primary": primary, "replica": replica},
	})
	if err != nil {
		t.Fatal(err)
	}

	repo, _ := hfapi.ParseRepoID("org/order")
	seedCommit(t, primary, "org/order", commitA, map[string][]byte{"a": []byte("v1")}, map[string]string{"main": commitA})
	seedCommit(t, primary, "org/order", commitB, map[string][]byte{"a": []byte("v2")}, map[string]string{"main": commitB})

	// Enqueue both before starting workers: they must apply oldest-first,
	// leaving the replica's main at commitB.
	if err := q.EnqueueCommit(hfapi.RepoKindModel, repo, "primary", commitA, map[string]string{"main": commitA}, []string{"replica"}); err != nil {
		t.Fatal(err)
	}
	if err := q.EnqueueCommit(hfapi.RepoKindModel, repo, "primary", commitB, map[string]string{"main": commitB}, []string{"replica"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	waitFor(t, 5*time.Second, func() bool { return q.Depth() == 0 }, "queue drain")
	sha, err := replica.ResolveRef(context.Background(), hfapi.RepoKindModel, repo, "main")
	if err != nil || sha != commitB {
		t.Fatalf("replica main = %s, %v; want %s", sha, err, commitB)
	}
}

func TestDeleteReplication(t *testing.T) {
	t.Parallel()
	primary, _ := fsbackend.New("primary", t.TempDir())
	replica, _ := fsbackend.New("replica", t.TempDir())
	q, err := New(Options{
		SpoolDir: t.TempDir(),
		Backends: map[string]backend.Backend{"primary": primary, "replica": replica},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	repo, _ := hfapi.ParseRepoID("org/gone")
	seedCommit(t, replica, "org/gone", commitA, map[string][]byte{"a": []byte("x")}, map[string]string{"main": commitA})

	if err := q.EnqueueDeleteRepo(hfapi.RepoKindModel, repo, "primary", []string{"replica"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, err := replica.ResolveRef(context.Background(), hfapi.RepoKindModel, repo, "main")
		return errors.Is(err, backend.ErrRepoNotFound) && q.Depth() == 0
	}, "replica deletion")
}
