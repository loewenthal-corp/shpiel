package xet

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// rawShard builds shard bytes from hand-assembled sections, for exercising
// exact layouts buildShard (which always bookends and footers) cannot.
type rawShard struct{ buf bytes.Buffer }

func (r *rawShard) header(footerSize uint64) *rawShard {
	r.buf.Write(shardHeaderTag)
	_ = binary.Write(&r.buf, binary.LittleEndian, uint64(2)) // version
	_ = binary.Write(&r.buf, binary.LittleEndian, footerSize)
	return r
}

// record writes one 48-byte record: a 32-byte hash then four u32 fields.
func (r *rawShard) record(hash byte, fields ...uint32) *rawShard {
	r.buf.Write(bytes.Repeat([]byte{hash}, 32))
	for i := range 4 {
		v := uint32(0)
		if i < len(fields) {
			v = fields[i]
		}
		_ = binary.Write(&r.buf, binary.LittleEndian, v)
	}
	return r
}

func (r *rawShard) bookend() *rawShard { return r.record(0xFF) }

// footer32 appends the minimal 32-byte footer the parser reads: a version
// word and the three section offsets.
func (r *rawShard) footer32(fileInfo, xorbInfo, fileLookup uint64) *rawShard {
	for _, v := range []uint64{1, fileInfo, xorbInfo, fileLookup} {
		_ = binary.Write(&r.buf, binary.LittleEndian, v)
	}
	return r
}

func (r *rawShard) bytes() []byte { return r.buf.Bytes() }

// TestParseShardSequential covers the layout shipping hf_xet wheels send:
// footer_size zero, sections back to back, each ending in a bookend.
func TestParseShardSequential(t *testing.T) {
	t.Parallel()
	var s rawShard
	s.header(0)
	// One file: verification flag + metadata ext, one term.
	s.record(0x01, fileFlagWithVerification|fileFlagWithMetadataExt, 1)
	s.record(0x02, 0, 700, 0, 3) // term: xorb 0x02.., unpacked 700, chunks [0,3)
	s.record(0x03)               // verification record (skipped)
	s.record(0x04)               // metadata ext: sha256 = 0x04..
	s.bookend()
	// One xorb with two chunk entries.
	s.record(0x02, 0, 2, 1400, 900)
	s.record(0x05)
	s.record(0x06)
	s.bookend()

	shard, err := ParseShard(s.bytes())
	if err != nil {
		t.Fatalf("ParseShard: %v", err)
	}
	if len(shard.Files) != 1 || len(shard.Xorbs) != 1 {
		t.Fatalf("parsed %d files, %d xorbs, want 1 and 1", len(shard.Files), len(shard.Xorbs))
	}
	f := shard.Files[0]
	if f.FileHash[0] != 0x01 || f.SHA256[0] != 0x04 {
		t.Fatalf("file hash %x, sha %x", f.FileHash[0], f.SHA256[0])
	}
	if len(f.Terms) != 1 || f.Terms[0].UnpackedLen != 700 || f.Terms[0].ChunkEnd != 3 {
		t.Fatalf("terms = %+v", f.Terms)
	}
	x := shard.Xorbs[0]
	if x.NumChunks != 2 || x.NumBytesInXorb != 1400 || x.NumBytesOnDisk != 900 {
		t.Fatalf("xorb = %+v", x)
	}
}

// TestParseShardHeaderOnly: a header-only sequential shard (48 bytes
// exactly) is valid and empty.
func TestParseShardHeaderOnly(t *testing.T) {
	t.Parallel()
	var s rawShard
	data := s.header(0).bytes()
	if len(data) != shardHeaderLen {
		t.Fatalf("header = %d bytes, want %d", len(data), shardHeaderLen)
	}
	shard, err := ParseShard(data)
	if err != nil {
		t.Fatalf("ParseShard: %v", err)
	}
	if len(shard.Files) != 0 || len(shard.Xorbs) != 0 {
		t.Fatalf("parsed %d files, %d xorbs, want none", len(shard.Files), len(shard.Xorbs))
	}
}

// TestParseShardFooteredEmpty: all three section offsets may legitimately
// coincide at the end of the data (empty sections, footer only).
func TestParseShardFooteredEmpty(t *testing.T) {
	t.Parallel()
	var s rawShard
	s.header(32)
	end := uint64(shardHeaderLen + 32)
	s.footer32(end, end, end)
	shard, err := ParseShard(s.bytes())
	if err != nil {
		t.Fatalf("ParseShard: %v", err)
	}
	if len(shard.Files) != 0 || len(shard.Xorbs) != 0 {
		t.Fatalf("parsed %d files, %d xorbs, want none", len(shard.Files), len(shard.Xorbs))
	}
}

// TestParseShardFooteredExactSections: footered sections carry no bookends
// when the offsets bound them exactly; records ending flush with the
// section end must parse.
func TestParseShardFooteredExactSections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		file      func(s *rawShard) // file-section records
		fileLen   int               // in 48-byte records
		wantTerms int
		wantSHA   byte
	}{
		{
			name:    "file header only",
			file:    func(s *rawShard) { s.record(0x01, 0, 0) },
			fileLen: 1,
		},
		{
			name: "term ends at section end",
			file: func(s *rawShard) {
				s.record(0x01, 0, 1)
				s.record(0x02, 0, 500, 0, 2)
			},
			fileLen:   2,
			wantTerms: 1,
		},
		{
			name: "metadata ext ends at section end",
			file: func(s *rawShard) {
				s.record(0x01, fileFlagWithMetadataExt, 0)
				s.record(0x04)
			},
			fileLen: 2,
			wantSHA: 0x04,
		},
		{
			name: "verification records end at section end",
			file: func(s *rawShard) {
				s.record(0x01, fileFlagWithVerification, 1)
				s.record(0x02, 0, 500, 0, 2)
				s.record(0x03)
			},
			fileLen:   3,
			wantTerms: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var s rawShard
			s.header(32)
			fileInfo := uint64(shardHeaderLen)
			xorbInfo := fileInfo + uint64(tc.fileLen*shardRecordLen)
			tc.file(&s)
			// Xorb section: one xorb with one chunk entry, no bookend.
			s.record(0x07, 0, 1, 800, 600)
			s.record(0x08)
			fileLookup := xorbInfo + 2*shardRecordLen
			s.footer32(fileInfo, xorbInfo, fileLookup)

			shard, err := ParseShard(s.bytes())
			if err != nil {
				t.Fatalf("ParseShard: %v", err)
			}
			if len(shard.Files) != 1 {
				t.Fatalf("parsed %d files, want 1", len(shard.Files))
			}
			f := shard.Files[0]
			if len(f.Terms) != tc.wantTerms {
				t.Fatalf("terms = %+v, want %d", f.Terms, tc.wantTerms)
			}
			if f.SHA256[0] != tc.wantSHA {
				t.Fatalf("sha[0] = %x, want %x", f.SHA256[0], tc.wantSHA)
			}
			if len(shard.Xorbs) != 1 || shard.Xorbs[0].NumChunks != 1 || shard.Xorbs[0].NumBytesOnDisk != 600 {
				t.Fatalf("xorbs = %+v", shard.Xorbs)
			}
		})
	}
}

// TestParseShardZeroChunkXorb: a xorb declaring zero chunks is plausible
// (an empty upload), not an error.
func TestParseShardZeroChunkXorb(t *testing.T) {
	t.Parallel()
	var s rawShard
	s.header(0)
	s.bookend() // empty file section
	s.record(0x07, 0, 0, 0, 0)
	s.bookend()
	shard, err := ParseShard(s.bytes())
	if err != nil {
		t.Fatalf("ParseShard: %v", err)
	}
	if len(shard.Xorbs) != 1 || shard.Xorbs[0].NumChunks != 0 {
		t.Fatalf("xorbs = %+v", shard.Xorbs)
	}
}

func TestParseShardMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		data []byte
		want string // substring of the error
	}{
		{
			name: "too short",
			data: []byte("tiny"),
			want: "too short",
		},
		{
			name: "bad magic",
			data: bytes.Repeat([]byte{0xAB}, shardHeaderLen),
			want: "bad magic",
		},
		{
			name: "footer size beyond data",
			data: new(rawShard).header(uint64(shardHeaderLen) + 1).bytes(),
			want: "implausible shard footer size",
		},
		{
			// footerSize == len(data) is not "implausible" by the size
			// check; it fails later because the offsets it yields (magic
			// bytes) are garbage.
			name: "footer covering whole shard",
			data: new(rawShard).header(uint64(shardHeaderLen)).bytes(),
			want: "out of order",
		},
		{
			name: "footer too short",
			data: func() []byte {
				var s rawShard
				s.header(8)
				_ = binary.Write(&s.buf, binary.LittleEndian, uint64(0))
				return s.bytes()
			}(),
			want: "footer too short",
		},
		{
			name: "offsets out of order",
			data: func() []byte {
				var s rawShard
				s.header(32)
				end := uint64(shardHeaderLen + 32)
				return s.footer32(end, end-8, end).bytes()
			}(),
			want: "out of order",
		},
		{
			// Entry count exactly at the plausibility cap passes the cap
			// check and dies on truncation instead.
			name: "file entry count at cap truncates",
			data: func() []byte {
				var s rawShard
				s.header(0)
				return s.record(0x01, 0, 1<<20).bytes()
			}(),
			want: "file section truncated",
		},
		{
			name: "file entry count beyond cap",
			data: func() []byte {
				var s rawShard
				s.header(0)
				return s.record(0x01, 0, 1<<20+1).bytes()
			}(),
			want: "implausible",
		},
		{
			name: "file term truncated",
			data: func() []byte {
				var s rawShard
				s.header(0)
				s.record(0x01, 0, 2)
				s.record(0x02, 0, 500, 0, 2) // one term present, one missing
				return s.bytes()
			}(),
			want: "file section truncated",
		},
		{
			name: "metadata ext truncated",
			data: func() []byte {
				var s rawShard
				s.header(0)
				return s.record(0x01, fileFlagWithMetadataExt, 0).bytes()
			}(),
			want: "metadata ext truncated",
		},
		{
			name: "verification overruns section",
			data: func() []byte {
				var s rawShard
				s.header(0)
				s.record(0x01, fileFlagWithVerification, 1)
				s.record(0x02, 0, 500, 0, 2)
				// Verification record missing: skip lands past the end.
				return s.bytes()
			}(),
			want: "file section truncated",
		},
		{
			name: "xorb entry count at cap truncates",
			data: func() []byte {
				var s rawShard
				s.header(0)
				s.bookend() // empty file section
				return s.record(0x07, 0, 1<<20, 800, 600).bytes()
			}(),
			want: "xorb section truncated",
		},
		{
			name: "xorb entry count beyond cap",
			data: func() []byte {
				var s rawShard
				s.header(0)
				s.bookend()
				return s.record(0x07, 0, 1<<20+1, 800, 600).bytes()
			}(),
			want: "implausible",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseShard(tc.data)
			if err == nil {
				t.Fatal("malformed shard accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err, tc.want)
			}
		})
	}
}
