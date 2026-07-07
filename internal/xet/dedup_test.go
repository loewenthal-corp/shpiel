package xet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/loewenthal-corp/shpiel/internal/fakes3"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/s3client"
)

// eligibleChunkHash builds a chunk hash whose fourth little-endian u64
// limb is n*1024 — global-dedup eligible by construction, distinct per n.
func eligibleChunkHash(n uint64) Hash {
	var h Hash
	h[0] = 0xC0 // keep it distinct from the zero hash
	binary.LittleEndian.PutUint64(h[24:32], n*globalDedupChunkModulus)
	return h
}

// TestGlobalDedupEligible pins the criterion against xet-core's DataHash
// semantics: the fourth LE u64 limb, modulo 1024.
func TestGlobalDedupEligible(t *testing.T) {
	t.Parallel()
	var zero Hash
	if !zero.GlobalDedupEligible() {
		t.Error("zero hash (limb 0) must be eligible")
	}
	if !eligibleChunkHash(1).GlobalDedupEligible() {
		t.Error("limb 1024 must be eligible")
	}
	var almost Hash
	binary.LittleEndian.PutUint64(almost[24:32], globalDedupChunkModulus-1)
	if almost.GlobalDedupEligible() {
		t.Error("limb 1023 must not be eligible")
	}
	var bigLowLimbs Hash
	for i := range 24 { // only the fourth limb participates
		bigLowLimbs[i] = 0xFF
	}
	if !bigLowLimbs.GlobalDedupEligible() {
		t.Error("eligibility must ignore the first three limbs")
	}
}

// TestParseShardChunkHashes: the xorb section's chunk records surface
// their hashes in declaration order.
func TestParseShardChunkHashes(t *testing.T) {
	t.Parallel()
	var xorbHash Hash
	xorbHash[0] = 0xAB
	chunks := []Hash{eligibleChunkHash(1), {0x22}, eligibleChunkHash(2)}
	shardBytes := buildShard(t, nil,
		[]ShardXorb{{XorbHash: xorbHash, NumChunks: 3, NumBytesInXorb: 30, ChunkHashes: chunks}})
	shard, err := ParseShard(shardBytes)
	if err != nil {
		t.Fatalf("ParseShard: %v", err)
	}
	if len(shard.Xorbs) != 1 || len(shard.Xorbs[0].ChunkHashes) != 3 {
		t.Fatalf("xorbs = %+v", shard.Xorbs)
	}
	for i, want := range chunks {
		if shard.Xorbs[0].ChunkHashes[i] != want {
			t.Errorf("chunk %d = %s, want %s", i, shard.Xorbs[0].ChunkHashes[i].Hex(), want.Hex())
		}
	}
}

// TestParseXorbSectionTruncated: a xorb header declaring more chunk
// records than the section holds must error, not read out of bounds.
func TestParseXorbSectionTruncated(t *testing.T) {
	t.Parallel()
	var section bytes.Buffer
	var xorbHash Hash
	xorbHash[0] = 0xAB
	section.Write(xorbHash[:])
	_ = binary.Write(&section, binary.LittleEndian, uint32(0)) // flags
	_ = binary.Write(&section, binary.LittleEndian, uint32(2)) // two chunks declared
	_ = binary.Write(&section, binary.LittleEndian, uint32(16))
	_ = binary.Write(&section, binary.LittleEndian, uint32(16))
	section.Write(make([]byte, shardRecordLen)) // ...but only one present
	if _, err := parseXorbSection(section.Bytes(), &Shard{}); err == nil {
		t.Fatal("truncated xorb section accepted")
	}
}

// TestDedupShardStore runs the index round-trip on both persistence
// layers.
func TestDedupShardStore(t *testing.T) {
	t.Parallel()
	for name, store := range storeVariants(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			shard := []byte("pretend shard bytes")
			chunkA, chunkB := eligibleChunkHash(1), eligibleChunkHash(2)

			if _, err := store.DedupShard(ctx, chunkA); !errors.Is(err, ErrNotFound) {
				t.Fatalf("miss = %v, want ErrNotFound", err)
			}
			if err := store.PutDedupShard(ctx, shard, []Hash{chunkA, chunkB}); err != nil {
				t.Fatalf("PutDedupShard: %v", err)
			}
			for _, c := range []Hash{chunkA, chunkB} {
				got, err := store.DedupShard(ctx, c)
				if err != nil || !bytes.Equal(got, shard) {
					t.Errorf("DedupShard(%s) = %q, %v", c.Hex(), got, err)
				}
			}
			// A second shard with an overlapping chunk repoints it.
			shard2 := []byte("newer shard bytes")
			if err := store.PutDedupShard(ctx, shard2, []Hash{chunkB}); err != nil {
				t.Fatal(err)
			}
			if got, _ := store.DedupShard(ctx, chunkB); !bytes.Equal(got, shard2) {
				t.Errorf("repointed chunk = %q, want newer shard", got)
			}
			if got, _ := store.DedupShard(ctx, chunkA); !bytes.Equal(got, shard) {
				t.Errorf("untouched chunk = %q, want original shard", got)
			}
		})
	}
}

// TestDedupShardStoreNoChunksIsNoOp: shards without eligible chunks are
// not stored at all.
func TestDedupShardStoreNoChunksIsNoOp(t *testing.T) {
	t.Parallel()
	store, fake := newBucketTestStoreWithFake(t, "xet")
	if err := store.PutDedupShard(context.Background(), []byte("bytes"), nil); err != nil {
		t.Fatal(err)
	}
	if keys := fake.Keys(); len(keys) != 0 {
		t.Errorf("no-op PutDedupShard wrote %v", keys)
	}
}

// TestDedupShardCorruptPointer: a chunk pointer that is not a well-formed
// shard key must error, never be used as a storage key.
func TestDedupShardCorruptPointer(t *testing.T) {
	t.Parallel()
	store, fake := newBucketTestStoreWithFake(t, "xet")
	chunk := eligibleChunkHash(1)
	fake.Seed("xet/chunks/"+chunk.Hex(), []byte("../../../etc/passwd"))
	if _, err := store.DedupShard(context.Background(), chunk); err == nil ||
		errors.Is(err, ErrNotFound) {
		t.Errorf("corrupt pointer = %v, want hard error", err)
	}
}

// TestGlobalDedupFlow drives the whole protocol: a shard upload indexes
// its eligible chunks, and the chunk query returns the shard bytes
// verbatim — on both store persistence layers.
func TestGlobalDedupFlow(t *testing.T) {
	t.Parallel()
	for name, store := range storeVariants(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testGlobalDedupFlow(t, store)
		})
	}
}

func testGlobalDedupFlow(t *testing.T, store *Store) {
	mat := newMemMaterializer()
	svc, mux := newTestServiceOn(t, mat, store)
	repo, _ := hfapi.ParseRepoID("org/repo")
	writeTok, _ := svc.MintToken("write", hfapi.RepoKindModel, repo, "alice")
	readTok, _ := svc.MintToken("read", hfapi.RepoKindModel, repo, "alice")

	// One xorb of two chunks; the first chunk hash is dedup-eligible.
	chunkA, chunkB := []byte("aaaaAAAA"), []byte("bbbbBBBB")
	content := append(append([]byte{}, chunkA...), chunkB...)
	xorbHex := uploadTestXorb(t, mux, writeTok, chunkA, chunkB)
	xorbHash, _ := ParseHex(xorbHex)
	eligible := eligibleChunkHash(7)
	var notEligible Hash
	notEligible[0] = 0x22
	notEligible[24] = 1 // fourth limb = 1: not a multiple of the modulus

	fileHash, _ := ParseHex(strings.Repeat("cd", 32))
	sum := sha256.Sum256(content)
	sha, err := ParseHex(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	shardBytes := buildShard(t,
		[]ShardFile{{
			FileHash: fileHash,
			SHA256:   sha,
			Terms:    []Term{{XorbHash: xorbHash, UnpackedLen: int64(len(content)), ChunkStart: 0, ChunkEnd: 2}},
		}},
		[]ShardXorb{{
			XorbHash: xorbHash, NumChunks: 2, NumBytesInXorb: int64(len(content)),
			ChunkHashes: []Hash{eligible, notEligible},
		}},
	)
	if rec := casRequest(t, mux, http.MethodPost, "/xet/v1/shards", writeTok, shardBytes); rec.Code != http.StatusOK {
		t.Fatalf("shard upload: %d %s", rec.Code, rec.Body)
	}

	query := func(hash, token string) (int, []byte) {
		rec := casRequest(t, mux, http.MethodGet, "/xet/v1/chunks/default/"+hash, token, nil)
		return rec.Code, rec.Body.Bytes()
	}

	// The eligible chunk answers with the exact shard bytes.
	code, body := query(eligible.Hex(), readTok)
	if code != http.StatusOK || !bytes.Equal(body, shardBytes) {
		t.Fatalf("eligible query = %d, %d bytes; want 200 with the shard verbatim", code, len(body))
	}
	// The response parses as a shard referencing the xorb (what the
	// client will do with it).
	back, err := ParseShard(body)
	if err != nil || len(back.Xorbs) != 1 || back.Xorbs[0].XorbHash != xorbHash {
		t.Fatalf("response shard = %+v, %v", back, err)
	}

	// Ineligible and unknown chunks are 404 (the ineligible one proves
	// indexing is selective).
	if code, _ := query(notEligible.Hex(), readTok); code != http.StatusNotFound {
		t.Errorf("ineligible chunk query = %d, want 404", code)
	}
	if code, _ := query(eligibleChunkHash(99).Hex(), readTok); code != http.StatusNotFound {
		t.Errorf("unknown chunk query = %d, want 404", code)
	}

	// Guard rails.
	if code, _ := query(eligible.Hex(), ""); code != http.StatusUnauthorized {
		t.Errorf("no-token query = %d, want 401", code)
	}
	if rec := casRequest(t, mux, http.MethodGet, "/xet/v1/chunks/other/"+eligible.Hex(), readTok, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad prefix = %d, want 400", rec.Code)
	}
	if rec := casRequest(t, mux, http.MethodGet, "/xet/v1/chunks/default/nothex", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad hash = %d, want 400", rec.Code)
	}

	// Metrics: one hit, two misses, one new xorb.
	count := func(event string) float64 {
		return testutil.ToFloat64(svc.metrics.XetDedup.WithLabelValues(event))
	}
	if got := count("query_hit"); got != 1 {
		t.Errorf("query_hit = %v, want 1", got)
	}
	if got := count("query_miss"); got != 2 {
		t.Errorf("query_miss = %v, want 2", got)
	}
	if got := count("xorb_new"); got != 1 {
		t.Errorf("xorb_new = %v, want 1", got)
	}
	// A duplicate xorb upload counts as deduped bytes.
	rec := casRequest(t, mux, http.MethodPost, "/xet/v1/xorbs/default/"+xorbHex, writeTok, buildChunkPair(t, chunkA, chunkB))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-upload: %d", rec.Code)
	}
	if got := count("xorb_duplicate"); got != 1 {
		t.Errorf("xorb_duplicate = %v, want 1", got)
	}
}

// buildChunkPair serializes two chunks the way uploadTestXorb does.
func buildChunkPair(t *testing.T, a, b []byte) []byte {
	t.Helper()
	return append(buildChunk(t, a, compressionNone), buildChunk(t, b, compressionNone)...)
}

// newDeadBucketStore builds a bucket store whose server the caller can
// kill to simulate outages.
func newDeadBucketStore(t *testing.T) (*Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fakes3.New("b", "", ""))
	client, err := s3client.New(s3client.Options{Endpoint: srv.URL, Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	return NewBucketStore(client, "xet"), srv
}

// TestDedupUnavailableStoreDegradesToMiss: store trouble answers 404, not
// 500 — dedup is an optimization and must never fail an upload.
func TestDedupUnavailableStoreDegradesToMiss(t *testing.T) {
	t.Parallel()
	store, srv := newDeadBucketStore(t)
	svc, mux := newTestServiceOn(t, nil, store)
	repo, _ := hfapi.ParseRepoID("org/repo")
	readTok, _ := svc.MintToken("read", hfapi.RepoKindModel, repo, "alice")
	srv.Close() // the bucket is now unreachable

	rec := casRequest(t, mux, http.MethodGet, "/xet/v1/chunks/default/"+eligibleChunkHash(1).Hex(), readTok, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("query against dead store = %d, want 404", rec.Code)
	}
}

// TestServiceCountDedupNilMetrics: a nil metrics wiring must not panic.
func TestServiceCountDedupNilMetrics(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.countDedup("query_hit") // must not panic
}
