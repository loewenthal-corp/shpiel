// Package xet implements the server side of the Xet protocol: the CAS HTTP
// API (xorb/shard ingest, file reconstruction), the binary formats clients
// produce (xorb chunk streams, MDB shards), and a content-addressed store.
//
// Shpiel never chunks or hashes content itself — hf_xet clients compute all
// merkle hashes and chunk layouts. The server stores uploaded xorbs
// verbatim, parses shard metadata to learn file -> chunk mappings, and
// serves back exactly the bytes clients uploaded, which keeps client-side
// verification intact without reimplementing xet's hashing.
//
// Formats follow huggingface/xet-core (xet_core_structures); see the
// format comments on each type.
package xet

import (
	"encoding/binary"
	"fmt"
)

// Hash is a 32-byte xet merkle hash (file hash, xorb hash, or chunk hash).
//
// String conversion quirk inherited from xet-core's DataHash: the canonical
// hex form treats the 32 bytes as four little-endian u64s and prints each
// as a 16-digit hex number — so every 8-byte group is byte-reversed
// relative to a naive hex dump. All URLs, JSON payloads, and lookups use
// this canonical form.
type Hash [32]byte

// Hex renders the canonical xet hex form.
func (h Hash) Hex() string {
	return fmt.Sprintf("%016x%016x%016x%016x",
		binary.LittleEndian.Uint64(h[0:8]),
		binary.LittleEndian.Uint64(h[8:16]),
		binary.LittleEndian.Uint64(h[16:24]),
		binary.LittleEndian.Uint64(h[24:32]),
	)
}

// ParseHex parses the canonical xet hex form.
func ParseHex(s string) (Hash, error) {
	var h Hash
	if len(s) != 64 {
		return h, fmt.Errorf("xet: hash %q is not 64 hex chars", s)
	}
	for i := range 4 {
		var v uint64
		if _, err := fmt.Sscanf(s[i*16:(i+1)*16], "%016x", &v); err != nil {
			return h, fmt.Errorf("xet: invalid hash %q: %v", s, err)
		}
		binary.LittleEndian.PutUint64(h[i*8:(i+1)*8], v)
	}
	return h, nil
}

// IsBookend reports whether the hash is the all-ones sentinel that
// terminates shard sections.
func (h Hash) IsBookend() bool {
	for _, b := range h {
		if b != 0xFF {
			return false
		}
	}
	return true
}

// IsZero reports whether the hash is all zeroes.
func (h Hash) IsZero() bool {
	return h == Hash{}
}
