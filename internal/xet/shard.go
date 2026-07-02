package xet

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// MDB shard format (xet_core_structures/metadata_shard): a header with a
// 32-byte magic tag, a file-info section, a xorb-info section, lookup
// tables, and a footer whose offsets locate the sections. All integers are
// little-endian; all records are 48 bytes.

// shardHeaderTag is MDB_SHARD_HEADER_TAG from xet-core.
var shardHeaderTag = []byte{
	'H', 'F', 'R', 'e', 'p', 'o', 'M', 'e', 't', 'a', 'D', 'a', 't', 'a', 0,
	85, 105, 103, 69, 106, 123, 129, 87, 131, 165, 189, 217, 92, 205, 209, 74, 169,
}

const (
	shardHeaderLen = 32 + 8 + 8 // tag + version + footer_size
	shardRecordLen = 48

	fileFlagWithVerification = uint32(1) << 31
	fileFlagWithMetadataExt  = uint32(1) << 30
)

// Shard is the parsed subset of an MDB shard Shpiel acts on.
type Shard struct {
	Files []ShardFile
	Xorbs []ShardXorb
}

// ShardFile describes one file: its xet hash, its sha256 (when the client
// attached metadata), and the ordered terms reconstructing it from xorbs.
type ShardFile struct {
	FileHash Hash
	// SHA256 of the file content; zero when the shard has no metadata
	// extension.
	SHA256 Hash
	Terms  []Term
}

// Term is one reconstruction step: chunks [ChunkStart, ChunkEnd) of a xorb.
type Term struct {
	XorbHash    Hash
	UnpackedLen int64
	ChunkStart  int
	ChunkEnd    int
}

// ShardXorb is the shard's declaration of a xorb's chunk sequence.
type ShardXorb struct {
	XorbHash  Hash
	NumChunks int
	// NumBytesInXorb is the total unpacked length; NumBytesOnDisk the
	// serialized (chunk stream) length.
	NumBytesInXorb int64
	NumBytesOnDisk int64
}

// ParseShard parses a serialized MDB shard. Two layouts exist in the wild:
//
//   - footered (xet-core main): the header's footer_size field is set and
//     the footer's offsets locate the sections;
//   - sequential (shipping hf_xet wheels): footer_size is zero and the
//     file-info and xorb-info sections simply follow the header, each
//     terminated by an all-ones bookend record.
func ParseShard(data []byte) (*Shard, error) {
	if len(data) < shardHeaderLen {
		return nil, fmt.Errorf("xet: shard too short (%d bytes)", len(data))
	}
	if !bytes.Equal(data[:32], shardHeaderTag) {
		return nil, fmt.Errorf("xet: not an MDB shard (bad magic)")
	}
	footerSize := binary.LittleEndian.Uint64(data[40:48])
	if footerSize > uint64(len(data)) {
		return nil, fmt.Errorf("xet: implausible shard footer size %d", footerSize)
	}

	shard := &Shard{}
	if footerSize == 0 {
		// Sequential layout: sections run back to back after the header.
		consumed, err := parseFileSection(data[shardHeaderLen:], shard)
		if err != nil {
			return nil, err
		}
		if _, err := parseXorbSection(data[shardHeaderLen+consumed:], shard); err != nil {
			return nil, err
		}
		return shard, nil
	}

	footer := data[uint64(len(data))-footerSize:]
	if len(footer) < 4*8 {
		return nil, fmt.Errorf("xet: shard footer too short")
	}
	fileInfoOffset := binary.LittleEndian.Uint64(footer[8:16])
	xorbInfoOffset := binary.LittleEndian.Uint64(footer[16:24])
	fileLookupOffset := binary.LittleEndian.Uint64(footer[24:32])
	if fileInfoOffset > uint64(len(data)) || xorbInfoOffset > uint64(len(data)) ||
		fileLookupOffset > uint64(len(data)) || fileInfoOffset > xorbInfoOffset || xorbInfoOffset > fileLookupOffset {
		return nil, fmt.Errorf("xet: shard section offsets out of order (%d, %d, %d)",
			fileInfoOffset, xorbInfoOffset, fileLookupOffset)
	}
	if _, err := parseFileSection(data[fileInfoOffset:xorbInfoOffset], shard); err != nil {
		return nil, err
	}
	if _, err := parseXorbSection(data[xorbInfoOffset:fileLookupOffset], shard); err != nil {
		return nil, err
	}
	return shard, nil
}

// parseFileSection reads FileDataSequenceHeader records and their entries
// until the bookend (all-ones hash) or the section end. Returns the offset
// just past the bookend record.
func parseFileSection(section []byte, shard *Shard) (int, error) {
	off := 0
	for off+shardRecordLen <= len(section) {
		var fileHash Hash
		copy(fileHash[:], section[off:off+32])
		if fileHash.IsBookend() {
			off += shardRecordLen
			break
		}
		flags := binary.LittleEndian.Uint32(section[off+32 : off+36])
		numEntries := int(binary.LittleEndian.Uint32(section[off+36 : off+40]))
		off += shardRecordLen

		if numEntries < 0 || numEntries > 1<<20 {
			return 0, fmt.Errorf("xet: shard file entry count %d implausible", numEntries)
		}
		file := ShardFile{FileHash: fileHash}
		for range numEntries {
			if off+shardRecordLen > len(section) {
				return 0, fmt.Errorf("xet: shard file section truncated")
			}
			var xorbHash Hash
			copy(xorbHash[:], section[off:off+32])
			unpacked := binary.LittleEndian.Uint32(section[off+36 : off+40])
			start := binary.LittleEndian.Uint32(section[off+40 : off+44])
			end := binary.LittleEndian.Uint32(section[off+44 : off+48])
			file.Terms = append(file.Terms, Term{
				XorbHash:    xorbHash,
				UnpackedLen: int64(unpacked),
				ChunkStart:  int(start),
				ChunkEnd:    int(end),
			})
			off += shardRecordLen
		}
		if flags&fileFlagWithVerification != 0 {
			// One verification record per term; not consumed by the server.
			off += numEntries * shardRecordLen
		}
		if flags&fileFlagWithMetadataExt != 0 {
			if off+shardRecordLen > len(section) {
				return 0, fmt.Errorf("xet: shard metadata ext truncated")
			}
			copy(file.SHA256[:], section[off:off+32])
			off += shardRecordLen
		}
		if off > len(section) {
			return 0, fmt.Errorf("xet: shard file section truncated")
		}
		shard.Files = append(shard.Files, file)
	}
	return off, nil
}

// parseXorbSection reads XorbChunkSequenceHeader records and skips their
// chunk entries (boundaries come from walking the xorb bytes themselves).
// Returns the offset just past the bookend record.
func parseXorbSection(section []byte, shard *Shard) (int, error) {
	off := 0
	for off+shardRecordLen <= len(section) {
		var xorbHash Hash
		copy(xorbHash[:], section[off:off+32])
		if xorbHash.IsBookend() {
			off += shardRecordLen
			break
		}
		numEntries := int(binary.LittleEndian.Uint32(section[off+36 : off+40]))
		bytesInXorb := binary.LittleEndian.Uint32(section[off+40 : off+44])
		bytesOnDisk := binary.LittleEndian.Uint32(section[off+44 : off+48])
		off += shardRecordLen

		if numEntries < 0 || numEntries > 1<<20 {
			return 0, fmt.Errorf("xet: shard xorb entry count %d implausible", numEntries)
		}
		shard.Xorbs = append(shard.Xorbs, ShardXorb{
			XorbHash:       xorbHash,
			NumChunks:      numEntries,
			NumBytesInXorb: int64(bytesInXorb),
			NumBytesOnDisk: int64(bytesOnDisk),
		})
		off += numEntries * shardRecordLen
		if off > len(section) {
			return 0, fmt.Errorf("xet: shard xorb section truncated")
		}
	}
	return off, nil
}
