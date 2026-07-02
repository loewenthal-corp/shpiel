// Package replication fans writes out to replica backends asynchronously:
// a commit is acknowledged when the primary durably has it, and replicas
// reconcile in the background through a disk-spooled retry queue (spec
// §5.2). No database: jobs are JSON files in a spool directory, so a
// restart resumes exactly where it left off.
//
// Jobs are content-addressed copies — the executor reads the manifest from
// the source backend and copies whatever blobs the target is missing, so
// retries and duplicate jobs are harmless. Jobs for the same (target,
// repo) run in creation order, one at a time, so a newer commit can never
// be overtaken by an older one re-pointing a ref backwards.
package replication

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// Job actions.
const (
	ActionCommit     = "commit"
	ActionDeleteRepo = "delete_repo"
)

// Job is one unit of replication work, durable in the spool.
type Job struct {
	ID        string            `json:"id"`
	Action    string            `json:"action"`
	Kind      hfapi.RepoKind    `json:"kind"`
	Repo      string            `json:"repo"`
	CommitSHA string            `json:"commitSha,omitempty"`
	Refs      map[string]string `json:"refs,omitempty"`
	Source    string            `json:"source"`
	Target    string            `json:"target"`
	CreatedAt time.Time         `json:"createdAt"`

	Attempts  int       `json:"attempts"`
	LastError string    `json:"lastError,omitempty"`
	NextTry   time.Time `json:"nextTry"`
}

// groupKey serializes jobs touching the same repo on the same target.
func (j *Job) groupKey() string {
	return j.Target + "|" + string(j.Kind) + "|" + j.Repo
}

// Options configure the queue.
type Options struct {
	// SpoolDir holds one JSON file per pending job.
	SpoolDir string
	// Backends resolves job source/target names.
	Backends map[string]backend.Backend
	// Workers bounds concurrent job execution (default 4).
	Workers int
	// MaxBackoff caps retry backoff (default 5m).
	MaxBackoff time.Duration
	Log        *slog.Logger
}

// Queue is the replication engine.
type Queue struct {
	dir        string
	backends   map[string]backend.Backend
	workers    int
	maxBackoff time.Duration
	log        *slog.Logger

	mu       sync.Mutex
	jobs     map[string]*Job
	inflight map[string]bool // groupKey -> running
	wake     chan struct{}

	setDepth func(int)
	countJob func(target, outcome string)
}

// New opens (creating if needed) a queue over spoolDir and loads any jobs
// a previous process left behind.
func New(opts Options) (*Queue, error) {
	if opts.SpoolDir == "" {
		return nil, errors.New("replication: spool dir is required")
	}
	if err := os.MkdirAll(opts.SpoolDir, 0o755); err != nil {
		return nil, fmt.Errorf("replication: creating spool dir: %w", err)
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = 4
	}
	maxBackoff := opts.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 5 * time.Minute
	}
	q := &Queue{
		dir:        opts.SpoolDir,
		backends:   opts.Backends,
		workers:    workers,
		maxBackoff: maxBackoff,
		log:        log,
		jobs:       map[string]*Job{},
		inflight:   map[string]bool{},
		wake:       make(chan struct{}, 1),
		setDepth:   func(int) {},
		countJob:   func(string, string) {},
	}
	if err := q.loadSpool(); err != nil {
		return nil, err
	}
	return q, nil
}

// Instrument attaches metric callbacks (depth setter, per-job counter).
func (q *Queue) Instrument(setDepth func(int), countJob func(target, outcome string)) {
	if setDepth != nil {
		q.setDepth = setDepth
	}
	if countJob != nil {
		q.countJob = countJob
	}
	q.publishDepth()
}

func (q *Queue) loadSpool() error {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(q.dir, e.Name()))
		if err != nil {
			continue
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil {
			q.log.Warn("replication: skipping corrupt spool entry", "file", e.Name(), "error", err)
			continue
		}
		// Retry immediately on restart; backoff state restarts too.
		job.NextTry = time.Now()
		q.jobs[job.ID] = &job
	}
	if len(q.jobs) > 0 {
		q.log.Info("replication: recovered spooled jobs", "count", len(q.jobs))
	}
	return nil
}

// EnqueueCommit schedules a commit copy to each target.
func (q *Queue) EnqueueCommit(kind hfapi.RepoKind, repo hfapi.RepoID, source, commitSHA string, refs map[string]string, targets []string) error {
	return q.enqueue(kind, repo, source, targets, func(j *Job) {
		j.Action = ActionCommit
		j.CommitSHA = commitSHA
		j.Refs = refs
	})
}

// EnqueueDeleteRepo schedules repo deletion on each target.
func (q *Queue) EnqueueDeleteRepo(kind hfapi.RepoKind, repo hfapi.RepoID, source string, targets []string) error {
	return q.enqueue(kind, repo, source, targets, func(j *Job) {
		j.Action = ActionDeleteRepo
	})
}

func (q *Queue) enqueue(kind hfapi.RepoKind, repo hfapi.RepoID, source string, targets []string, fill func(*Job)) error {
	if len(targets) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, target := range targets {
		job := &Job{
			ID:        newJobID(),
			Kind:      kind,
			Repo:      repo.String(),
			Source:    source,
			Target:    target,
			CreatedAt: time.Now().UTC(),
			NextTry:   time.Now(),
		}
		fill(job)
		if err := q.persist(job); err != nil {
			return err
		}
		q.jobs[job.ID] = job
	}
	q.publishDepthLocked()
	q.kick()
	return nil
}

func newJobID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

func (q *Queue) persist(job *Job) error {
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(q.dir, job.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("replication: spooling job: %w", err)
	}
	return os.Rename(tmp, path)
}

func (q *Queue) remove(job *Job) {
	if err := os.Remove(filepath.Join(q.dir, job.ID+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		q.log.Warn("replication: removing spooled job", "id", job.ID, "error", err)
	}
}

func (q *Queue) kick() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Run executes jobs until ctx is canceled. Pending jobs stay spooled.
func (q *Queue) Run(ctx context.Context) {
	sem := make(chan struct{}, q.workers)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for {
		job := q.claimDue()
		if job == nil {
			// Nothing runnable: sleep until kicked or the next due time.
			delay := q.nextDelay()
			timer.Reset(delay)
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
			case <-timer.C:
			}
			continue
		}
		select {
		case <-ctx.Done():
			q.unclaim(job)
			return
		case sem <- struct{}{}:
		}
		go func(job *Job) {
			defer func() { <-sem; q.kick() }()
			q.execute(ctx, job)
		}(job)
	}
}

// claimDue picks the oldest due job whose group is idle.
func (q *Queue) claimDue() *Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	var candidates []*Job
	for _, j := range q.jobs {
		if !j.NextTry.After(now) && !q.inflight[j.groupKey()] {
			candidates = append(candidates, j)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, k int) bool { return candidates[i].CreatedAt.Before(candidates[k].CreatedAt) })
	// Groups run their oldest job first; a due-but-younger job in the same
	// group waits its turn.
	job := candidates[0]
	for _, c := range candidates[1:] {
		if c.groupKey() == job.groupKey() && c.CreatedAt.Before(job.CreatedAt) {
			job = c
		}
	}
	q.inflight[job.groupKey()] = true
	return job
}

func (q *Queue) unclaim(job *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inflight, job.groupKey())
}

// nextDelay computes how long until the next job could be due.
func (q *Queue) nextDelay() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	delay := time.Minute
	now := time.Now()
	for _, j := range q.jobs {
		if q.inflight[j.groupKey()] {
			continue
		}
		if d := j.NextTry.Sub(now); d < delay {
			delay = max(d, 10*time.Millisecond)
		}
	}
	return delay
}

func (q *Queue) execute(ctx context.Context, job *Job) {
	err := q.runJob(ctx, job)

	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inflight, job.groupKey())
	if err == nil {
		delete(q.jobs, job.ID)
		q.remove(job)
		q.countJob(job.Target, "ok")
		q.publishDepthLocked()
		q.log.Info("replication: job done",
			"id", job.ID, "action", job.Action, "repo", job.Repo, "target", job.Target, "attempts", job.Attempts+1)
		return
	}
	if ctx.Err() != nil {
		return // shutting down; leave the job spooled untouched
	}
	job.Attempts++
	job.LastError = err.Error()
	backoff := min(time.Duration(1<<min(job.Attempts, 16))*time.Second, q.maxBackoff)
	job.NextTry = time.Now().Add(backoff)
	if perr := q.persist(job); perr != nil {
		q.log.Error("replication: persisting failed job state", "id", job.ID, "error", perr)
	}
	q.countJob(job.Target, "error")
	q.log.Warn("replication: job failed, will retry",
		"id", job.ID, "action", job.Action, "repo", job.Repo, "target", job.Target,
		"attempts", job.Attempts, "backoff", backoff.String(), "error", err)
}

func (q *Queue) runJob(ctx context.Context, job *Job) error {
	src, ok := q.backends[job.Source]
	if !ok {
		return fmt.Errorf("unknown source backend %q", job.Source)
	}
	dst, ok := q.backends[job.Target]
	if !ok {
		return fmt.Errorf("unknown target backend %q", job.Target)
	}
	repo, err := hfapi.ParseRepoID(job.Repo)
	if err != nil {
		return err
	}

	switch job.Action {
	case ActionDeleteRepo:
		if err := dst.DeleteRepo(ctx, job.Kind, repo); err != nil && !errors.Is(err, backend.ErrRepoNotFound) {
			return err
		}
		return nil

	case ActionCommit:
		m, err := src.GetManifest(ctx, job.Kind, repo, job.CommitSHA)
		if err != nil {
			return fmt.Errorf("reading manifest from %s: %w", job.Source, err)
		}
		for i := range m.Files {
			f := &m.Files[i]
			if f.Digest.IsZero() {
				continue
			}
			if _, err := dst.StatBlob(ctx, job.Kind, repo, f.Digest); err == nil {
				continue // content-addressed: already there
			}
			if err := q.copyBlob(ctx, src, dst, job.Kind, repo, f); err != nil {
				return fmt.Errorf("copying %s (%s): %w", f.Path, f.Digest, err)
			}
		}
		if err := dst.PutManifest(ctx, m, job.Refs); err != nil {
			return fmt.Errorf("writing manifest to %s: %w", job.Target, err)
		}
		return nil

	default:
		return fmt.Errorf("unknown action %q", job.Action)
	}
}

func (q *Queue) copyBlob(ctx context.Context, src, dst backend.Backend, kind hfapi.RepoKind, repo hfapi.RepoID, f *backend.FileEntry) error {
	rc, err := src.OpenBlob(ctx, kind, repo, f.Digest)
	if err != nil {
		return fmt.Errorf("opening on %s: %w", src.Name(), err)
	}
	defer func() { _ = rc.Close() }()
	return dst.PutBlob(ctx, kind, repo, f.Digest, rc, f.Size)
}

// RetryNow clears backoff so every pending job is immediately due (admin
// "kick" endpoint).
func (q *Queue) RetryNow() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	n := 0
	for _, j := range q.jobs {
		if j.NextTry.After(now) {
			j.NextTry = now
			n++
		}
	}
	q.kick()
	return n
}

// Snapshot returns pending jobs for the admin API, oldest first.
func (q *Queue) Snapshot() []Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Job, 0, len(q.jobs))
	for _, j := range q.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.Before(out[k].CreatedAt) })
	return out
}

// Depth returns the number of pending jobs.
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

func (q *Queue) publishDepth() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.publishDepthLocked()
}

func (q *Queue) publishDepthLocked() {
	q.setDepth(len(q.jobs))
}
