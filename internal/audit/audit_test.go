package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenDisabled(t *testing.T) {
	t.Parallel()
	l, err := Open("")
	if err != nil || l != nil {
		t.Fatalf("Open(\"\") = %v, %v; want nil, nil", l, err)
	}
	// The nil logger is safe to use.
	l.Record(Event{Action: "noop"})
	if err := l.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestOpenStderr(t *testing.T) {
	t.Parallel()
	l, err := Open("-")
	if err != nil || l == nil {
		t.Fatalf("Open(\"-\") = %v, %v", l, err)
	}
	// No closer for stderr.
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOpenFileError(t *testing.T) {
	t.Parallel()
	if _, err := Open(filepath.Join(t.TempDir(), "no", "such", "dir", "audit.log")); err == nil {
		t.Fatal("unopenable path accepted")
	}
}

// TestRecordWritesJSONLines: records append as JSON lines carrying exactly
// the non-empty fields, with anonymous defaulting.
func TestRecordWritesJSONLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	l.Record(Event{
		Action: "commit", Actor: "alice", Repo: "org/x",
		Revision: "main", Commit: "abc123", Digest: "sha256:ff",
		Detail: map[string]any{"files": 3},
	})
	l.Record(Event{Action: "repo_create", Repo: "org/y"})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d: %s", len(lines), data)
	}

	var full map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &full); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"action": "commit", "actor": "alice", "repo": "org/x",
		"revision": "main", "commit": "abc123", "digest": "sha256:ff",
		"files": float64(3), "msg": "audit",
	} {
		if full[k] != want {
			t.Errorf("record[%q] = %v, want %v", k, full[k], want)
		}
	}
	if _, ok := full["ts"]; !ok {
		t.Error("record has no timestamp")
	}

	var minimal map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &minimal); err != nil {
		t.Fatal(err)
	}
	if minimal["actor"] != "anonymous" {
		t.Errorf("empty actor = %v, want anonymous", minimal["actor"])
	}
	// Empty optional fields stay out of the record entirely.
	for _, k := range []string{"revision", "commit", "digest"} {
		if _, present := minimal[k]; present {
			t.Errorf("empty field %q serialized", k)
		}
	}
}

// TestRecordAppends: reopening the same file appends rather than
// truncating — the audit trail is append-only.
func TestRecordAppends(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	for i := range 2 {
		l, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		l.Record(Event{Action: "run", Detail: map[string]any{"n": i}})
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	if got := strings.Count(string(data), "\"action\":\"run\""); got != 2 {
		t.Fatalf("appended records = %d, want 2 (%s)", got, data)
	}
}
