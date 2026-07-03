package xet

import (
	"bytes"
	"strings"
	"testing"
)

// rawChunkHeader assembles an 8-byte chunk header with explicit lengths,
// bypassing buildChunk so the size fields can sit exactly on the caps.
func rawChunkHeader(compressedLen, unpackedLen int, scheme byte) []byte {
	return []byte{
		chunkHeaderVersion,
		byte(compressedLen), byte(compressedLen >> 8), byte(compressedLen >> 16),
		scheme,
		byte(unpackedLen), byte(unpackedLen >> 8), byte(unpackedLen >> 16),
	}
}

// TestWalkChunksSizeCaps: lengths exactly at the caps are plausible; one
// past is not. An empty chunk is a header with nothing after it.
func TestWalkChunksSizeCaps(t *testing.T) {
	t.Parallel()

	var xorb []byte
	xorb = append(xorb, rawChunkHeader(0, 0, compressionNone)...)                // empty chunk, header only
	xorb = append(xorb, rawChunkHeader(0, maxChunkSize, compressionNone)...)     // unpacked at cap
	xorb = append(xorb, rawChunkHeader(2*maxChunkSize, 100, compressionNone)...) // compressed at cap
	xorb = append(xorb, bytes.Repeat([]byte{0x5A}, 2*maxChunkSize)...)
	xorb = append(xorb, xorbFooterIdent...)

	chunks, err := WalkChunks(xorb)
	if err != nil {
		t.Fatalf("WalkChunks: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	if chunks[0].End != chunkHeaderLen || chunks[1].UnpackedLen != maxChunkSize {
		t.Fatalf("chunks = %+v", chunks[:2])
	}

	for _, tc := range []struct {
		name   string
		header []byte
	}{
		{"unpacked beyond cap", rawChunkHeader(0, maxChunkSize+1, compressionNone)},
		{"compressed beyond cap", rawChunkHeader(2*maxChunkSize+1, 100, compressionNone)},
	} {
		bad := append(append([]byte{}, tc.header...), bytes.Repeat([]byte{0}, 3*maxChunkSize)...)
		if _, err := WalkChunks(bad); err == nil || !strings.Contains(err.Error(), "implausible") {
			t.Errorf("%s: err = %v, want implausible-lengths error", tc.name, err)
		}
	}
}

// TestWalkChunksExactHeader: a xorb that is exactly one header-only chunk
// (no footer, no padding) parses as that single empty chunk.
func TestWalkChunksExactHeader(t *testing.T) {
	t.Parallel()
	chunks, err := WalkChunks(rawChunkHeader(0, 0, compressionNone))
	if err != nil {
		t.Fatalf("WalkChunks: %v", err)
	}
	if len(chunks) != 1 || chunks[0].End != chunkHeaderLen {
		t.Fatalf("chunks = %+v", chunks)
	}
}

// TestWalkChunksTruncatedHeader: a trailing fragment shorter than a header
// (and not the footer) is an error, not a silent stop.
func TestWalkChunksTruncatedHeader(t *testing.T) {
	t.Parallel()
	xorb := rawChunkHeader(0, 0, compressionNone)
	xorb = append(xorb, 0x01, 0x02, 0x03) // 3 stray bytes
	if _, err := WalkChunks(xorb); err == nil || !strings.Contains(err.Error(), "truncated chunk header") {
		t.Fatalf("err = %v, want truncated-header error", err)
	}
}

// TestDecodeChunkHeaderOnly: an empty uncompressed chunk decodes to zero
// bytes; anything shorter than a header is rejected.
func TestDecodeChunkHeaderOnly(t *testing.T) {
	t.Parallel()
	out, err := DecodeChunk(rawChunkHeader(0, 0, compressionNone))
	if err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("decoded %d bytes, want 0", len(out))
	}
	if _, err := DecodeChunk(rawChunkHeader(0, 0, compressionNone)[:7]); err == nil {
		t.Fatal("7-byte chunk accepted")
	}
}

// TestDecodeChunkRangeBounds: an empty or inverted range is an error even
// when it would touch no chunks.
func TestDecodeChunkRangeBounds(t *testing.T) {
	t.Parallel()
	xorb := buildChunk(t, []byte("content"), compressionNone)
	chunks, err := WalkChunks(xorb)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, tc := range []struct{ start, end int }{
		{0, 0},  // empty
		{1, 1},  // empty at end
		{-1, 1}, // negative start
		{0, 2},  // beyond available chunks
	} {
		if err := DecodeChunkRange(xorb, chunks, tc.start, tc.end, &out); err == nil {
			t.Errorf("range [%d, %d) accepted", tc.start, tc.end)
		}
	}
}
