package xet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrNotFound is returned for unknown xorbs and files.
var ErrNotFound = errors.New("xet: not found")

// Store is a content-addressed directory of xorbs and file reconstruction
// records:
//
//	<root>/xorbs/{xorb-hex}              raw xorb bytes, verbatim as uploaded
//	<root>/xorbs/{xorb-hex}.chunks.json  cached chunk layout (WalkChunks)
//	<root>/files/{file-hex}.json         FileRecord (terms + sha256)
//	<root>/sha256/{sha256-hex}           file containing the xet file hash hex
//
// The store is global (not repo-scoped): hashes are content addresses, so
// identical chunks pushed to different repos share storage — that is the
// dedup story xet exists for. Authorization happens at the API layer.
type Store struct {
	root string
	mu   sync.Mutex // serializes multi-file record writes
}

// NewStore opens (creating if needed) a store rooted at dir.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("xet: store dir is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"xorbs", "files", "sha256"} {
		if err := os.MkdirAll(filepath.Join(abs, sub), 0o755); err != nil {
			return nil, fmt.Errorf("xet: creating store: %w", err)
		}
	}
	return &Store{root: abs}, nil
}

// Root returns the store directory.
func (s *Store) Root() string { return s.root }

func (s *Store) xorbPath(h Hash) string { return filepath.Join(s.root, "xorbs", h.Hex()) }
func (s *Store) filePath(h Hash) string { return filepath.Join(s.root, "files", h.Hex()+".json") }
func (s *Store) shaPath(sha string) string {
	return filepath.Join(s.root, "sha256", strings.ToLower(sha))
}

// HasXorb reports whether a xorb is stored.
func (s *Store) HasXorb(h Hash) bool {
	_, err := os.Stat(s.xorbPath(h))
	return err == nil
}

// PutXorb validates and stores a serialized xorb verbatim, caching its
// chunk layout. Returns false when the xorb was already present.
func (s *Store) PutXorb(h Hash, data []byte) (bool, error) {
	if s.HasXorb(h) {
		return false, nil
	}
	chunks, err := WalkChunks(data)
	if err != nil {
		return false, fmt.Errorf("xet: rejecting xorb %s: %w", h.Hex(), err)
	}
	layout, err := json.Marshal(chunks)
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(s.xorbPath(h)+".chunks.json", layout); err != nil {
		return false, err
	}
	if err := writeFileAtomic(s.xorbPath(h), data); err != nil {
		return false, err
	}
	return true, nil
}

// XorbChunks returns the chunk layout of a stored xorb.
func (s *Store) XorbChunks(h Hash) ([]ChunkInfo, error) {
	data, err := os.ReadFile(s.xorbPath(h) + ".chunks.json")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var chunks []ChunkInfo
	if err := json.Unmarshal(data, &chunks); err != nil {
		return nil, fmt.Errorf("xet: corrupt chunk layout for %s: %w", h.Hex(), err)
	}
	return chunks, nil
}

// OpenXorb opens a stored xorb for reading (supports ranged serving).
func (s *Store) OpenXorb(h Hash) (*os.File, error) {
	f, err := os.Open(s.xorbPath(h))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// ReadXorb reads a stored xorb fully into memory (materialization path;
// xorbs are capped at 64 MiB by the protocol).
func (s *Store) ReadXorb(h Hash) ([]byte, error) {
	data, err := os.ReadFile(s.xorbPath(h))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

// FileRecord is a stored file reconstruction: the ordered terms plus
// identity mappings.
type FileRecord struct {
	// FileHash is the canonical xet hex of the file's merkle hash.
	FileHash string `json:"fileHash"`
	// SHA256 is the standard hex sha256 of the file content ("" when the
	// uploading shard carried no metadata extension).
	SHA256   string       `json:"sha256,omitempty"`
	TotalLen int64        `json:"totalLen"`
	Terms    []TermRecord `json:"terms"`
}

// TermRecord is the stored form of one reconstruction term.
type TermRecord struct {
	Xorb        string `json:"xorb"` // xet hex
	ChunkStart  int    `json:"chunkStart"`
	ChunkEnd    int    `json:"chunkEnd"`
	UnpackedLen int64  `json:"unpackedLen"`
}

// PutFile stores a reconstruction record and its sha256 mapping.
func (s *Store) PutFile(rec *FileRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := ParseHex(rec.FileHash)
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.filePath(h), payload); err != nil {
		return err
	}
	if rec.SHA256 != "" {
		if err := writeFileAtomic(s.shaPath(rec.SHA256), []byte(rec.FileHash)); err != nil {
			return err
		}
	}
	return nil
}

// File loads a reconstruction record by xet file hash.
func (s *Store) File(h Hash) (*FileRecord, error) {
	data, err := os.ReadFile(s.filePath(h))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var rec FileRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("xet: corrupt file record %s: %w", h.Hex(), err)
	}
	return &rec, nil
}

// FileHashBySHA256 returns the xet file hash for a content sha256, powering
// X-Xet-Hash advertisement on resolve.
func (s *Store) FileHashBySHA256(sha256hex string) (string, bool) {
	data, err := os.ReadFile(s.shaPath(sha256hex))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
