package ocibackend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/fakeregistry"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

func TestNewFormatValidation(t *testing.T) {
	t.Parallel()
	b, err := New("dflt", Options{URL: "http://registry.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Format() != FormatModelPack {
		t.Fatalf("default format = %q, want %q", b.Format(), FormatModelPack)
	}
	if _, err := New("bad", Options{URL: "http://registry.invalid", Format: "zip"}); err == nil {
		t.Fatal("unknown format accepted")
	}
	if _, err := New("bad", Options{URL: "registry.invalid"}); err == nil {
		t.Fatal("scheme-less URL accepted")
	}
}

// TestPublishLayerShapes: an empty commit publishes with the mandatory
// empty-JSON placeholder layer; a file commit publishes the file layers,
// not the placeholder. Also covers defaulting the manifest kind.
func TestPublishLayerShapes(t *testing.T) {
	t.Parallel()
	b := newStrictBackend(t, FormatModelPack)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/shapes")

	// Empty commit, kind defaulted to model.
	empty := &backend.Manifest{Repo: repo, CommitSHA: commitA, FetchedAt: time.Now()}
	if err := b.PutManifest(ctx, empty, map[string]string{"main": commitA}); err != nil {
		t.Fatal(err)
	}
	om, err := b.client.GetManifest(ctx, "models/org/shapes", commitA)
	if err != nil {
		t.Fatalf("published under unexpected repo/tag: %v", err)
	}
	if len(om.Layers) != 1 || om.Layers[0].MediaType != "application/vnd.oci.empty.v1+json" {
		t.Fatalf("empty-commit layers = %+v", om.Layers)
	}

	// A commit whose blob is present publishes real weight layers.
	content := []byte("weights")
	digest := backend.SHA256Digest(fakehub.SHA256Hex(content))
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, digest, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	const commitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	full := &backend.Manifest{
		Repo: repo, Kind: hfapi.RepoKindModel, CommitSHA: commitB, FetchedAt: time.Now(),
		Files: []backend.FileEntry{{Path: "w.bin", Size: int64(len(content)), Digest: digest}},
	}
	if err := b.PutManifest(ctx, full, map[string]string{"main": commitB}); err != nil {
		t.Fatal(err)
	}
	om, err = b.client.GetManifest(ctx, "models/org/shapes", commitB)
	if err != nil {
		t.Fatal(err)
	}
	if len(om.Layers) != 1 || om.Layers[0].MediaType != mediaTypeModelWeightRaw {
		t.Fatalf("file-commit layers = %+v", om.Layers)
	}
	if om.Layers[0].Annotations[annoTitle] != "w.bin" {
		t.Fatalf("layer annotations = %+v", om.Layers[0].Annotations)
	}
}

// failingProxy passes requests to a fakeregistry but fails those matching
// the filter.
func failingProxy(t *testing.T, match func(r *http.Request) bool) *httptest.Server {
	t.Helper()
	inner := fakeregistry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if match(r) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestStageFailureSurfaces: when the staged-tag manifest cannot be written
// the commit is not silently dropped.
func TestStageFailureSurfaces(t *testing.T) {
	t.Parallel()
	srv := failingProxy(t, func(r *http.Request) bool {
		return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/"+stagedTagPrefix)
	})
	b, err := New("flaky", Options{URL: srv.URL, Format: FormatModelPack})
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := hfapi.ParseRepoID("org/stagefail")
	m := &backend.Manifest{
		Repo: repo, Kind: hfapi.RepoKindModel, CommitSHA: commitA, FetchedAt: time.Now(),
		// Blob not uploaded: forces the staging path.
		Files: []backend.FileEntry{{Path: "w.bin", Size: 1, Digest: backend.SHA256Digest(strings.Repeat("ab", 32))}},
	}
	if err := b.PutManifest(context.Background(), m, nil); err == nil {
		t.Fatal("staging failure swallowed")
	}
}

// TestPromoteFailureSurfaces: the blob that completes a staged commit must
// report a failed promotion, or the commit stays invisible with no signal.
func TestPromoteFailureSurfaces(t *testing.T) {
	t.Parallel()
	srv := failingProxy(t, func(r *http.Request) bool {
		return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+commitA)
	})
	b, err := New("flaky", Options{URL: srv.URL, Format: FormatModelPack})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/promotefail")
	content := []byte("weights")
	digest := backend.SHA256Digest(fakehub.SHA256Hex(content))
	m := &backend.Manifest{
		Repo: repo, Kind: hfapi.RepoKindModel, CommitSHA: commitA, FetchedAt: time.Now(),
		Files: []backend.FileEntry{{Path: "w.bin", Size: int64(len(content)), Digest: digest}},
	}
	if err := b.PutManifest(ctx, m, map[string]string{"main": commitA}); err != nil {
		t.Fatalf("staging: %v", err)
	}
	err = b.PutBlob(ctx, hfapi.RepoKindModel, repo, digest, bytes.NewReader(content), int64(len(content)))
	if err == nil {
		t.Fatal("failed promotion swallowed by PutBlob")
	}
}

// TestEmptyBlobTarLayer: zero-length content is a legitimate tar layer.
func TestEmptyBlobTarLayer(t *testing.T) {
	t.Parallel()
	b := newStrictBackend(t, FormatTarLayers)
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/empty")
	digest := backend.SHA256Digest(fakehub.SHA256Hex(nil))
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, digest, bytes.NewReader(nil), 0); err != nil {
		t.Fatalf("empty blob rejected: %v", err)
	}
	info, err := b.StatBlob(ctx, hfapi.RepoKindModel, repo, digest)
	if err != nil || info.Size != 0 {
		t.Fatalf("stat = %+v, %v", info, err)
	}
	// Unknown-size content, by contrast, cannot become a tar layer.
	other := backend.SHA256Digest(fakehub.SHA256Hex([]byte("x")))
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, other, strings.NewReader("x"), -1); err == nil {
		t.Fatal("unknown-size tar layer accepted")
	}
}

// countingProxy counts blob GETs while passing everything to a fakeregistry.
func countingProxy(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	inner := fakeregistry.New()
	var mu sync.Mutex
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/sha256:") {
			mu.Lock()
			gets++
			mu.Unlock()
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return gets
	}
}

// TestBlobReaderWindow: reads and seeks stay inside the content window of
// the tar layer, EOF at the window end costs no extra registry round trip,
// and Close is safe at any point.
func TestBlobReaderWindow(t *testing.T) {
	t.Parallel()
	srv, blobGets := countingProxy(t)
	b, err := New("windowed", Options{URL: srv.URL, Format: FormatTarLayers})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/window")
	content := make([]byte, 4096)
	for i := range content {
		content[i] = byte(i*7 + i>>9) // non-repeating so misreads shift bytes
	}
	digest := backend.SHA256Digest(fakehub.SHA256Hex(content))
	if err := b.PutBlob(ctx, hfapi.RepoKindModel, repo, digest, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}

	// Close with nothing read: no panic, no error.
	rc, err := b.OpenBlob(ctx, hfapi.RepoKindModel, repo, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close before read: %v", err)
	}

	rc, err = b.OpenBlob(ctx, hfapi.RepoKindModel, repo, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	// Seek to 0 is valid; negative is not.
	if pos, err := rc.Seek(0, io.SeekStart); err != nil || pos != 0 {
		t.Fatalf("Seek(0) = %d, %v", pos, err)
	}
	if _, err := rc.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek accepted")
	}

	// A mid-content seek reads exactly the suffix — no tar framing bytes.
	if _, err := rc.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll after seek: %v", err)
	}
	if !bytes.Equal(got, content[100:]) {
		t.Fatalf("suffix read = %d bytes (want %d); first bytes %x vs %x", len(got), len(content)-100, got[:4], content[100:104])
	}

	// At exactly end-of-content, Read is EOF without another registry GET.
	// Seek away first so the reader's connection is dropped and a fresh
	// ranged GET would be observable.
	if _, err := rc.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	before := blobGets()
	if _, err := rc.Seek(int64(len(content)), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	n, err := rc.Read(make([]byte, 16))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read at EOF = %d, %v", n, err)
	}
	if after := blobGets(); after != before {
		t.Fatalf("EOF read cost %d extra blob GETs", after-before)
	}
}
