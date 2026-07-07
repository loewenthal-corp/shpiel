package s3backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/fakes3"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

const commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// newTestBackend runs a credentialed fakes3, so every backend operation
// in these tests also exercises the SigV4 signer/verifier pair.
func newTestBackend(t *testing.T, prefix string) (*Backend, *fakes3.Server) {
	t.Helper()
	fake := fakes3.New("models-bucket", "AKIDTEST", "secret")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	b, err := New("test", Options{
		Endpoint:        srv.URL,
		Bucket:          "models-bucket",
		Region:          "us-east-1",
		Prefix:          prefix,
		AccessKeyID:     "AKIDTEST",
		SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b, fake
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
	b, _ := newTestBackend(t, "")
	ctx := context.Background()
	m := testManifest("org/repo", map[string][]byte{"config.json": []byte(`{}`)})

	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA, "refs/pr/1": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}

	for _, ref := range []string{"main", "refs/pr/1", commitA} {
		sha, err := b.ResolveRef(ctx, hfapi.RepoKindModel, m.Repo, ref)
		if err != nil {
			t.Fatalf("ResolveRef(%s): %v", ref, err)
		}
		if sha != commitA {
			t.Fatalf("ResolveRef(%s) = %q, want %q", ref, sha, commitA)
		}
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
	b, _ := newTestBackend(t, "")
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("no/such")

	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main"); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Errorf("ResolveRef on missing repo = %v, want ErrRepoNotFound", err)
	}
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, commitA); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Errorf("ResolveRef(sha) on missing repo = %v, want ErrRepoNotFound", err)
	}
	if _, err := b.GetManifest(ctx, hfapi.RepoKindModel, repo, commitA); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Errorf("GetManifest on missing repo = %v, want ErrRepoNotFound", err)
	}

	m := testManifest("org/repo", map[string][]byte{"a": []byte("x")})
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, m.Repo, "nope"); !errors.Is(err, backend.ErrRevisionNotFound) {
		t.Errorf("ResolveRef on missing ref = %v, want ErrRevisionNotFound", err)
	}
	other := strings.Repeat("b", 40)
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, m.Repo, other); !errors.Is(err, backend.ErrRevisionNotFound) {
		t.Errorf("ResolveRef on missing sha = %v, want ErrRevisionNotFound", err)
	}
	if _, err := b.GetManifest(ctx, hfapi.RepoKindModel, m.Repo, other); !errors.Is(err, backend.ErrRevisionNotFound) {
		t.Errorf("GetManifest on missing sha = %v, want ErrRevisionNotFound", err)
	}
	if _, err := b.StatBlob(ctx, hfapi.RepoKindModel, m.Repo, backend.SHA256Digest(strings.Repeat("0", 64))); !errors.Is(err, backend.ErrBlobNotFound) {
		t.Errorf("StatBlob on missing blob = %v, want ErrBlobNotFound", err)
	}
	if _, err := b.OpenBlob(ctx, hfapi.RepoKindModel, m.Repo, backend.SHA256Digest(strings.Repeat("0", 64))); !errors.Is(err, backend.ErrBlobNotFound) {
		t.Errorf("OpenBlob on missing blob = %v, want ErrBlobNotFound", err)
	}
}

func TestBlobRoundtripAndVerification(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t, "")
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/repo")

	content := []byte("large binary weights")
	lfsDigest := backend.SHA256Digest(sha256Hex(content))
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, lfsDigest, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("PutBlob(sha256): %v", err)
	}
	info, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, lfsDigest)
	if err != nil || info.Size != int64(len(content)) {
		t.Fatalf("StatBlob = %+v, %v", info, err)
	}
	rc, err := b.OpenBlob(ctx, hfapi.RepoKindModel, repo, lfsDigest)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Errorf("OpenBlob = %q, want %q", got, content)
	}

	// git-sha1 keys (regular files from pull-through) round-trip too.
	regular := []byte("{\"a\": 1}")
	gitDigest := backend.SHA1Digest(fakehub.GitBlobOID(regular))
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, gitDigest, bytes.NewReader(regular), int64(len(regular))); err != nil {
		t.Fatalf("PutBlob(sha1): %v", err)
	}
	if _, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, gitDigest); err != nil {
		t.Errorf("StatBlob(sha1): %v", err)
	}

	// Corrupt content is rejected before anything lands in the bucket.
	wrong := backend.SHA256Digest(strings.Repeat("1", 64))
	err = b.PutBlob(ctx, hfapi.RepoKindModel, repo, wrong, bytes.NewReader(content), int64(len(content)))
	if !errors.Is(err, backend.ErrDigestMismatch) {
		t.Errorf("PutBlob with wrong sha256 = %v, want ErrDigestMismatch", err)
	}
	if _, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, wrong); !errors.Is(err, backend.ErrBlobNotFound) {
		t.Errorf("corrupt blob visible after rejected PutBlob: %v", err)
	}
	wrongSHA1 := backend.SHA1Digest(strings.Repeat("2", 40))
	err = b.PutBlob(ctx, hfapi.RepoKindModel, repo, wrongSHA1, bytes.NewReader(regular), int64(len(regular)))
	if !errors.Is(err, backend.ErrDigestMismatch) {
		t.Errorf("PutBlob with wrong sha1 = %v, want ErrDigestMismatch", err)
	}
}

func TestPutBlobUnknownSize(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t, "")
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/repo")

	content := []byte("streamed with unknown length")
	lfsDigest := backend.SHA256Digest(sha256Hex(content))
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, lfsDigest, bytes.NewReader(content), -1); err != nil {
		t.Fatalf("PutBlob(sha256, -1): %v", err)
	}
	info, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, lfsDigest)
	if err != nil || info.Size != int64(len(content)) {
		t.Fatalf("StatBlob = %+v, %v", info, err)
	}

	// sha1 with unknown size verifies by re-reading the spool.
	gitDigest := backend.SHA1Digest(fakehub.GitBlobOID(content))
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, gitDigest, bytes.NewReader(content), -1); err != nil {
		t.Fatalf("PutBlob(sha1, -1): %v", err)
	}
	wrongSHA1 := backend.SHA1Digest(strings.Repeat("3", 40))
	err = b.PutBlob(ctx, hfapi.RepoKindModel, repo, wrongSHA1, bytes.NewReader(content), -1)
	if !errors.Is(err, backend.ErrDigestMismatch) {
		t.Errorf("PutBlob(bad sha1, -1) = %v, want ErrDigestMismatch", err)
	}
}

func TestPutBlobSizeMismatch(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t, "")
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/repo")

	content := []byte("exactly this")
	digest := backend.SHA256Digest(sha256Hex(content))
	err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, digest, bytes.NewReader(content), int64(len(content))+5)
	if err == nil {
		t.Fatal("PutBlob with wrong size succeeded")
	}
	if _, statErr := b.StatBlob(ctx, hfapi.RepoKindModel, repo, digest); !errors.Is(statErr, backend.ErrBlobNotFound) {
		t.Errorf("blob visible after size-mismatch PutBlob: %v", statErr)
	}

	// A claimed size of zero is still a size claim: non-empty content must
	// fail even though its digest matches the actual bytes.
	err = b.PutBlob(ctx, hfapi.RepoKindModel, repo, digest, bytes.NewReader(content), 0)
	if err == nil {
		t.Fatal("PutBlob with claimed size 0 and non-empty content succeeded")
	}
	if _, statErr := b.StatBlob(ctx, hfapi.RepoKindModel, repo, digest); !errors.Is(statErr, backend.ErrBlobNotFound) {
		t.Errorf("blob visible after zero-size-claim PutBlob: %v", statErr)
	}
}

func TestPutBlobIdempotent(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t, "")
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/repo")

	content := []byte("same bytes")
	digest := backend.SHA256Digest(sha256Hex(content))
	for range 2 {
		if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, digest, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
	}
	// The second put may skip the body entirely; content must survive.
	rc, err := b.OpenBlob(ctx, hfapi.RepoKindModel, repo, digest)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Errorf("content after re-put = %q", got)
	}
}

// TestManifestServesBeforeBlobs pins the staging invariant: backends
// accept manifests before their blobs arrive, and the manifest is
// readable the whole time.
func TestManifestServesBeforeBlobs(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t, "")
	ctx := context.Background()

	content := []byte("late-arriving weights")
	digest := backend.SHA256Digest(sha256Hex(content))
	id, _ := hfapi.ParseRepoID("org/staged")
	m := &backend.Manifest{
		Repo: id, Kind: hfapi.RepoKindModel, CommitSHA: commitA,
		Files: []backend.FileEntry{{Path: "weights.bin", Size: int64(len(content)), Digest: digest}},
	}
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	got, err := b.GetManifest(ctx, hfapi.RepoKindModel, id, commitA)
	if err != nil || len(got.Files) != 1 {
		t.Fatalf("GetManifest before blobs = %+v, %v", got, err)
	}
	if _, err := b.StatBlob(ctx, hfapi.RepoKindModel, id, digest); !errors.Is(err, backend.ErrBlobNotFound) {
		t.Fatalf("StatBlob before arrival = %v, want ErrBlobNotFound", err)
	}
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, id, digest, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if _, err := b.StatBlob(ctx, hfapi.RepoKindModel, id, digest); err != nil {
		t.Errorf("StatBlob after arrival: %v", err)
	}
}

func TestBlobReaderWindow(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t, "")
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/repo")

	content := []byte("0123456789abcdefghij")
	digest := backend.SHA256Digest(sha256Hex(content))
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, digest, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	r, err := b.OpenBlob(ctx, hfapi.RepoKindModel, repo, digest)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	defer r.Close()

	// SeekEnd reports the size, and reading there is EOF without any GET.
	if end, err := r.Seek(0, io.SeekEnd); err != nil || end != int64(len(content)) {
		t.Fatalf("Seek(0, End) = %d, %v", end, err)
	}
	buf := make([]byte, 5)
	if n, err := r.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at end = %d, %v, want 0, io.EOF", n, err)
	}
	// Seek back and read a window (a fresh ranged GET).
	if _, err := r.Seek(10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "abcde" {
		t.Fatalf("window read = %q, %v", buf, err)
	}
	// SeekCurrent continues from the position.
	if pos, err := r.Seek(2, io.SeekCurrent); err != nil || pos != 17 {
		t.Fatalf("Seek(2, Current) = %d, %v", pos, err)
	}
	rest, err := io.ReadAll(r)
	if err != nil || string(rest) != "hij" {
		t.Fatalf("tail read = %q, %v", rest, err)
	}
	// Reads at EOF return io.EOF.
	if n, err := r.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at EOF = %d, %v", n, err)
	}
	// SeekEnd with a negative offset addresses from the tail.
	if pos, err := r.Seek(-3, io.SeekEnd); err != nil || pos != 17 {
		t.Fatalf("Seek(-3, End) = %d, %v", pos, err)
	}
	if tail, err := io.ReadAll(r); err != nil || string(tail) != "hij" {
		t.Fatalf("tail via SeekEnd = %q, %v", tail, err)
	}
	// Seeking to exactly 0 is legal; negative and bogus seeks fail.
	if pos, err := r.Seek(0, io.SeekStart); err != nil || pos != 0 {
		t.Fatalf("Seek(0, Start) = %d, %v", pos, err)
	}
	if _, err := io.ReadFull(r, buf[:1]); err != nil || buf[0] != '0' {
		t.Fatalf("read after rewind = %q, %v", buf[:1], err)
	}
	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Error("negative seek accepted")
	}
	if _, err := r.Seek(0, 99); err == nil {
		t.Error("bogus whence accepted")
	}

	// Closing a reader that never opened a stream is a no-op.
	unread, err := b.OpenBlob(ctx, hfapi.RepoKindModel, repo, digest)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	if err := unread.Close(); err != nil {
		t.Errorf("Close without read: %v", err)
	}
}

// TestBlobReaderHonorsReportedSize pins the size contract: the reader
// serves exactly the size it reported at open time, even if the object
// grows underneath it (http.ServeContent trusts this for Content-Length).
func TestBlobReaderHonorsReportedSize(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackend(t, "")
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/repo")

	content := []byte("0123456789")
	digest := backend.SHA256Digest(sha256Hex(content))
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, digest, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	r, err := b.OpenBlob(ctx, hfapi.RepoKindModel, repo, digest)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	defer r.Close()
	// The object grows after open; the reader must stop at its size —
	// reading from a mid-blob position, where the remaining window
	// (size-pos) is what bounds the stream.
	fake.Seed("models/org/repo/blobs/"+digest.Hex(), []byte("0123456789EXTRA"))
	if _, err := r.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "456789" {
		t.Errorf("ReadAll from pos 4 = %q, %v, want \"456789\"", got, err)
	}
}

func TestCreateDeleteRepo(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackend(t, "")
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/lifecycle")

	if err := b.DeleteRepo(ctx, hfapi.RepoKindModel, repo); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Errorf("DeleteRepo on missing = %v, want ErrRepoNotFound", err)
	}
	if err := b.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if err := b.CreateRepo(ctx, hfapi.RepoKindModel, repo); !errors.Is(err, backend.ErrRepoExists) {
		t.Errorf("second CreateRepo = %v, want ErrRepoExists", err)
	}
	// A created-but-empty repo exists: missing refs are RevisionNotFound.
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main"); !errors.Is(err, backend.ErrRevisionNotFound) {
		t.Errorf("ResolveRef on empty repo = %v, want ErrRevisionNotFound", err)
	}

	m := testManifest("org/lifecycle", map[string][]byte{"config.json": []byte(`{}`)})
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	content := []byte("blob")
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, backend.SHA256Digest(sha256Hex(content)), bytes.NewReader(content), 4); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	if err := b.DeleteRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	for _, key := range fake.Keys() {
		if strings.HasPrefix(key, "models/org/lifecycle/") {
			t.Errorf("key %s survived DeleteRepo", key)
		}
	}
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main"); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Errorf("ResolveRef after delete = %v, want ErrRepoNotFound", err)
	}
}

// TestDeleteRepoPaginates seeds more objects than one list page returns.
func TestDeleteRepoPaginates(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackend(t, "")
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/big")

	for i := range 1200 {
		fake.Seed(fmt.Sprintf("models/org/big/blobs/%04d", i), []byte("x"))
	}
	if err := b.DeleteRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	if keys := fake.Keys(); len(keys) != 0 {
		t.Errorf("%d keys survived paginated delete", len(keys))
	}
}

// TestImplicitRepoExistence pins that repos materialize without
// CreateRepo (pull-through and replication write manifests directly).
func TestImplicitRepoExistence(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t, "")
	ctx := context.Background()

	m := testManifest("org/implicit", map[string][]byte{"a": []byte("1")})
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if err := b.CreateRepo(ctx, hfapi.RepoKindModel, m.Repo); !errors.Is(err, backend.ErrRepoExists) {
		t.Errorf("CreateRepo after implicit materialization = %v, want ErrRepoExists", err)
	}
}

func TestPathValidation(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t, "")
	ctx := context.Background()
	id, _ := hfapi.ParseRepoID("org/repo")

	for _, bad := range []string{"../escape", "a/../b", "/abs", "a//b", "a\\b", ".", ""} {
		m := &backend.Manifest{
			Repo: id, Kind: hfapi.RepoKindModel, CommitSHA: commitA,
			Files: []backend.FileEntry{{Path: bad, Size: 1}},
		}
		if err := b.PutManifest(ctx, m, nil); err == nil {
			t.Errorf("PutManifest accepted file path %q", bad)
		}
	}

	good := testManifest("org/repo", map[string][]byte{"ok": []byte("1")})
	if err := b.PutManifest(ctx, good, map[string]string{"../sneaky": commitA}); err == nil {
		t.Error("PutManifest accepted ref name ../sneaky")
	}
	if err := b.PutManifest(ctx, good, map[string]string{"main": "not-a-sha"}); err == nil {
		t.Error("PutManifest accepted invalid commit SHA for ref")
	}
	if err := b.PutManifest(ctx, &backend.Manifest{}, nil); err == nil {
		t.Error("PutManifest accepted empty manifest")
	}
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, id, "../../etc"); err == nil {
		t.Error("ResolveRef accepted traversal ref")
	}
}

func TestRefContentValidated(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackend(t, "")
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/repo")

	fake.Seed("models/org/repo/refs/main", []byte("garbage-not-a-sha"))
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main"); err == nil ||
		errors.Is(err, backend.ErrRevisionNotFound) {
		t.Errorf("ResolveRef with corrupt ref = %v, want hard error", err)
	}

	fake.Seed("models/org/repo/manifests/"+commitA+".json", []byte("not json"))
	if _, err := b.GetManifest(ctx, hfapi.RepoKindModel, repo, commitA); err == nil ||
		errors.Is(err, backend.ErrRevisionNotFound) {
		t.Errorf("GetManifest with corrupt manifest = %v, want hard error", err)
	}
}

// TestPrefixIsolation shares one bucket between two backends under
// different prefixes.
func TestPrefixIsolation(t *testing.T) {
	t.Parallel()
	fake := fakes3.New("shared", "", "")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	newAt := func(prefix string) *Backend {
		b, err := New("test", Options{Endpoint: srv.URL, Bucket: "shared", Prefix: prefix})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return b
	}
	a, bb := newAt("team-a/"), newAt("team-b")
	ctx := context.Background()

	m := testManifest("org/repo", map[string][]byte{"a": []byte("1")})
	if err := a.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if _, err := a.ResolveRef(ctx, hfapi.RepoKindModel, m.Repo, "main"); err != nil {
		t.Errorf("prefixed ResolveRef: %v", err)
	}
	if _, err := bb.ResolveRef(ctx, hfapi.RepoKindModel, m.Repo, "main"); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Errorf("cross-prefix ResolveRef = %v, want ErrRepoNotFound", err)
	}
	for _, key := range fake.Keys() {
		if !strings.HasPrefix(key, "team-a/models/org/repo/") {
			t.Errorf("unexpected key %s", key)
		}
	}
}

func TestPing(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t, "")
	if err := b.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}

	dead, err := New("dead", Options{Endpoint: "http://127.0.0.1:1", Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.Ping(context.Background()); err == nil {
		t.Error("Ping against dead endpoint succeeded")
	}
}

func TestName(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t, "")
	if b.Name() != "test" {
		t.Errorf("Name = %q", b.Name())
	}
}

func TestKindNamespaces(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackend(t, "")
	ctx := context.Background()
	id, _ := hfapi.ParseRepoID("org/dual")

	model := testManifest("org/dual", map[string][]byte{"a": []byte("1")})
	if err := b.PutManifest(ctx, model, map[string]string{"main": commitA}); err != nil {
		t.Fatal(err)
	}
	dataset := testManifest("org/dual", map[string][]byte{"a": []byte("1")})
	dataset.Kind = hfapi.RepoKindDataset
	if err := b.PutManifest(ctx, dataset, map[string]string{"main": commitA}); err != nil {
		t.Fatal(err)
	}
	var models, datasets bool
	for _, key := range fake.Keys() {
		models = models || strings.HasPrefix(key, "models/org/dual/")
		datasets = datasets || strings.HasPrefix(key, "datasets/org/dual/")
	}
	if !models || !datasets {
		t.Errorf("kinds share a namespace: %v", fake.Keys())
	}
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindDataset, id, "main"); err != nil {
		t.Errorf("dataset ResolveRef: %v", err)
	}
}
