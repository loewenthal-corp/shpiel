package xet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/loewenthal-corp/shpiel/internal/s3client"
)

// ErrNotFound is returned for unknown xorbs and files.
var ErrNotFound = errors.New("xet: not found")

// Store is a content-addressed collection of xorbs and file reconstruction
// records:
//
//	xorbs/{xorb-hex}              raw xorb bytes, verbatim as uploaded
//	xorbs/{xorb-hex}.chunks.json  cached chunk layout (WalkChunks)
//	files/{file-hex}.json         FileRecord (terms + sha256)
//	sha256/{sha256-hex}           object containing the xet file hash hex
//	shards/{sha256-hex}           raw shard bytes (global-dedup answers)
//	chunks/{chunk-hex}            object containing the shard key holding
//	                              this dedup-eligible chunk
//
// The keys live on a byte-persistence layer that is either a local
// directory (NewStore) or an S3-compatible bucket (NewBucketStore) — the
// spec's "the S3 backend doubles as the xorb store" story.
//
// The store is global (not repo-scoped): hashes are content addresses, so
// identical chunks pushed to different repos share storage — that is the
// dedup story xet exists for. Authorization happens at the API layer.
type Store struct {
	objects objects
	mu      sync.Mutex // serializes multi-file record writes
}

// objects is the byte-persistence layer under Store: small immutable
// objects addressed by slash-separated keys. Implementations map
// ErrNotFound onto missing keys.
type objects interface {
	has(ctx context.Context, key string) (bool, error)
	read(ctx context.Context, key string) ([]byte, error)
	open(ctx context.Context, key string) (io.ReadSeekCloser, error)
	// write must be atomic: readers never observe a partial object.
	write(ctx context.Context, key string, data []byte) error
}

// NewStore opens (creating if needed) a store rooted at a local directory.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("xet: store dir is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"xorbs", "files", "sha256", "shards", "chunks"} {
		if err := os.MkdirAll(filepath.Join(abs, sub), 0o755); err != nil {
			return nil, fmt.Errorf("xet: creating store: %w", err)
		}
	}
	return &Store{objects: diskObjects{root: abs}}, nil
}

// NewBucketStore opens a store on an S3-compatible bucket, keyed under
// prefix (e.g. "xet"). The bucket persists nothing locally, so N replicas
// can share one xorb store.
func NewBucketStore(client *s3client.Client, prefix string) *Store {
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &Store{objects: bucketObjects{client: client, prefix: prefix}}
}

func xorbKey(h Hash) string       { return "xorbs/" + h.Hex() }
func chunksKey(h Hash) string     { return "xorbs/" + h.Hex() + ".chunks.json" }
func fileKey(h Hash) string       { return "files/" + h.Hex() + ".json" }
func shaKey(sha string) string    { return "sha256/" + strings.ToLower(sha) }
func chunkIndexKey(h Hash) string { return "chunks/" + h.Hex() }

// HasXorb reports whether a xorb is stored (errors read as absent — the
// caller's fallback is a re-upload, which is idempotent).
func (s *Store) HasXorb(ctx context.Context, h Hash) bool {
	ok, err := s.objects.has(ctx, xorbKey(h))
	return err == nil && ok
}

// PutXorb validates and stores a serialized xorb verbatim, caching its
// chunk layout. Returns false when the xorb was already present.
func (s *Store) PutXorb(ctx context.Context, h Hash, data []byte) (bool, error) {
	if s.HasXorb(ctx, h) {
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
	// Layout first: a visible xorb always has its layout.
	if err := s.objects.write(ctx, chunksKey(h), layout); err != nil {
		return false, err
	}
	if err := s.objects.write(ctx, xorbKey(h), data); err != nil {
		return false, err
	}
	return true, nil
}

// XorbChunks returns the chunk layout of a stored xorb.
func (s *Store) XorbChunks(ctx context.Context, h Hash) ([]ChunkInfo, error) {
	data, err := s.objects.read(ctx, chunksKey(h))
	if err != nil {
		return nil, err
	}
	var chunks []ChunkInfo
	if err := json.Unmarshal(data, &chunks); err != nil {
		return nil, fmt.Errorf("xet: corrupt chunk layout for %s: %w", h.Hex(), err)
	}
	return chunks, nil
}

// OpenXorb opens a stored xorb for reading (supports ranged serving).
func (s *Store) OpenXorb(ctx context.Context, h Hash) (io.ReadSeekCloser, error) {
	return s.objects.open(ctx, xorbKey(h))
}

// ReadXorb reads a stored xorb fully into memory (materialization path;
// xorb size is capped by the protocol).
func (s *Store) ReadXorb(ctx context.Context, h Hash) ([]byte, error) {
	return s.objects.read(ctx, xorbKey(h))
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
func (s *Store) PutFile(ctx context.Context, rec *FileRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := ParseHex(rec.FileHash)
	if err != nil {
		return err
	}
	if rec.SHA256 != "" && !sha256HexPattern.MatchString(rec.SHA256) {
		return fmt.Errorf("xet: file record sha256 %q is not a sha256 hex", rec.SHA256)
	}
	payload, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := s.objects.write(ctx, fileKey(h), payload); err != nil {
		return err
	}
	if rec.SHA256 != "" {
		if err := s.objects.write(ctx, shaKey(rec.SHA256), []byte(rec.FileHash)); err != nil {
			return err
		}
	}
	return nil
}

// File loads a reconstruction record by xet file hash.
func (s *Store) File(ctx context.Context, h Hash) (*FileRecord, error) {
	data, err := s.objects.read(ctx, fileKey(h))
	if err != nil {
		return nil, err
	}
	var rec FileRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("xet: corrupt file record %s: %w", h.Hex(), err)
	}
	return &rec, nil
}

// PutDedupShard stores raw shard bytes and points each dedup-eligible
// chunk at them: the shard itself is the global-dedup query answer (the
// same trick xet-core's reference server uses — clients parse the shard
// they get back exactly like one they would upload). Shard first, then
// the chunk pointers, so a visible pointer always resolves.
func (s *Store) PutDedupShard(ctx context.Context, shard []byte, chunks []Hash) error {
	if len(chunks) == 0 {
		return nil
	}
	key := "shards/" + sha256Hex(shard)
	if err := s.objects.write(ctx, key, shard); err != nil {
		return err
	}
	for _, chunk := range chunks {
		if err := s.objects.write(ctx, chunkIndexKey(chunk), []byte(key)); err != nil {
			return err
		}
	}
	return nil
}

var shardKeyPattern = regexp.MustCompile(`^shards/[0-9a-f]{64}$`)

// DedupShard returns the stored shard covering a dedup-eligible chunk, or
// ErrNotFound.
func (s *Store) DedupShard(ctx context.Context, chunk Hash) ([]byte, error) {
	pointer, err := s.objects.read(ctx, chunkIndexKey(chunk))
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(string(pointer))
	if !shardKeyPattern.MatchString(key) {
		return nil, fmt.Errorf("xet: corrupt chunk index %s: pointer %q", chunk.Hex(), key)
	}
	return s.objects.read(ctx, key)
}

var sha256HexPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// FileHashBySHA256 returns the xet file hash for a content sha256, powering
// X-Xet-Hash advertisement on resolve. The input flows from manifest LFS
// metadata (potentially upstream-controlled), so anything but a sha256 hex
// is a miss — it must never reach a storage key.
func (s *Store) FileHashBySHA256(ctx context.Context, sha256hex string) (string, bool) {
	if !sha256HexPattern.MatchString(sha256hex) {
		return "", false
	}
	data, err := s.objects.read(ctx, shaKey(sha256hex))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// --- local-directory persistence ---

type diskObjects struct{ root string }

// path maps a store key onto the root. Keys are built from fixed names
// plus validated hex (ParseHex hashes, the sha256HexPattern guard), so
// they cannot traverse; the gosec suppressions below record that.
func (d diskObjects) path(key string) string {
	return filepath.Join(d.root, filepath.FromSlash(key))
}

func (d diskObjects) has(_ context.Context, key string) (bool, error) {
	_, err := os.Stat(d.path(key)) // #nosec G703 -- keys are fixed names + validated hex
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (d diskObjects) read(_ context.Context, key string) ([]byte, error) {
	data, err := os.ReadFile(d.path(key)) // #nosec G703 -- keys are fixed names + validated hex
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

func (d diskObjects) open(_ context.Context, key string) (io.ReadSeekCloser, error) {
	f, err := os.Open(d.path(key)) // #nosec G703 -- keys are fixed names + validated hex
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (d diskObjects) write(_ context.Context, key string, data []byte) error {
	path := d.path(key)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*") // #nosec G703 -- keys are fixed names + validated hex
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // #nosec G703 -- keys are fixed names + validated hex
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path) // #nosec G703 -- keys are fixed names + validated hex
}

// --- bucket persistence ---

type bucketObjects struct {
	client *s3client.Client
	prefix string // "" or slash-terminated
}

func (b bucketObjects) key(key string) string { return b.prefix + key }

func (b bucketObjects) has(ctx context.Context, key string) (bool, error) {
	_, err := b.client.Head(ctx, b.key(key))
	if errors.Is(err, s3client.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (b bucketObjects) read(ctx context.Context, key string) ([]byte, error) {
	rc, err := b.client.Get(ctx, b.key(key), 0)
	if err != nil {
		if errors.Is(err, s3client.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (b bucketObjects) open(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	rc, err := b.client.OpenRanged(ctx, b.key(key))
	if errors.Is(err, s3client.ErrNotFound) {
		return nil, ErrNotFound
	}
	return rc, err
}

func (b bucketObjects) write(ctx context.Context, key string, data []byte) error {
	return b.client.Put(ctx, b.key(key), strings.NewReader(string(data)), int64(len(data)), sha256Hex(data))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
