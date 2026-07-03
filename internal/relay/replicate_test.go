package relay

import (
	"bytes"
	"context"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// recordingReplicator captures enqueue calls.
type recordingReplicator struct {
	mu      sync.Mutex
	commits [][]string // targets per EnqueueCommit
	deletes [][]string
}

func (r *recordingReplicator) EnqueueCommit(kind hfapi.RepoKind, repo hfapi.RepoID, source, commitSHA string, refs map[string]string, targets []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commits = append(r.commits, targets)
	return nil
}

func (r *recordingReplicator) EnqueueDeleteRepo(kind hfapi.RepoKind, repo hfapi.RepoID, source string, targets []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes = append(r.deletes, targets)
	return nil
}

// newReplicaRelay wires a relay whose route fans out to one replica.
func newReplicaRelay(t *testing.T, rep Replicator, withReplica bool) *Relay {
	t.Helper()
	primary, err := fsbackend.New("primary", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	replica, err := fsbackend.New("replica", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	route := config.Route{Match: "*", Primary: "primary"}
	if withReplica {
		route.Replicas = []string{"replica"}
	}
	router, err := NewRouter([]config.Route{route},
		map[string]backend.Backend{"primary": primary, "replica": replica})
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{Router: router, Replicator: rep})
}

// TestReplicationFanOut: creates, commits, and deletes on a replicated
// route enqueue exactly one job each for the replica.
func TestReplicationFanOut(t *testing.T) {
	t.Parallel()
	rec := &recordingReplicator{}
	rl := newReplicaRelay(t, rec, true)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/fan")

	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	created := len(rec.commits)
	rec.mu.Unlock()
	if created != 1 || rec.commits[0][0] != "replica" {
		t.Fatalf("commits after create = %+v, want one for the replica", rec.commits)
	}

	if _, err := rl.Commit(ctx, hfapi.RepoKindModel, repo, "main", &CommitOps{
		Summary: "add",
		Files:   []CommitOpFile{{Path: "a.txt", Content: []byte("x")}},
	}); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	committed := len(rec.commits)
	rec.mu.Unlock()
	if committed != 2 {
		t.Fatalf("commits after commit = %d, want 2", committed)
	}

	if err := rl.DeleteRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	deletes := rec.deletes
	rec.mu.Unlock()
	if len(deletes) != 1 || deletes[0][0] != "replica" {
		t.Fatalf("deletes = %+v, want one for the replica", deletes)
	}
}

// TestNoFanOutWithoutReplicas: a replicator wired to a replica-less route
// must never be called.
func TestNoFanOutWithoutReplicas(t *testing.T) {
	t.Parallel()
	rec := &recordingReplicator{}
	rl := newReplicaRelay(t, rec, false)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/solo")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	if err := rl.DeleteRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.commits) != 0 || len(rec.deletes) != 0 {
		t.Fatalf("replicator called on replica-less route: %+v %+v", rec.commits, rec.deletes)
	}
}

// TestNilReplicatorWithReplicas: replicas without a queue (queue failed to
// build, or tests) must not panic writes.
func TestNilReplicatorWithReplicas(t *testing.T) {
	t.Parallel()
	rl := newReplicaRelay(t, nil, true)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/norep")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	if err := rl.DeleteRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
}

// TestPutLFSBlobVerifiesDigest: content that does not hash to the declared
// oid must be rejected, not acknowledged.
func TestPutLFSBlobVerifiesDigest(t *testing.T) {
	t.Parallel()
	rl := newWriteRelay(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/lfsverify")
	content := []byte("actual bytes")
	wrongOID := fakehub.SHA256Hex([]byte("declared other bytes"))
	err := rl.PutLFSBlob(ctx, hfapi.RepoKindModel, repo, wrongOID, int64(len(content)), bytes.NewReader(content))
	if err == nil {
		t.Fatal("digest-mismatched LFS upload accepted")
	}
}

// TestCommitManifestSortedByPath: manifests list files in ascending path
// order regardless of commit payload order (stable commit SHAs depend on
// it).
func TestCommitManifestSortedByPath(t *testing.T) {
	t.Parallel()
	rl := newWriteRelay(t)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/sorted")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := rl.Commit(ctx, hfapi.RepoKindModel, repo, "main", &CommitOps{
		Summary: "unsorted",
		Files: []CommitOpFile{
			{Path: "zebra.txt", Content: []byte("z")},
			{Path: "alpha.txt", Content: []byte("a")},
			{Path: "midway/nested.txt", Content: []byte("m")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := rl.ResolveManifest(ctx, hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(m.Files))
	for i, f := range m.Files {
		paths[i] = f.Path
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("manifest paths not sorted: %v", paths)
	}
	if len(paths) != 3 || paths[0] != "alpha.txt" {
		t.Fatalf("paths = %v", paths)
	}
}

// TestStale pins the refresh policy: no interval means always revalidate;
// with an interval, only manifests older than it are stale.
func TestStale(t *testing.T) {
	t.Parallel()
	fresh := &backend.Manifest{FetchedAt: time.Now()}
	old := &backend.Manifest{FetchedAt: time.Now().Add(-2 * time.Hour)}
	for _, tc := range []struct {
		name     string
		interval time.Duration
		m        *backend.Manifest
		want     bool
	}{
		{"zero interval fresh", 0, fresh, true},
		{"negative interval fresh", -time.Second, fresh, true},
		{"within interval", time.Hour, fresh, false},
		{"beyond interval", time.Hour, old, true},
	} {
		r := &Relay{refreshInterval: tc.interval}
		if got := r.stale(tc.m); got != tc.want {
			t.Errorf("%s: stale = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFanOutEnqueueFailureLogged: a failing enqueue is logged, not
// returned — the primary write already succeeded.
type failingReplicator struct{}

func (failingReplicator) EnqueueCommit(hfapi.RepoKind, hfapi.RepoID, string, string, map[string]string, []string) error {
	return errEnqueue
}
func (failingReplicator) EnqueueDeleteRepo(hfapi.RepoKind, hfapi.RepoID, string, []string) error {
	return errEnqueue
}

var errEnqueue = &enqueueError{}

type enqueueError struct{}

func (*enqueueError) Error() string { return "spool full" }

func TestFanOutEnqueueFailureDoesNotFailWrite(t *testing.T) {
	t.Parallel()
	rl := newReplicaRelay(t, failingReplicator{}, true)
	rec := &recordingLogHandler{}
	rl.log = slog.New(rec)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/spoolfull")
	if err := rl.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatalf("CreateRepo failed on enqueue error: %v", err)
	}
	if err := rl.DeleteRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatalf("DeleteRepo failed on enqueue error: %v", err)
	}
	// The failures are logged so operators can see the replicas fall
	// behind.
	if !rec.has("enqueueing replication failed") || !rec.has("enqueueing replication delete failed") {
		t.Fatalf("enqueue failures not logged: %v", rec.messages())
	}

	// A healthy replicator logs no such errors.
	quiet := &recordingLogHandler{}
	ok := newReplicaRelay(t, &recordingReplicator{}, true)
	ok.log = slog.New(quiet)
	repo2, _ := hfapi.ParseRepoID("org/quiet")
	if err := ok.CreateRepo(ctx, hfapi.RepoKindModel, repo2); err != nil {
		t.Fatal(err)
	}
	if quiet.has("enqueueing replication failed") {
		t.Fatal("successful enqueue logged as failure")
	}
}

// recordingLogHandler captures log messages for asserting on log-only
// behavior.
type recordingLogHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingLogHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingLogHandler) has(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

func (h *recordingLogHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.msgs...)
}
