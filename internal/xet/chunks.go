package xet

import (
	"bytes"
	"fmt"
	"io"

	"github.com/pierrec/lz4/v4"
)

// Compression schemes from xet-core's CompressionScheme enum.
const (
	compressionNone    = 0
	compressionLZ4     = 1
	compressionBG4LZ4  = 2
	chunkHeaderLen     = 8
	chunkHeaderVersion = 0
	// maxChunkSize mirrors xet-core's hard chunk cap (64 KiB target, 128
	// KiB max; headers are validated against 2x max on the compressed
	// side). Used purely as a sanity bound while walking.
	maxChunkSize = 128 << 10
)

// xorbFooterIdent marks the start of the xorb object footer ("XETBLOB");
// the chunk stream ends where it begins.
var xorbFooterIdent = []byte("XETBLOB")

// ChunkInfo locates one serialized chunk within a xorb.
type ChunkInfo struct {
	// Start and End bound the serialized chunk (header + payload) within
	// the xorb bytes.
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	// UnpackedLen is the decompressed chunk length.
	UnpackedLen int64 `json:"unpackedLen"`
	// Scheme is the compression scheme byte.
	Scheme byte `json:"scheme"`
}

// WalkChunks scans a serialized xorb and returns the chunk layout,
// stopping cleanly at the xorb object footer (or EOF). This derives chunk
// byte boundaries from the bytes themselves rather than trusting shard
// metadata.
func WalkChunks(xorb []byte) ([]ChunkInfo, error) {
	var chunks []ChunkInfo
	offset := int64(0)
	for offset < int64(len(xorb)) {
		remaining := xorb[offset:]
		if len(remaining) >= len(xorbFooterIdent) && bytes.Equal(remaining[:len(xorbFooterIdent)], xorbFooterIdent) {
			break // footer reached
		}
		if len(remaining) < chunkHeaderLen {
			return nil, fmt.Errorf("xet: truncated chunk header at offset %d", offset)
		}
		version := remaining[0]
		compressedLen := int64(remaining[1]) | int64(remaining[2])<<8 | int64(remaining[3])<<16
		scheme := remaining[4]
		unpackedLen := int64(remaining[5]) | int64(remaining[6])<<8 | int64(remaining[7])<<16

		if version != chunkHeaderVersion {
			return nil, fmt.Errorf("xet: unsupported chunk header version %d at offset %d", version, offset)
		}
		if scheme > compressionBG4LZ4 {
			return nil, fmt.Errorf("xet: unknown compression scheme %d at offset %d", scheme, offset)
		}
		if unpackedLen > maxChunkSize || compressedLen > 2*maxChunkSize {
			return nil, fmt.Errorf("xet: implausible chunk lengths (compressed %d, unpacked %d) at offset %d", compressedLen, unpackedLen, offset)
		}
		end := offset + chunkHeaderLen + compressedLen
		if end > int64(len(xorb)) {
			return nil, fmt.Errorf("xet: chunk at offset %d overruns xorb (%d > %d)", offset, end, len(xorb))
		}
		chunks = append(chunks, ChunkInfo{Start: offset, End: end, UnpackedLen: unpackedLen, Scheme: scheme})
		offset = end
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("xet: xorb contains no chunks")
	}
	return chunks, nil
}

// DecodeChunk decompresses one serialized chunk (header included).
func DecodeChunk(chunk []byte) ([]byte, error) {
	if len(chunk) < chunkHeaderLen {
		return nil, fmt.Errorf("xet: chunk shorter than header")
	}
	scheme := chunk[4]
	unpackedLen := int(chunk[5]) | int(chunk[6])<<8 | int(chunk[7])<<16
	payload := chunk[chunkHeaderLen:]

	switch scheme {
	case compressionNone:
		if len(payload) != unpackedLen {
			return nil, fmt.Errorf("xet: uncompressed chunk length mismatch (%d != %d)", len(payload), unpackedLen)
		}
		return payload, nil
	case compressionLZ4:
		out, err := lz4FrameDecompress(payload, unpackedLen)
		if err != nil {
			return nil, fmt.Errorf("xet: lz4 chunk: %w", err)
		}
		return out, nil
	case compressionBG4LZ4:
		grouped, err := lz4FrameDecompress(payload, unpackedLen)
		if err != nil {
			return nil, fmt.Errorf("xet: bg4-lz4 chunk: %w", err)
		}
		return bg4Regroup(grouped), nil
	default:
		return nil, fmt.Errorf("xet: unknown compression scheme %d", scheme)
	}
}

// DecodeChunkRange decompresses chunks [start, end) of a xorb into w.
func DecodeChunkRange(xorb []byte, chunks []ChunkInfo, start, end int, w io.Writer) error {
	if start < 0 || end > len(chunks) || start >= end {
		return fmt.Errorf("xet: chunk range [%d, %d) out of bounds (0..%d)", start, end, len(chunks))
	}
	for i := start; i < end; i++ {
		c := chunks[i]
		data, err := DecodeChunk(xorb[c.Start:c.End])
		if err != nil {
			return fmt.Errorf("xet: decoding chunk %d: %w", i, err)
		}
		if int64(len(data)) != c.UnpackedLen {
			return fmt.Errorf("xet: chunk %d decoded to %d bytes, header says %d", i, len(data), c.UnpackedLen)
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// lz4FrameDecompress inflates an LZ4 frame (xet uses frame format, not raw
// blocks), bounding output to the declared length.
func lz4FrameDecompress(payload []byte, unpackedLen int) ([]byte, error) {
	r := lz4.NewReader(bytes.NewReader(payload))
	out := make([]byte, 0, unpackedLen)
	buf := bytes.NewBuffer(out)
	n, err := io.Copy(buf, io.LimitReader(r, int64(unpackedLen)+1))
	if err != nil {
		return nil, err
	}
	if n != int64(unpackedLen) {
		return nil, fmt.Errorf("decompressed %d bytes, want %d", n, unpackedLen)
	}
	return buf.Bytes(), nil
}

// bg4Regroup inverts xet's byte-grouping-4 transform: the input holds four
// byte planes (plane p contains original bytes at positions ≡ p mod 4,
// with remainder bytes distributed to the lowest planes first).
func bg4Regroup(g []byte) []byte {
	n := len(g)
	split := n / 4
	rem := n % 4
	sizes := [4]int{split, split, split, split}
	for p := range rem {
		sizes[p]++
	}
	starts := [4]int{}
	for p := 1; p < 4; p++ {
		starts[p] = starts[p-1] + sizes[p-1]
	}
	out := make([]byte, n)
	for p := range 4 {
		plane := g[starts[p] : starts[p]+sizes[p]]
		for i, b := range plane {
			out[4*i+p] = b
		}
	}
	return out
}
