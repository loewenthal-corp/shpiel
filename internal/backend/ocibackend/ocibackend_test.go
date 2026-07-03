package ocibackend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"testing"
	"time"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/fakeregistry"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// newTestBackend spins an in-process OCI registry (go-containerregistry)
// and a backend in the given format against it.
func newTestBackend(t *testing.T, format string) *Backend {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	b, err := New("test-oci", Options{URL: srv.URL, Format: format})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newStrictBackend runs against internal/fakeregistry, which reproduces
// Zot's strict upload-session semantics. Both registries stay in the
// suite: ggcr's is lenient where Zot is strict, so only this one would
// have caught the chunked-commit 416.
func newStrictBackend(t *testing.T, format string) *Backend {
	t.Helper()
	srv := httptest.NewServer(fakeregistry.New())
	t.Cleanup(srv.Close)
	b, err := New("test-oci-strict", Options{URL: srv.URL, Format: format})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func manifestFor(repo string, commit string, files map[string][]byte) *backend.Manifest {
	id, _ := hfapi.ParseRepoID(repo)
	m := &backend.Manifest{
		Repo:      id,
		Kind:      hfapi.RepoKindModel,
		CommitSHA: commit,
		FetchedAt: time.Now().UTC(),
	}
	for path, content := range files {
		m.Files = append(m.Files, backend.FileEntry{
			Path:   path,
			Size:   int64(len(content)),
			Digest: backend.SHA256Digest(fakehub.SHA256Hex(content)),
			OID:    fakehub.GitBlobOID(content),
		})
	}
	return m
}

// testRoundtrip drives the full lifecycle for one format: blobs first,
// manifest, refs, reads with seeking.
func testRoundtrip(t *testing.T, b *Backend) {
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("Org/Model.Name") // exercises lowercasing

	files := map[string][]byte{
		"config.json":       []byte(`{"model_type":"oci"}`),
		"model.safetensors": bytes.Repeat([]byte{1, 2, 3, 4, 5}, 2048),
	}
	m := manifestFor("Org/Model.Name", commitA, files)

	// Write-path order: blobs first, then the manifest with refs.
	for path, content := range files {
		entry := m.File(path)
		if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, entry.Digest, bytes.NewReader(content), entry.Size); err != nil {
			t.Fatalf("PutBlob(%s): %v", path, err)
		}
	}
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}

	sha, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main")
	if err != nil || sha != commitA {
		t.Fatalf("ResolveRef(main) = %q, %v; want %s", sha, err, commitA)
	}
	got, err := b.GetManifest(ctx, hfapi.RepoKindModel, repo, commitA)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if len(got.Files) != len(files) {
		t.Fatalf("manifest files = %d, want %d", len(got.Files), len(files))
	}

	for path, content := range files {
		entry := got.File(path)
		info, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, entry.Digest)
		if err != nil || info.Size != int64(len(content)) {
			t.Fatalf("StatBlob(%s) = %+v, %v", path, info, err)
		}
		rc, err := b.OpenBlob(ctx, hfapi.RepoKindModel, repo, entry.Digest)
		if err != nil {
			t.Fatalf("OpenBlob(%s): %v", path, err)
		}
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !bytes.Equal(data, content) {
			t.Fatalf("%s content mismatch (%d vs %d bytes)", path, len(data), len(content))
		}

		// Seek within the blob: ServeContent's Range pattern.
		if _, err := rc.Seek(5, io.SeekStart); err != nil {
			t.Fatalf("Seek: %v", err)
		}
		part := make([]byte, 4)
		if _, err := io.ReadFull(rc, part); err != nil {
			t.Fatalf("ranged read: %v", err)
		}
		if !bytes.Equal(part, content[5:9]) {
			t.Fatalf("ranged read = %v, want %v", part, content[5:9])
		}
		rc.Close()
	}
}

func TestRoundtripModelPack(t *testing.T) {
	t.Parallel()
	testRoundtrip(t, newTestBackend(t, FormatModelPack))
}
func TestRoundtripTarLayers(t *testing.T) {
	t.Parallel()
	testRoundtrip(t, newTestBackend(t, FormatTarLayers))
}
func TestRoundtripModelPackStrict(t *testing.T) {
	t.Parallel()
	testRoundtrip(t, newStrictBackend(t, FormatModelPack))
}
func TestRoundtripTarLayersStrict(t *testing.T) {
	t.Parallel()
	testRoundtrip(t, newStrictBackend(t, FormatTarLayers))
}

// TestLargeBlobCrossesChunkBoundary is the cluster regression: LFS
// uploads and Xet materializations of 8 MiB and up cross ociclient's
// chunk size, and committing them to Zot died with 416
// BLOB_UPLOAD_INVALID. 8 MiB exactly reproduces the reported failure;
// the odd extra reproduces the every-larger-upload case.
func TestLargeBlobCrossesChunkBoundary(t *testing.T) {
	t.Parallel()
	for _, format := range []string{FormatModelPack, FormatTarLayers} {
		for _, size := range []int{8 << 20, 8<<20 + 4097} {
			t.Run(fmt.Sprintf("%s-%d", format, size), func(t *testing.T) {
				t.Parallel()
				b := newStrictBackend(t, format)
				ctx := context.Background()
				repo, _ := hfapi.ParseRepoID("org/large")

				payload := make([]byte, size)
				for i := range payload {
					payload[i] = byte(i*31 + i>>10)
				}
				digest := backend.SHA256Digest(fakehub.SHA256Hex(payload))
				if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, digest, bytes.NewReader(payload), int64(size)); err != nil {
					t.Fatalf("PutBlob(%d bytes): %v", size, err)
				}

				info, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, digest)
				if err != nil || info.Size != int64(size) {
					t.Fatalf("StatBlob = %+v, %v", info, err)
				}
				rc, err := b.OpenBlob(ctx, hfapi.RepoKindModel, repo, digest)
				if err != nil {
					t.Fatalf("OpenBlob: %v", err)
				}
				defer rc.Close()
				got, err := io.ReadAll(rc)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, payload) {
					t.Fatalf("read back %d bytes, want %d (content mismatch)", len(got), len(payload))
				}
			})
		}
	}
}

// TestStagingPromote covers the pull-through order: manifest first (blobs
// missing => staged), blobs trickle in, and the final blob promotes the
// commit to its real tags.
func testStagingPromote(t *testing.T, b *Backend) {
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/staged")

	files := map[string][]byte{
		"a.bin": bytes.Repeat([]byte{9}, 4096),
		"b.bin": bytes.Repeat([]byte{7}, 2048),
	}
	m := manifestFor("org/staged", commitA, files)

	// Manifest before any blob: staged, but readable (pull-through relies
	// on reading staged manifests to know what to fetch).
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("PutManifest (staged): %v", err)
	}
	got, err := b.GetManifest(ctx, hfapi.RepoKindModel, repo, commitA)
	if err != nil {
		t.Fatalf("GetManifest while staged: %v", err)
	}
	if len(got.Files) != 2 {
		t.Fatalf("staged manifest files = %d", len(got.Files))
	}
	// Refs must not resolve while staged: the commit is not servable.
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main"); !errors.Is(err, backend.ErrRevisionNotFound) {
		t.Fatalf("ResolveRef while staged = %v, want ErrRevisionNotFound", err)
	}

	// First blob arrives: still staged.
	entryA := m.File("a.bin")
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, entryA.Digest, bytes.NewReader(files["a.bin"]), entryA.Size); err != nil {
		t.Fatalf("PutBlob(a): %v", err)
	}
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main"); !errors.Is(err, backend.ErrRevisionNotFound) {
		t.Fatalf("ResolveRef after first blob = %v, want ErrRevisionNotFound", err)
	}

	// Last blob arrives: promoted, refs live.
	entryB := m.File("b.bin")
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, entryB.Digest, bytes.NewReader(files["b.bin"]), entryB.Size); err != nil {
		t.Fatalf("PutBlob(b): %v", err)
	}
	sha, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main")
	if err != nil || sha != commitA {
		t.Fatalf("ResolveRef after promote = %q, %v; want %s", sha, err, commitA)
	}
}

func TestStagingPromoteModelPack(t *testing.T) {
	t.Parallel()
	testStagingPromote(t, newTestBackend(t, FormatModelPack))
}
func TestStagingPromoteTarLayers(t *testing.T) {
	t.Parallel()
	testStagingPromote(t, newTestBackend(t, FormatTarLayers))
}
func TestStagingPromoteModelPackStrict(t *testing.T) {
	t.Parallel()
	testStagingPromote(t, newStrictBackend(t, FormatModelPack))
}
func TestStagingPromoteTarLayersStrict(t *testing.T) {
	t.Parallel()
	testStagingPromote(t, newStrictBackend(t, FormatTarLayers))
}

func TestRepoLifecycle(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t, FormatModelPack)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/lifecycle")

	if err := b.CreateRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	m := manifestFor("org/lifecycle", commitA, map[string][]byte{"f": []byte("x")})
	entry := m.File("f")
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, entry.Digest, bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatal(err)
	}
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateRepo(ctx, hfapi.RepoKindModel, repo); !errors.Is(err, backend.ErrRepoExists) {
		t.Fatalf("CreateRepo on existing = %v, want ErrRepoExists", err)
	}
	if err := b.DeleteRepo(ctx, hfapi.RepoKindModel, repo); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main"); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Fatalf("after delete = %v, want ErrRepoNotFound", err)
	}
}

func TestNotFoundMapping(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t, FormatModelPack)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/absent")

	if _, err := b.ResolveRef(ctx, hfapi.RepoKindModel, repo, "main"); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Errorf("ResolveRef = %v, want ErrRepoNotFound", err)
	}
	if _, err := b.GetManifest(ctx, hfapi.RepoKindModel, repo, commitA); !errors.Is(err, backend.ErrRepoNotFound) {
		t.Errorf("GetManifest = %v, want ErrRepoNotFound", err)
	}
	if _, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, backend.SHA256Digest(fakehub.SHA256Hex([]byte("nope")))); !errors.Is(err, backend.ErrBlobNotFound) {
		t.Errorf("StatBlob = %v, want ErrBlobNotFound", err)
	}
}

func TestPutBlobVerifiesDigest(t *testing.T) {
	t.Parallel()
	for _, format := range []string{FormatModelPack, FormatTarLayers} {
		b := newTestBackend(t, format)
		ctx := context.Background()
		repo, _ := hfapi.ParseRepoID("org/corrupt")
		wrong := backend.SHA256Digest(fakehub.SHA256Hex([]byte("expected content")))
		err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, wrong, bytes.NewReader([]byte("actual different content")), 24)
		if err == nil {
			t.Errorf("format %s: PutBlob with wrong digest succeeded", format)
		}
	}
}

func TestSHA1KeysRejected(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t, FormatModelPack)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/sha1")
	err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, backend.SHA1Digest(fakehub.GitBlobOID([]byte("x"))), bytes.NewReader([]byte("x")), 1)
	if err == nil {
		t.Fatal("PutBlob with sha1 key succeeded; OCI requires sha256")
	}
}

func TestTagSanitization(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"main":       "main",
		"v1.0":       "v1.0",
		"refs/pr/1":  "refs-pr-1",
		"weird ref!": "ref-weird-ref-",
	}
	for ref, want := range cases {
		if got := tagForRef(ref); got != want {
			t.Errorf("tagForRef(%q) = %q, want %q", ref, got, want)
		}
	}
}
