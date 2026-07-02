package xet

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/pierrec/lz4/v4"
)

func TestHashHexRoundtrip(t *testing.T) {
	t.Parallel()
	var h Hash
	for i := range h {
		h[i] = byte(i * 7)
	}
	hex := h.Hex()
	if len(hex) != 64 {
		t.Fatalf("hex length = %d", len(hex))
	}
	back, err := ParseHex(hex)
	if err != nil {
		t.Fatal(err)
	}
	if back != h {
		t.Fatalf("roundtrip mismatch: %x != %x", back, h)
	}
	// The xet quirk: each 8-byte group is byte-reversed relative to a
	// naive hex dump. Bytes 0..8 are 00 07 0e 15 1c 23 2a 31 little-endian
	// => the u64 is 0x312a231c150e0700.
	if hex[:16] != "312a231c150e0700" {
		t.Fatalf("first group = %s, want 312a231c150e0700 (xet per-u64 ordering)", hex[:16])
	}
}

// buildChunk serializes one chunk per the xorb chunk format.
func buildChunk(t *testing.T, content []byte, scheme byte) []byte {
	t.Helper()
	var payload []byte
	switch scheme {
	case compressionNone:
		payload = content
	case compressionLZ4:
		var buf bytes.Buffer
		w := lz4.NewWriter(&buf)
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		payload = buf.Bytes()
	case compressionBG4LZ4:
		grouped := bg4Split(content)
		var buf bytes.Buffer
		w := lz4.NewWriter(&buf)
		if _, err := w.Write(grouped); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		payload = buf.Bytes()
	}
	header := make([]byte, chunkHeaderLen)
	header[0] = chunkHeaderVersion
	header[1] = byte(len(payload))
	header[2] = byte(len(payload) >> 8)
	header[3] = byte(len(payload) >> 16)
	header[4] = scheme
	header[5] = byte(len(content))
	header[6] = byte(len(content) >> 8)
	header[7] = byte(len(content) >> 16)
	return append(header, payload...)
}

// bg4Split is the forward transform (test-only): bytes at position ≡ p
// (mod 4) go to plane p, remainder distributed to low planes first.
func bg4Split(data []byte) []byte {
	n := len(data)
	split, rem := n/4, n%4
	sizes := [4]int{split, split, split, split}
	for p := range rem {
		sizes[p]++
	}
	out := make([]byte, 0, n)
	for p := range 4 {
		for i := range sizes[p] {
			out = append(out, data[4*i+p])
		}
	}
	return out
}

func TestWalkAndDecodeChunks(t *testing.T) {
	t.Parallel()
	contents := [][]byte{
		bytes.Repeat([]byte("abcd1234"), 512), // 4 KiB, compresses well
		{1, 2, 3, 4, 5, 6, 7},                 // tiny, odd length (bg4 remainder)
		bytes.Repeat([]byte{0xAA}, 100_000),   // near max chunk size
	}
	schemes := []byte{compressionNone, compressionLZ4, compressionBG4LZ4}

	var xorb []byte
	var want [][]byte
	for i, content := range contents {
		for _, scheme := range schemes {
			xorb = append(xorb, buildChunk(t, content, scheme)...)
			want = append(want, content)
			_ = i
		}
	}
	// Append a fake XETBLOB footer: the walk must stop cleanly.
	xorb = append(xorb, []byte("XETBLOB")...)
	xorb = append(xorb, bytes.Repeat([]byte{0}, 90)...)

	chunks, err := WalkChunks(xorb)
	if err != nil {
		t.Fatalf("WalkChunks: %v", err)
	}
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %d, want %d", len(chunks), len(want))
	}

	var out bytes.Buffer
	if err := DecodeChunkRange(xorb, chunks, 0, len(chunks), &out); err != nil {
		t.Fatalf("DecodeChunkRange: %v", err)
	}
	if !bytes.Equal(out.Bytes(), bytes.Join(want, nil)) {
		t.Fatal("decoded content mismatch")
	}

	// Partial range decodes just those chunks.
	out.Reset()
	if err := DecodeChunkRange(xorb, chunks, 3, 6, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), bytes.Join(want[3:6], nil)) {
		t.Fatal("partial range mismatch")
	}
}

func TestBG4Regroup(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 2, 3, 4, 5, 7, 8, 100, 1001, 4096} {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(i * 31)
		}
		if got := bg4Regroup(bg4Split(data)); !bytes.Equal(got, data) {
			t.Fatalf("bg4 roundtrip failed for n=%d", n)
		}
	}
}

// buildShard serializes a minimal MDB shard per the format.
func buildShard(t *testing.T, files []ShardFile, xorbs []ShardXorb) []byte {
	t.Helper()
	var body bytes.Buffer
	u32 := func(v uint32) { _ = binary.Write(&body, binary.LittleEndian, v) }
	u64 := func(v uint64) { _ = binary.Write(&body, binary.LittleEndian, v) }

	// Header.
	body.Write(shardHeaderTag)
	u64(2)      // version
	u64(25 * 8) // footer size: 25 u64-sized words, matching MDBShardFileFooter
	fileInfoOffset := uint64(body.Len())

	for _, f := range files {
		flags := uint32(0)
		if !f.SHA256.IsZero() {
			flags |= fileFlagWithMetadataExt
		}
		body.Write(f.FileHash[:])
		u32(flags)
		u32(uint32(len(f.Terms)))
		u64(0) // unused
		for _, term := range f.Terms {
			body.Write(term.XorbHash[:])
			u32(0) // xorb flags
			u32(uint32(term.UnpackedLen))
			u32(uint32(term.ChunkStart))
			u32(uint32(term.ChunkEnd))
		}
		if flags&fileFlagWithMetadataExt != 0 {
			body.Write(f.SHA256[:])
			u64(0)
			u64(0)
		}
	}
	// File section bookend.
	body.Write(bytes.Repeat([]byte{0xFF}, 32))
	u32(0)
	u32(0)
	u64(0)

	xorbInfoOffset := uint64(body.Len())
	for _, x := range xorbs {
		body.Write(x.XorbHash[:])
		u32(0) // flags
		u32(uint32(x.NumChunks))
		u32(uint32(x.NumBytesInXorb))
		u32(uint32(x.NumBytesOnDisk))
		for range x.NumChunks {
			body.Write(bytes.Repeat([]byte{0x11}, 32)) // chunk hash (unused by parser)
			u32(0)                                     // byte range start
			u32(0)                                     // unpacked
			u32(0)                                     // flags
			u32(0)                                     // unused
		}
	}
	// Xorb section bookend.
	body.Write(bytes.Repeat([]byte{0xFF}, 32))
	u32(0)
	u32(0)
	u64(0)

	lookupOffset := uint64(body.Len())

	// Footer: version + 8 offsets/counts + 32-byte hmac + 2 timestamps +
	// 6 buffer words + 3 byte counts + footer_offset = 45 words total.
	footerStart := uint64(body.Len())
	u64(1) // footer version
	u64(fileInfoOffset)
	u64(xorbInfoOffset)
	u64(lookupOffset) // file lookup offset
	u64(0)            // file lookup entries
	u64(lookupOffset) // xorb lookup offset
	u64(0)
	u64(lookupOffset) // chunk lookup offset
	u64(0)
	body.Write(make([]byte, 32)) // hmac key (zero)
	u64(0)                       // creation timestamp
	u64(^uint64(0))              // expiry
	for range 6 {
		u64(0)
	}
	u64(0) // stored_bytes_on_disk
	u64(0) // materialized_bytes
	u64(0) // stored_bytes
	u64(footerStart)
	return body.Bytes()
}

func TestParseShard(t *testing.T) {
	t.Parallel()
	var fileHash, xorbHash, sha Hash
	for i := range 32 {
		fileHash[i] = byte(i)
		xorbHash[i] = byte(i + 100)
		sha[i] = byte(i + 7)
	}
	files := []ShardFile{{
		FileHash: fileHash,
		SHA256:   sha,
		Terms: []Term{
			{XorbHash: xorbHash, UnpackedLen: 1000, ChunkStart: 0, ChunkEnd: 3},
			{XorbHash: xorbHash, UnpackedLen: 500, ChunkStart: 5, ChunkEnd: 7},
		},
	}}
	xorbs := []ShardXorb{{XorbHash: xorbHash, NumChunks: 7, NumBytesInXorb: 5000, NumBytesOnDisk: 3000}}

	shard, err := ParseShard(buildShard(t, files, xorbs))
	if err != nil {
		t.Fatalf("ParseShard: %v", err)
	}
	if len(shard.Files) != 1 || len(shard.Xorbs) != 1 {
		t.Fatalf("parsed %d files, %d xorbs", len(shard.Files), len(shard.Xorbs))
	}
	f := shard.Files[0]
	if f.FileHash != fileHash || f.SHA256 != sha {
		t.Fatal("file hashes mismatch")
	}
	if len(f.Terms) != 2 || f.Terms[0].ChunkEnd != 3 || f.Terms[1].ChunkStart != 5 || f.Terms[1].UnpackedLen != 500 {
		t.Fatalf("terms = %+v", f.Terms)
	}
	if shard.Xorbs[0].NumChunks != 7 || shard.Xorbs[0].NumBytesOnDisk != 3000 {
		t.Fatalf("xorb = %+v", shard.Xorbs[0])
	}
}

func TestParseShardRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := ParseShard([]byte("definitely not a shard, far too short and wrong")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestStoreRoundtrip(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	content := bytes.Repeat([]byte("weights!"), 1024)
	xorb := buildChunk(t, content, compressionLZ4)
	xorb = append(xorb, []byte("XETBLOB")...)

	var h Hash
	h[0] = 0xAB
	created, err := store.PutXorb(h, xorb)
	if err != nil || !created {
		t.Fatalf("PutXorb = %v, %v", created, err)
	}
	// Idempotent.
	created, err = store.PutXorb(h, xorb)
	if err != nil || created {
		t.Fatalf("second PutXorb = %v, %v", created, err)
	}

	chunks, err := store.XorbChunks(h)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("XorbChunks = %v, %v", chunks, err)
	}
	data, err := store.ReadXorb(h)
	if err != nil || !bytes.Equal(data, xorb) {
		t.Fatalf("ReadXorb mismatch: %v", err)
	}

	rec := &FileRecord{
		FileHash: h.Hex(),
		SHA256:   "aa11bb22cc33aa11bb22cc33aa11bb22cc33aa11bb22cc33aa11bb22cc33dd44",
		TotalLen: int64(len(content)),
		Terms:    []TermRecord{{Xorb: h.Hex(), ChunkStart: 0, ChunkEnd: 1, UnpackedLen: int64(len(content))}},
	}
	if err := store.PutFile(rec); err != nil {
		t.Fatal(err)
	}
	back, err := store.File(h)
	if err != nil || back.SHA256 != rec.SHA256 || len(back.Terms) != 1 {
		t.Fatalf("File = %+v, %v", back, err)
	}
	fileHash, ok := store.FileHashBySHA256(rec.SHA256)
	if !ok || fileHash != h.Hex() {
		t.Fatalf("FileHashBySHA256 = %q, %v", fileHash, ok)
	}
	if _, err := store.File(Hash{0x01}); err != ErrNotFound {
		t.Fatalf("missing file = %v, want ErrNotFound", err)
	}
}

func TestRejectCorruptXorb(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var h Hash
	if _, err := store.PutXorb(h, []byte{9, 9, 9}); err == nil {
		t.Fatal("corrupt xorb accepted")
	}
}
