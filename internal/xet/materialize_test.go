package xet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/ocibackend"
	"github.com/loewenthal-corp/shpiel/internal/fakeregistry"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// registryMaterializer adapts an OCI backend to the Materializer the
// service wants (in production the relay is that adapter).
type registryMaterializer struct{ b *ocibackend.Backend }

func (m registryMaterializer) HasLFSBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, oid string) bool {
	_, err := m.b.StatBlob(ctx, kind, repo, backend.SHA256Digest(oid))
	return err == nil
}

func (m registryMaterializer) PutLFSBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, oid string, size int64, body io.Reader) error {
	return m.b.PutBlob(ctx, kind, repo, backend.SHA256Digest(oid), body, size)
}

// TestMaterializeLargeFileIntoOCIBackend is the cluster regression for
// "xet materialization failed ... 416 BLOB_UPLOAD_INVALID": a shard whose
// file exceeds ociclient's 8 MiB chunk size, materialized through the
// tar-layers OCI backend into a registry with Zot's strict upload
// semantics. Everything downstream of the shard parse is real.
func TestMaterializeLargeFileIntoOCIBackend(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// A 8.25 MiB file as one xorb of 132 uncompressed 64 KiB chunks.
	const chunkLen = 64 << 10
	const numChunks = 132
	content := make([]byte, numChunks*chunkLen)
	for i := range content {
		content[i] = byte(i*13 + i>>12)
	}
	var xorb []byte
	for i := range numChunks {
		xorb = append(xorb, buildChunk(t, content[i*chunkLen:(i+1)*chunkLen], compressionNone)...)
	}
	xorbHash, err := ParseHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutXorb(context.Background(), xorbHash, xorb); err != nil {
		t.Fatalf("PutXorb: %v", err)
	}

	sum := sha256.Sum256(content)
	rec := &FileRecord{
		FileHash: strings.Repeat("cd", 32),
		SHA256:   hex.EncodeToString(sum[:]),
		TotalLen: int64(len(content)),
		Terms: []TermRecord{{
			Xorb: xorbHash.Hex(), ChunkStart: 0, ChunkEnd: numChunks, UnpackedLen: int64(len(content)),
		}},
	}

	registry := httptest.NewServer(fakeregistry.New())
	t.Cleanup(registry.Close)
	ob, err := ocibackend.New("strict", ocibackend.Options{URL: registry.URL, Format: ocibackend.FormatTarLayers})
	if err != nil {
		t.Fatal(err)
	}
	mat := registryMaterializer{b: ob}
	svc, err := NewService(store, mat, nil, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	repo, _ := hfapi.ParseRepoID("org/xet-large")
	if err := svc.materialize(ctx, hfapi.RepoKindModel, repo, rec); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	if !mat.HasLFSBlob(ctx, hfapi.RepoKindModel, repo, rec.SHA256) {
		t.Fatal("materialized blob not in backend")
	}
	rc, err := ob.OpenBlob(ctx, hfapi.RepoKindModel, repo, backend.SHA256Digest(rec.SHA256))
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("materialized content mismatch: %d bytes back, want %d", len(got), len(content))
	}

	// Re-materializing is a no-op, not an error (shards are re-posted on
	// client retries).
	if err := svc.materialize(ctx, hfapi.RepoKindModel, repo, rec); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
}

// TestMaterializeStatus pins the shard-response status mapping: shards
// referencing content the client never sent are the client's fault,
// backend trouble is not. The cluster failure surfaced as POST /xet/shards
// 400 when the true cause was Shpiel's own registry commit failing.
func TestMaterializeStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"missing xorb", fmt.Errorf("term references xorb ab: %w", ErrNotFound), http.StatusBadRequest},
		{"digest mismatch", fmt.Errorf("verify: %w", backend.ErrDigestMismatch), http.StatusBadRequest},
		{"backend failure", errors.New("ociclient: committing blob: status 503"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		if got := materializeStatus(tc.err); got != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, got, tc.want)
		}
	}
}
