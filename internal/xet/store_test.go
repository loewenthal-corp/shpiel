package xet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewStoreRequiresDir(t *testing.T) {
	t.Parallel()
	if _, err := NewStore(""); err == nil {
		t.Fatal("empty store dir accepted")
	}
}

// TestPutFileShaMappingFailure: a failed sha256-mapping write must fail the
// PutFile, not leave the store silently missing the mapping that powers
// X-Xet-Hash advertisement.
func TestPutFileShaMappingFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	shaDir := filepath.Join(dir, "sha256")
	if err := os.Chmod(shaDir, 0o500); err != nil { //nolint:gosec // dir needs the x bit
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shaDir, 0o700) }) //nolint:gosec // restoring a traversable test dir

	rec := &FileRecord{
		FileHash: strings.Repeat("cd", 32),
		SHA256:   strings.Repeat("ab", 32),
		TotalLen: 1,
	}
	if err := store.PutFile(rec); err == nil {
		t.Fatal("PutFile succeeded with unwritable sha256 dir")
	}

	// Without a sha256 the mapping is skipped and the write succeeds.
	rec.SHA256 = ""
	if err := store.PutFile(rec); err != nil {
		t.Fatalf("PutFile without sha256: %v", err)
	}
	if _, ok := store.FileHashBySHA256(strings.Repeat("ab", 32)); ok {
		t.Fatal("mapping present despite failed write")
	}
}
