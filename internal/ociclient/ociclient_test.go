package ociclient

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/fakeregistry"
)

// Tests run against internal/fakeregistry, which reproduces Zot's strict
// upload-session semantics — the lenient go-containerregistry fake let a
// broken Commit (tail bytes on the closing PUT without Content-Range)
// pass unit tests and 416 in production.

func newTestClient(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(fakeregistry.New())
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// testPayload is deterministic but non-repeating, so misplaced chunk
// boundaries corrupt the digest rather than cancel out.
func testPayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*7 + i>>9)
	}
	return p
}

// TestBlobWriterCommitShapes covers every shape the closing sequence can
// take relative to the chunk size. "exact chunk" and "chunks plus tail"
// are the regression cases for the cluster failure: an 8 MiB LFS upload
// (and every Xet materialization over 8 MiB) crossed the chunk boundary
// and the old Commit then died with 416 BLOB_UPLOAD_INVALID against Zot.
func TestBlobWriterCommitShapes(t *testing.T) {
	t.Parallel()
	const chunk = 1024
	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"sub-chunk", 512},
		{"exact chunk", chunk},
		{"exact multiple", 3 * chunk},
		{"chunks plus tail", 3*chunk + 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t)
			ctx := context.Background()

			w, err := c.NewBlobWriter(ctx, "org/model")
			if err != nil {
				t.Fatal(err)
			}
			w.chunkSize = chunk

			payload := testPayload(tc.size)
			// Odd-sized writes so flushes land mid-Write, not only on
			// call boundaries.
			for chunkStart := 0; chunkStart < len(payload); chunkStart += 700 {
				end := min(chunkStart+700, len(payload))
				if _, err := w.Write(payload[chunkStart:end]); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}

			digest, size, err := w.Commit()
			if err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if want := SHA256Digest(payload); digest != want {
				t.Fatalf("digest = %s, want %s", digest, want)
			}
			if size != int64(len(payload)) {
				t.Fatalf("size = %d, want %d", size, len(payload))
			}

			gotSize, err := c.HeadBlob(ctx, "org/model", digest)
			if err != nil || gotSize != int64(len(payload)) {
				t.Fatalf("HeadBlob = %d, %v", gotSize, err)
			}
			rc, err := c.GetBlob(ctx, "org/model", digest, 0)
			if err != nil {
				t.Fatalf("GetBlob: %v", err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("content mismatch: %d bytes back, want %d", len(got), len(payload))
			}
		})
	}
}

// TestPutBlobUnknownSize: with no known size there is no Content-Length,
// and a monolithic PUT without one is a 400 on Zot — the client must fall
// back to an upload session.
func TestPutBlobUnknownSize(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	ctx := context.Background()

	payload := testPayload(4096)
	digest := SHA256Digest(payload)
	if err := c.PutBlob(ctx, "org/model", digest, bytes.NewReader(payload), -1); err != nil {
		t.Fatalf("PutBlob(size=-1): %v", err)
	}
	size, err := c.HeadBlob(ctx, "org/model", digest)
	if err != nil || size != int64(len(payload)) {
		t.Fatalf("HeadBlob after unsized put = %d, %v", size, err)
	}

	// A digest the content doesn't hash to must be reported, not stored
	// under the claimed name.
	wrong := "sha256:" + strings.Repeat("0", 64)
	err = c.PutBlob(ctx, "org/model", wrong, bytes.NewReader(payload), -1)
	if err == nil || !strings.Contains(err.Error(), "hashed to") {
		t.Fatalf("PutBlob with wrong digest = %v, want digest mismatch error", err)
	}
	if _, err := c.HeadBlob(ctx, "org/model", wrong); err == nil {
		t.Fatal("blob stored under a digest its content does not match")
	}
}

// TestPutBlobKnownSize: the sized path completes monolithically and is
// idempotent for content the registry already holds.
func TestPutBlobKnownSize(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	ctx := context.Background()

	payload := testPayload(2048)
	digest := SHA256Digest(payload)
	for range 2 {
		if err := c.PutBlob(ctx, "org/model", digest, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
	}
	rc, err := c.GetBlob(ctx, "org/model", digest, 100)
	if err != nil {
		t.Fatalf("GetBlob(offset): %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload[100:]) {
		t.Fatalf("ranged read mismatch: %d bytes, want %d", len(got), len(payload)-100)
	}
}
