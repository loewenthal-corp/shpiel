package xet

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/fakes3"
	"github.com/loewenthal-corp/shpiel/internal/s3client"
)

// storeVariants returns every persistence layer a Store can run on: the
// local directory store and the bucket store on a SigV4-verified fakes3.
// The store contract tests run identically against each.
func storeVariants(t *testing.T) map[string]*Store {
	t.Helper()
	disk, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*Store{
		"disk":   disk,
		"bucket": newBucketTestStore(t, "xet"),
	}
}

// newBucketTestStore builds a bucket store over a credentialed fakes3.
func newBucketTestStore(t *testing.T, prefix string) *Store {
	t.Helper()
	store, _ := newBucketTestStoreWithFake(t, prefix)
	return store
}

func newBucketTestStoreWithFake(t *testing.T, prefix string) (*Store, *fakes3.Server) {
	t.Helper()
	fake := fakes3.New("xorb-bucket", "AKIDXETSTORE", "xet-store-secret")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client, err := s3client.New(s3client.Options{
		Endpoint: srv.URL,
		Bucket:   "xorb-bucket",
		Region:   "us-east-1",
		Credentials: s3client.Credentials{
			AccessKeyID:     "AKIDXETSTORE",
			SecretAccessKey: "xet-store-secret",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewBucketStore(client, prefix), fake
}

func TestNewStoreRequiresDir(t *testing.T) {
	t.Parallel()
	if _, err := NewStore(""); err == nil {
		t.Fatal("empty store dir accepted")
	}
}

// TestStoreMissingObjects pins the not-found contract on every
// persistence layer.
func TestStoreMissingObjects(t *testing.T) {
	t.Parallel()
	for name, store := range storeVariants(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			var h Hash
			h[0] = 0x42
			if store.HasXorb(ctx, h) {
				t.Error("HasXorb on empty store")
			}
			if _, err := store.ReadXorb(ctx, h); !errors.Is(err, ErrNotFound) {
				t.Errorf("ReadXorb = %v, want ErrNotFound", err)
			}
			if _, err := store.XorbChunks(ctx, h); !errors.Is(err, ErrNotFound) {
				t.Errorf("XorbChunks = %v, want ErrNotFound", err)
			}
			if _, err := store.OpenXorb(ctx, h); !errors.Is(err, ErrNotFound) {
				t.Errorf("OpenXorb = %v, want ErrNotFound", err)
			}
			if _, err := store.File(ctx, h); !errors.Is(err, ErrNotFound) {
				t.Errorf("File = %v, want ErrNotFound", err)
			}
			if _, ok := store.FileHashBySHA256(ctx, strings.Repeat("ab", 32)); ok {
				t.Error("FileHashBySHA256 hit on empty store")
			}
		})
	}
}

// TestSHA256InputsValidated: the sha256 strings flowing into storage keys
// come from manifest metadata and shards, so anything but 64-hex must be
// rejected before it can shape a key (path traversal on the disk store).
func TestSHA256InputsValidated(t *testing.T) {
	t.Parallel()
	for name, store := range storeVariants(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			for _, bad := range []string{"../../../../etc/passwd", "abc", "", strings.Repeat("zz", 32), strings.Repeat("ab", 33)} {
				if _, ok := store.FileHashBySHA256(ctx, bad); ok {
					t.Errorf("FileHashBySHA256(%q) hit", bad)
				}
				if bad == "" {
					continue // an absent sha256 is legal on records
				}
				rec := &FileRecord{FileHash: strings.Repeat("cd", 32), SHA256: bad, TotalLen: 1}
				if err := store.PutFile(ctx, rec); err == nil {
					t.Errorf("PutFile accepted sha256 %q", bad)
				}
			}
			// Uppercase hex is a legal spelling of the same address.
			rec := &FileRecord{FileHash: strings.Repeat("cd", 32), SHA256: strings.Repeat("AB", 32), TotalLen: 1}
			if err := store.PutFile(ctx, rec); err != nil {
				t.Errorf("PutFile rejected uppercase sha256: %v", err)
			}
			if _, ok := store.FileHashBySHA256(ctx, strings.Repeat("ab", 32)); !ok {
				t.Error("lowercase lookup missed uppercase-stored mapping")
			}
		})
	}
}

// TestBucketStoreLayout pins the bucket key scheme (prefix + the same
// keys the disk store uses) and ranged xorb serving straight off the
// bucket.
func TestBucketStoreLayout(t *testing.T) {
	t.Parallel()
	store, fake := newBucketTestStoreWithFake(t, "team-a/xet/")
	ctx := context.Background()

	content := []byte("chunky xorb bytes")
	xorb := buildChunk(t, content, compressionNone)
	var h Hash
	h[0] = 0xEE
	if _, err := store.PutXorb(ctx, h, xorb); err != nil {
		t.Fatalf("PutXorb: %v", err)
	}
	rec := &FileRecord{FileHash: h.Hex(), SHA256: strings.Repeat("55", 32), TotalLen: int64(len(content))}
	if err := store.PutFile(ctx, rec); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	want := []string{
		"team-a/xet/files/" + h.Hex() + ".json",
		"team-a/xet/sha256/" + strings.Repeat("55", 32),
		"team-a/xet/xorbs/" + h.Hex(),
		"team-a/xet/xorbs/" + h.Hex() + ".chunks.json",
	}
	if got := strings.Join(fake.Keys(), ","); got != strings.Join(want, ",") {
		t.Errorf("bucket keys = %v, want %v", fake.Keys(), want)
	}

	// OpenXorb serves ranges without buffering the xorb.
	r, err := store.OpenXorb(ctx, h)
	if err != nil {
		t.Fatalf("OpenXorb: %v", err)
	}
	defer r.Close()
	if _, err := r.Seek(8, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(r)
	if err != nil || string(tail) != string(xorb[8:]) {
		t.Errorf("ranged xorb read = %q, %v", tail, err)
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
	if err := store.PutFile(context.Background(), rec); err == nil {
		t.Fatal("PutFile succeeded with unwritable sha256 dir")
	}

	// Without a sha256 the mapping is skipped and the write succeeds.
	rec.SHA256 = ""
	if err := store.PutFile(context.Background(), rec); err != nil {
		t.Fatalf("PutFile without sha256: %v", err)
	}
	if _, ok := store.FileHashBySHA256(context.Background(), strings.Repeat("ab", 32)); ok {
		t.Fatal("mapping present despite failed write")
	}
}
