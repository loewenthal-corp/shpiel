package replication

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// recordingHandler captures slog records for asserting on log-side effects
// (recovery notices, retry accounting).
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// find returns the first record with the given message, if any.
func (h *recordingHandler) find(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

func intAttr(r slog.Record, key string) (int64, bool) {
	var v int64
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.Int64()
			found = true
			return false
		}
		return true
	})
	return v, found
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	q, err := New(Options{SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if q.workers != 4 {
		t.Errorf("default workers = %d, want 4", q.workers)
	}
	if q.maxBackoff != 5*time.Minute {
		t.Errorf("default maxBackoff = %v, want 5m", q.maxBackoff)
	}
	q, err = New(Options{SpoolDir: t.TempDir(), Workers: 2, MaxBackoff: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if q.workers != 2 || q.maxBackoff != 30*time.Second {
		t.Errorf("explicit options not kept: workers=%d maxBackoff=%v", q.workers, q.maxBackoff)
	}
	if _, err := New(Options{}); err == nil {
		t.Error("missing spool dir accepted")
	}
}

// enqueueTestJob puts one due commit job in the queue and returns it.
func enqueueTestJob(t *testing.T, q *Queue, repo, target string) *Job {
	t.Helper()
	id, _ := hfapi.ParseRepoID(repo)
	if err := q.EnqueueCommit(hfapi.RepoKindModel, id, "src", commitA, nil, []string{target}); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.Repo == repo && j.Target == target {
			return j
		}
	}
	t.Fatal("job not found after enqueue")
	return nil
}

func TestNextDelay(t *testing.T) {
	t.Parallel()
	q, err := New(Options{SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// Idle queues sleep the full minute.
	if d := q.nextDelay(); d != time.Minute {
		t.Errorf("idle delay = %v, want 1m", d)
	}

	job := enqueueTestJob(t, q, "org/a", "dst")
	q.mu.Lock()
	job.NextTry = time.Now().Add(5 * time.Second)
	q.mu.Unlock()
	if d := q.nextDelay(); d < 4*time.Second || d > 5*time.Second {
		t.Errorf("delay for job due in 5s = %v", d)
	}

	// Overdue jobs clamp to the 10ms floor, never negative.
	q.mu.Lock()
	job.NextTry = time.Now().Add(-time.Hour)
	q.mu.Unlock()
	if d := q.nextDelay(); d != 10*time.Millisecond {
		t.Errorf("overdue delay = %v, want 10ms", d)
	}

	// Inflight groups are ignored: nothing else pending means a full sleep.
	q.mu.Lock()
	q.inflight[job.groupKey()] = true
	q.mu.Unlock()
	if d := q.nextDelay(); d != time.Minute {
		t.Errorf("delay with only inflight jobs = %v, want 1m", d)
	}
}

func TestClaimDueOrdersAndSerializesGroups(t *testing.T) {
	t.Parallel()
	q, err := New(Options{SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	older := enqueueTestJob(t, q, "org/same", "dst")
	younger := enqueueTestJob(t, q, "org/same", "dst")
	q.mu.Lock()
	older.CreatedAt = time.Now().Add(-2 * time.Second)
	younger.CreatedAt = time.Now().Add(-1 * time.Second)
	now := time.Now()
	older.NextTry, younger.NextTry = now, now
	q.mu.Unlock()

	got := q.claimDue()
	if got == nil || got.ID != older.ID {
		t.Fatalf("claimed %+v, want the older job %s", got, older.ID)
	}
	// The group is now busy: its younger job must wait.
	if second := q.claimDue(); second != nil {
		t.Fatalf("claimed %s while group inflight", second.ID)
	}
	q.unclaim(got)
	if second := q.claimDue(); second == nil || second.ID != older.ID {
		t.Fatalf("after unclaim claimed %+v, want oldest again", second)
	}
}

func TestRetryNowClearsBackoff(t *testing.T) {
	t.Parallel()
	q, err := New(Options{SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	j1 := enqueueTestJob(t, q, "org/a", "dst")
	j2 := enqueueTestJob(t, q, "org/b", "dst")
	j3 := enqueueTestJob(t, q, "org/c", "dst")
	q.mu.Lock()
	j1.NextTry = time.Now().Add(time.Hour)
	j2.NextTry = time.Now().Add(time.Hour)
	j3.NextTry = time.Now().Add(-time.Second) // already due; not counted
	q.mu.Unlock()

	if n := q.RetryNow(); n != 2 {
		t.Fatalf("RetryNow = %d, want 2", n)
	}
	now := time.Now()
	for _, j := range q.Snapshot() {
		if j.NextTry.After(now) {
			t.Fatalf("job %s still backed off until %v", j.ID, j.NextTry)
		}
	}
}

// failingBackend errors every state-changing call.
type failingBackend struct {
	backend.Backend
	name string
}

var errBoom = errors.New("boom")

func (f *failingBackend) Name() string { return f.name }
func (f *failingBackend) GetManifest(context.Context, hfapi.RepoKind, hfapi.RepoID, string) (*backend.Manifest, error) {
	return nil, errBoom
}
func (f *failingBackend) DeleteRepo(context.Context, hfapi.RepoKind, hfapi.RepoID) error {
	return errBoom
}

// TestExecuteBackoffAndLogging drives execute directly: failures back off
// exponentially and stay spooled; success removes the job and logs the
// human attempt count.
func TestExecuteBackoffAndLogging(t *testing.T) {
	t.Parallel()
	rec := &recordingHandler{}
	src, _ := fsbackend.New("src", t.TempDir())
	dst, _ := fsbackend.New("dst", t.TempDir())
	q, err := New(Options{
		SpoolDir: t.TempDir(),
		Backends: map[string]backend.Backend{
			"src":  src,
			"dst":  dst,
			"dead": &failingBackend{name: "dead"},
		},
		Log: slog.New(rec),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Failing job: attempts increment, backoff lands in (1s, maxBackoff].
	fail := enqueueTestJob(t, q, "org/fail", "dead")
	before := time.Now()
	q.execute(ctx, fail)
	if fail.Attempts != 1 || fail.LastError == "" {
		t.Fatalf("after failure: attempts=%d lastError=%q", fail.Attempts, fail.LastError)
	}
	if got := fail.NextTry.Sub(before); got < time.Second || got > 3*time.Second {
		t.Fatalf("first backoff = %v, want ~2s", got)
	}
	q.execute(ctx, fail)
	if got := time.Until(fail.NextTry); got < 3*time.Second {
		t.Fatalf("second backoff ends in %v, want ~4s away", got)
	}
	if q.Depth() != 1 {
		t.Fatalf("failed job dropped from queue (depth %d)", q.Depth())
	}
	// The spool write succeeded, so no persistence error was logged.
	if r, found := rec.find("replication: persisting failed job state"); found {
		t.Fatalf("unexpected persistence-error log: %v", r)
	}

	// Successful job: removed, spool file gone, "attempts" logs 1 (human
	// count), not 0 or -1.
	seedCommit(t, src, "org/ok", commitA, map[string][]byte{"a": []byte("x")}, map[string]string{"main": commitA})
	ok := enqueueTestJob(t, q, "org/ok", "dst")
	q.execute(ctx, ok)
	if q.Depth() != 1 { // only the failing job remains
		t.Fatalf("depth after success = %d, want 1", q.Depth())
	}
	if _, err := os.Stat(filepath.Join(q.dir, ok.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spool file for done job still present (err=%v)", err)
	}
	r, found := rec.find("replication: job done")
	if !found {
		t.Fatal("no job-done log")
	}
	if n, ok := intAttr(r, "attempts"); !ok || n != 1 {
		t.Fatalf("job-done attempts attr = %d (present=%v), want 1", n, ok)
	}
}

// TestDeleteRepoTolerance: deleting a repo the target never had is
// success; real backend failures are not.
func TestDeleteRepoTolerance(t *testing.T) {
	t.Parallel()
	src, _ := fsbackend.New("src", t.TempDir())
	dst, _ := fsbackend.New("dst", t.TempDir())
	q, err := New(Options{
		SpoolDir: t.TempDir(),
		Backends: map[string]backend.Backend{
			"src":  src,
			"dst":  dst,
			"dead": &failingBackend{name: "dead"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/ghost")

	job := &Job{Action: ActionDeleteRepo, Kind: hfapi.RepoKindModel, Repo: repo.String(), Source: "src", Target: "dst"}
	if err := q.runJob(ctx, job); err != nil {
		t.Fatalf("delete of absent repo = %v, want success", err)
	}
	job.Target = "dead"
	if err := q.runJob(ctx, job); !errors.Is(err, errBoom) {
		t.Fatalf("delete on broken backend = %v, want errBoom", err)
	}

	// Unknown names and actions fail loudly.
	if err := q.runJob(ctx, &Job{Action: ActionDeleteRepo, Repo: "org/x", Source: "nope", Target: "dst"}); err == nil {
		t.Error("unknown source accepted")
	}
	if err := q.runJob(ctx, &Job{Action: ActionDeleteRepo, Repo: "org/x", Source: "src", Target: "nope"}); err == nil {
		t.Error("unknown target accepted")
	}
	if err := q.runJob(ctx, &Job{Action: "explode", Repo: "org/x", Source: "src", Target: "dst"}); err == nil {
		t.Error("unknown action accepted")
	}
}

// TestSpoolRecoveryLogging: recovering jobs logs a count; a clean start
// logs nothing.
func TestSpoolRecoveryLogging(t *testing.T) {
	t.Parallel()
	spool := t.TempDir()
	q1, err := New(Options{SpoolDir: spool})
	if err != nil {
		t.Fatal(err)
	}
	enqueueTestJob(t, q1, "org/spooled", "dst")

	rec := &recordingHandler{}
	if _, err := New(Options{SpoolDir: spool, Log: slog.New(rec)}); err != nil {
		t.Fatal(err)
	}
	r, found := rec.find("replication: recovered spooled jobs")
	if !found {
		t.Fatal("no recovery log for non-empty spool")
	}
	if n, ok := intAttr(r, "count"); !ok || n != 1 {
		t.Fatalf("recovery count = %d, want 1", n)
	}

	empty := &recordingHandler{}
	if _, err := New(Options{SpoolDir: t.TempDir(), Log: slog.New(empty)}); err != nil {
		t.Fatal(err)
	}
	if _, found := empty.find("replication: recovered spooled jobs"); found {
		t.Fatal("recovery logged for empty spool")
	}
}

// TestRemoveSpoolFile: removing an already-gone spool entry is quiet;
// stranger failures warn.
func TestRemoveSpoolFile(t *testing.T) {
	t.Parallel()
	rec := &recordingHandler{}
	q, err := New(Options{SpoolDir: t.TempDir(), Log: slog.New(rec)})
	if err != nil {
		t.Fatal(err)
	}
	q.remove(&Job{ID: "never-existed"})
	if _, found := rec.find("replication: removing spooled job"); found {
		t.Fatal("warned about removing a missing spool file")
	}

	// A non-empty directory in the spool file's place cannot be removed.
	blocked := filepath.Join(q.dir, "blocked.json")
	if err := os.MkdirAll(filepath.Join(blocked, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	q.remove(&Job{ID: "blocked"})
	if _, found := rec.find("replication: removing spooled job"); !found {
		t.Fatal("no warning for a genuinely failed removal")
	}
}
