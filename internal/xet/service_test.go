package xet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/metrics"
)

// memMaterializer stores blobs in memory, verifying content against the
// sha256 oid the way real backends do.
type memMaterializer struct{ blobs map[string][]byte }

func newMemMaterializer() *memMaterializer {
	return &memMaterializer{blobs: map[string][]byte{}}
}

func (m *memMaterializer) HasLFSBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, oid string) bool {
	_, ok := m.blobs[oid]
	return ok
}

func (m *memMaterializer) PutLFSBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, oid string, size int64, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != oid {
		return backend.ErrDigestMismatch
	}
	m.blobs[oid] = data
	return nil
}

// newTestService wires a Service over a temp store and returns the CAS mux
// mounted the way server.go mounts it.
func newTestService(t *testing.T, mat Materializer) (*Service, *http.ServeMux) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newTestServiceOn(t, mat, store)
}

// newTestServiceOn is newTestService over a caller-provided store (the
// protocol tests run on both the disk and bucket persistence layers).
func newTestServiceOn(t *testing.T, mat Materializer, store *Store) (*Service, *http.ServeMux) {
	t.Helper()
	svc, err := NewService(store, mat, metrics.New(), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /xet/v1/xorbs/{prefix}/{hash}", svc.HandleXorbUpload)
	mux.HandleFunc("POST /xet/v1/shards", svc.HandleShardUpload)
	mux.HandleFunc("GET /xet/v1/reconstructions/{file_id}", svc.HandleReconstruction)
	mux.HandleFunc("GET /xet/v1/chunks/{prefix}/{hash}", svc.HandleChunkQuery)
	mux.HandleFunc("GET /xet/data/{hash}", svc.HandleXorbData)
	return svc, mux
}

func casRequest(t *testing.T, mux *http.ServeMux, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestNewServiceDefaultsLogger(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if svc.log == nil {
		t.Fatal("nil logger not defaulted")
	}
	custom := slog.New(slog.DiscardHandler)
	svc, err = NewService(store, nil, nil, custom, nil)
	if err != nil {
		t.Fatal(err)
	}
	if svc.log != custom {
		t.Fatal("custom logger replaced")
	}
	if svc.Store() != store {
		t.Fatal("Store() does not return the wired store")
	}
}

func TestTokenScopeAndTampering(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t, nil)
	repo, _ := hfapi.ParseRepoID("org/repo")

	readTok, exp := svc.MintToken("read", hfapi.RepoKindModel, repo, "alice")
	if exp <= time.Now().Unix() {
		t.Fatalf("expiry %d not in the future", exp)
	}

	verify := func(token, needScope string) (tokenClaims, bool) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return svc.verifyBearer(r, needScope)
	}

	claims, ok := verify(readTok, "read")
	if !ok || claims.Repo != "org/repo" || claims.User != "alice" || claims.Scope != "read" {
		t.Fatalf("read token rejected or claims wrong: %+v ok=%v", claims, ok)
	}
	if _, ok := verify(readTok, "write"); ok {
		t.Fatal("read token accepted for write")
	}
	writeTok, _ := svc.MintToken("write", hfapi.RepoKindModel, repo, "alice")
	if _, ok := verify(writeTok, "write"); !ok {
		t.Fatal("write token rejected for write")
	}
	if _, ok := verify(writeTok, "read"); !ok {
		t.Fatal("write token rejected for read (write implies read)")
	}

	// Tampering with the payload invalidates the signature.
	parts := strings.Split(readTok, ".")
	forged := tokenClaims{Scope: "write", Kind: hfapi.RepoKindModel, Repo: "org/repo", Exp: time.Now().Add(time.Hour).Unix()}
	payload, _ := json.Marshal(forged)
	parts[1] = base64.RawURLEncoding.EncodeToString(payload)
	if _, ok := verify(strings.Join(parts, "."), "write"); ok {
		t.Fatal("forged payload accepted")
	}

	// An expired token signed with the real secret is rejected.
	expired := tokenClaims{Scope: "read", Kind: hfapi.RepoKindModel, Repo: "org/repo", Exp: time.Now().Add(-time.Minute).Unix()}
	payload, _ = json.Marshal(expired)
	body := base64.RawURLEncoding.EncodeToString(payload)
	if _, ok := verify("sxet1."+body+"."+svc.sign(body), "read"); ok {
		t.Fatal("expired token accepted")
	}

	// Structurally broken tokens.
	for _, bad := range []string{"", "junk", "sxet1.only-two", "wrong." + parts[1] + "." + parts[2], "sxet1.!!!." + svc.sign("!!!")} {
		if _, ok := verify(bad, "read"); ok {
			t.Fatalf("token %q accepted", bad)
		}
	}
	// Missing Bearer prefix.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", readTok)
	if _, ok := svc.verifyBearer(r, "read"); ok {
		t.Fatal("token without Bearer prefix accepted")
	}
}

func TestWriteTokenResponse(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t, nil)
	repo, _ := hfapi.ParseRepoID("org/repo")

	r := httptest.NewRequest(http.MethodGet, "http://shpiel.example/api/models/org/repo/xet-read-token/main", nil)
	rec := httptest.NewRecorder()
	svc.WriteTokenResponse(rec, r, "read", hfapi.RepoKindModel, repo, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(HeaderCasURL); got != "http://shpiel.example/xet" {
		t.Fatalf("cas url = %q", got)
	}
	token := rec.Header().Get(HeaderAccessToken)
	if token == "" || rec.Header().Get(HeaderTokenExpiration) == "" {
		t.Fatal("token headers missing")
	}
	var body struct {
		CasURL      string `json:"casUrl"`
		AccessToken string `json:"accessToken"`
		Exp         int64  `json:"exp"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.CasURL != "http://shpiel.example/xet" || body.AccessToken != token || body.Exp == 0 {
		t.Fatalf("body = %+v", body)
	}

	// Behind TLS-terminating proxies the advertised URL follows
	// X-Forwarded-Proto.
	r = httptest.NewRequest(http.MethodGet, "http://shpiel.example/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	rec = httptest.NewRecorder()
	svc.WriteTokenResponse(rec, r, "read", hfapi.RepoKindModel, repo, "")
	if got := rec.Header().Get(HeaderCasURL); got != "https://shpiel.example/xet" {
		t.Fatalf("forwarded-proto cas url = %q", got)
	}
}

func TestHandleXorbUpload(t *testing.T) {
	t.Parallel()
	svc, mux := newTestService(t, nil)
	repo, _ := hfapi.ParseRepoID("org/repo")
	writeTok, _ := svc.MintToken("write", hfapi.RepoKindModel, repo, "alice")
	readTok, _ := svc.MintToken("read", hfapi.RepoKindModel, repo, "alice")

	xorb := buildChunk(t, []byte("xorb content"), compressionNone)
	hash := strings.Repeat("ab", 32)
	path := "/xet/v1/xorbs/default/" + hash

	if rec := casRequest(t, mux, http.MethodPost, path, "", xorb); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}
	if rec := casRequest(t, mux, http.MethodPost, path, readTok, xorb); rec.Code != http.StatusUnauthorized {
		t.Fatalf("read token: status = %d, want 401", rec.Code)
	}
	if rec := casRequest(t, mux, http.MethodPost, "/xet/v1/xorbs/other/"+hash, writeTok, xorb); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad prefix: status = %d, want 400", rec.Code)
	}
	if rec := casRequest(t, mux, http.MethodPost, "/xet/v1/xorbs/default/nothex", writeTok, xorb); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad hash: status = %d, want 400", rec.Code)
	}
	if rec := casRequest(t, mux, http.MethodPost, path, writeTok, []byte("not a xorb")); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage xorb: status = %d, want 400", rec.Code)
	}

	rec := casRequest(t, mux, http.MethodPost, path, writeTok, xorb)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: status = %d, body %s", rec.Code, rec.Body)
	}
	var resp map[string]bool
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp["was_inserted"] {
		t.Fatal("first upload reported was_inserted=false")
	}
	// Idempotent re-upload.
	rec = casRequest(t, mux, http.MethodPost, path, writeTok, xorb)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-upload: status = %d", rec.Code)
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["was_inserted"] {
		t.Fatal("re-upload reported was_inserted=true")
	}
}

// uploadTestXorb pushes one xorb of the given chunks through the HTTP
// handler and returns its hash hex.
func uploadTestXorb(t *testing.T, mux *http.ServeMux, token string, chunks ...[]byte) string {
	t.Helper()
	var xorb []byte
	for _, c := range chunks {
		xorb = append(xorb, buildChunk(t, c, compressionNone)...)
	}
	hash := strings.Repeat("ab", 32)
	rec := casRequest(t, mux, http.MethodPost, "/xet/v1/xorbs/default/"+hash, token, xorb)
	if rec.Code != http.StatusOK {
		t.Fatalf("xorb upload: status = %d, body %s", rec.Code, rec.Body)
	}
	return hash
}

func TestHandleShardUploadMaterializes(t *testing.T) {
	t.Parallel()
	mat := newMemMaterializer()
	svc, mux := newTestService(t, mat)
	repo, _ := hfapi.ParseRepoID("org/repo")
	writeTok, _ := svc.MintToken("write", hfapi.RepoKindModel, repo, "alice")

	content := []byte("the file content")
	xorbHex := uploadTestXorb(t, mux, writeTok, content)
	xorbHash, _ := ParseHex(xorbHex)
	fileHash, _ := ParseHex(strings.Repeat("cd", 32))
	sum := sha256.Sum256(content)
	var sha Hash
	// The shard carries the sha256 as a Hash; PutFile stores its canonical
	// hex, which for metadata extensions matches standard sha256 hex.
	shaHash, err := ParseHex(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	sha = shaHash

	shard := buildShard(t,
		[]ShardFile{{
			FileHash: fileHash,
			SHA256:   sha,
			Terms:    []Term{{XorbHash: xorbHash, UnpackedLen: int64(len(content)), ChunkStart: 0, ChunkEnd: 1}},
		}},
		[]ShardXorb{{XorbHash: xorbHash, NumChunks: 1, NumBytesInXorb: int64(len(content))}},
	)

	if rec := casRequest(t, mux, http.MethodPost, "/xet/v1/shards", "", shard); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}
	if rec := casRequest(t, mux, http.MethodPost, "/xet/v1/shards", writeTok, []byte("garbage")); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage shard: status = %d, want 400", rec.Code)
	}

	rec := casRequest(t, mux, http.MethodPost, "/xet/v1/shards", writeTok, shard)
	if rec.Code != http.StatusOK {
		t.Fatalf("shard upload: status = %d, body %s", rec.Code, rec.Body)
	}
	shaHex := hex.EncodeToString(sum[:])
	if got, ok := mat.blobs[shaHex]; !ok || !bytes.Equal(got, content) {
		t.Fatalf("materialized blob missing or wrong (%d bytes)", len(got))
	}
	// The file record and sha256 mapping landed in the store.
	if back, ok := svc.Store().FileHashBySHA256(context.Background(), shaHex); !ok || back != fileHash.Hex() {
		t.Fatalf("sha256 mapping = %q, %v", back, ok)
	}
}

func TestHandleShardUploadFailures(t *testing.T) {
	t.Parallel()
	mat := newMemMaterializer()
	svc, mux := newTestService(t, mat)
	repo, _ := hfapi.ParseRepoID("org/repo")
	writeTok, _ := svc.MintToken("write", hfapi.RepoKindModel, repo, "alice")

	content := []byte("the file content")
	sum := sha256.Sum256(content)
	sha, err := ParseHex(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	fileHash, _ := ParseHex(strings.Repeat("cd", 32))
	missingXorb, _ := ParseHex(strings.Repeat("ee", 32))

	// A shard whose term references a xorb the client never uploaded is
	// the client's fault: 400.
	shard := buildShard(t,
		[]ShardFile{{
			FileHash: fileHash,
			SHA256:   sha,
			Terms:    []Term{{XorbHash: missingXorb, UnpackedLen: int64(len(content)), ChunkStart: 0, ChunkEnd: 1}},
		}},
		[]ShardXorb{{XorbHash: missingXorb, NumChunks: 1}},
	)
	rec := casRequest(t, mux, http.MethodPost, "/xet/v1/shards", writeTok, shard)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing xorb: status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}

	// A term whose chunk range exceeds the xorb's real layout fails
	// reconstruction; that surfaces as materialization failure, not
	// success.
	xorbHex := uploadTestXorb(t, mux, writeTok, content)
	xorbHash, _ := ParseHex(xorbHex)
	shard = buildShard(t,
		[]ShardFile{{
			FileHash: fileHash,
			SHA256:   sha,
			Terms:    []Term{{XorbHash: xorbHash, UnpackedLen: int64(len(content)), ChunkStart: 0, ChunkEnd: 5}},
		}},
		[]ShardXorb{{XorbHash: xorbHash, NumChunks: 5}},
	)
	rec = casRequest(t, mux, http.MethodPost, "/xet/v1/shards", writeTok, shard)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("bad chunk range: status = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
}

// TestReconstructionAndDataURL runs the full CAS protocol flow — xorb
// upload, shard ingest with materialization, reconstruction (whole and
// ranged), and signed data URLs — identically on both store persistence
// layers.
func TestReconstructionAndDataURL(t *testing.T) {
	t.Parallel()
	for name, store := range storeVariants(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testReconstructionAndDataURL(t, store)
		})
	}
}

func testReconstructionAndDataURL(t *testing.T, store *Store) {
	mat := newMemMaterializer()
	svc, mux := newTestServiceOn(t, mat, store)
	repo, _ := hfapi.ParseRepoID("org/repo")
	writeTok, _ := svc.MintToken("write", hfapi.RepoKindModel, repo, "alice")
	readTok, _ := svc.MintToken("read", hfapi.RepoKindModel, repo, "alice")

	// Two chunks of 8 bytes each, then a shard mapping a file to them.
	chunkA, chunkB := []byte("aaaaAAAA"), []byte("bbbbBBBB")
	content := append(append([]byte{}, chunkA...), chunkB...)
	xorbHex := uploadTestXorb(t, mux, writeTok, chunkA, chunkB)
	xorbHash, _ := ParseHex(xorbHex)
	fileHash, _ := ParseHex(strings.Repeat("cd", 32))
	sum := sha256.Sum256(content)
	sha, err := ParseHex(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	shard := buildShard(t,
		[]ShardFile{{
			FileHash: fileHash,
			SHA256:   sha,
			Terms:    []Term{{XorbHash: xorbHash, UnpackedLen: int64(len(content)), ChunkStart: 0, ChunkEnd: 2}},
		}},
		[]ShardXorb{{XorbHash: xorbHash, NumChunks: 2, NumBytesInXorb: int64(len(content))}},
	)
	if rec := casRequest(t, mux, http.MethodPost, "/xet/v1/shards", writeTok, shard); rec.Code != http.StatusOK {
		t.Fatalf("shard upload: %d %s", rec.Code, rec.Body)
	}

	recPath := "/xet/v1/reconstructions/" + fileHash.Hex()
	if rec := casRequest(t, mux, http.MethodGet, recPath, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d", rec.Code)
	}
	if rec := casRequest(t, mux, http.MethodGet, "/xet/v1/reconstructions/nothex", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad file id: status = %d", rec.Code)
	}
	if rec := casRequest(t, mux, http.MethodGet, "/xet/v1/reconstructions/"+strings.Repeat("99", 32), readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown file: status = %d", rec.Code)
	}

	rec := casRequest(t, mux, http.MethodGet, recPath, readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconstruction: status = %d, body %s", rec.Code, rec.Body)
	}
	var resp reconstructionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.OffsetIntoFirstRange != 0 || len(resp.Terms) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	term := resp.Terms[0]
	if term.Hash != xorbHex || term.Range.Start != 0 || term.Range.End != 2 {
		t.Fatalf("term = %+v", term)
	}
	fetches := resp.FetchInfo[xorbHex]
	if len(fetches) != 1 || fetches[0].URLRange.Start != 0 {
		t.Fatalf("fetch_info = %+v", fetches)
	}

	// The fetch URL serves the xorb bytes without auth headers.
	u := fetches[0].URL
	req := httptest.NewRequest(http.MethodGet, u, nil)
	dataRec := httptest.NewRecorder()
	mux.ServeHTTP(dataRec, req)
	if dataRec.Code != http.StatusOK {
		t.Fatalf("data url: status = %d", dataRec.Code)
	}
	if int64(dataRec.Body.Len()-1) < fetches[0].URLRange.End {
		t.Fatalf("data body %d bytes, want at least %d", dataRec.Body.Len(), fetches[0].URLRange.End+1)
	}

	// Tampered or expired signatures are rejected.
	req = httptest.NewRequest(http.MethodGet, strings.Replace(u, "s=", "s=00", 1), nil)
	dataRec = httptest.NewRecorder()
	mux.ServeHTTP(dataRec, req)
	if dataRec.Code != http.StatusForbidden {
		t.Fatalf("tampered data url: status = %d", dataRec.Code)
	}
	pastExp := time.Now().Add(-time.Minute).Unix()
	expired := fmt.Sprintf("/xet/data/%s?e=%d&s=%s", xorbHex, pastExp, svc.sign(fmt.Sprintf("data.%s.%d", xorbHex, pastExp)))
	req = httptest.NewRequest(http.MethodGet, expired, nil)
	dataRec = httptest.NewRecorder()
	mux.ServeHTTP(dataRec, req)
	if dataRec.Code != http.StatusForbidden {
		t.Fatalf("expired data url: status = %d", dataRec.Code)
	}
	// A well-signed URL for a xorb that is not there is 404.
	ghost := strings.Repeat("77", 32)
	exp := time.Now().Add(time.Minute).Unix()
	missing := fmt.Sprintf("/xet/data/%s?e=%d&s=%s", ghost, exp, svc.sign(fmt.Sprintf("data.%s.%d", ghost, exp)))
	req = httptest.NewRequest(http.MethodGet, missing, nil)
	dataRec = httptest.NewRecorder()
	mux.ServeHTTP(dataRec, req)
	if dataRec.Code != http.StatusNotFound {
		t.Fatalf("missing xorb data url: status = %d", dataRec.Code)
	}

	// Range handling: the second half of the file starts in the same
	// (only) term, so the offset into it is the in-term position.
	req = httptest.NewRequest(http.MethodGet, recPath, nil)
	req.Header.Set("Authorization", "Bearer "+readTok)
	req.Header.Set("Range", "bytes=10-15")
	rangeRec := httptest.NewRecorder()
	mux.ServeHTTP(rangeRec, req)
	if rangeRec.Code != http.StatusOK {
		t.Fatalf("ranged reconstruction: status = %d", rangeRec.Code)
	}
	resp = reconstructionResponse{}
	if err := json.NewDecoder(rangeRec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.OffsetIntoFirstRange != 10 || len(resp.Terms) != 1 {
		t.Fatalf("ranged resp = %+v", resp)
	}

	// Ranges beyond the file and malformed ranges are 416.
	for _, rng := range []string{"bytes=16-20", "bytes=abc-def", "bytes=5-2", "chunks=0-1", "bytes=-5"} {
		req = httptest.NewRequest(http.MethodGet, recPath, nil)
		req.Header.Set("Authorization", "Bearer "+readTok)
		req.Header.Set("Range", rng)
		rangeRec = httptest.NewRecorder()
		mux.ServeHTTP(rangeRec, req)
		if rangeRec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("Range %q: status = %d, want 416", rng, rangeRec.Code)
		}
	}
}

// TestReconstructionTermBoundaries: ranges landing exactly on term
// boundaries select exactly the right terms with the right in-term offset.
func TestReconstructionTermBoundaries(t *testing.T) {
	t.Parallel()
	mat := newMemMaterializer()
	svc, mux := newTestService(t, mat)
	repo, _ := hfapi.ParseRepoID("org/terms")
	writeTok, _ := svc.MintToken("write", hfapi.RepoKindModel, repo, "")
	readTok, _ := svc.MintToken("read", hfapi.RepoKindModel, repo, "")

	// One xorb of two 8-byte chunks; the file uses them as two separate
	// terms so term boundaries land at byte 8.
	chunkA, chunkB := []byte("aaaaAAAA"), []byte("bbbbBBBB")
	content := append(append([]byte{}, chunkA...), chunkB...)
	xorbHex := uploadTestXorb(t, mux, writeTok, chunkA, chunkB)
	xorbHash, _ := ParseHex(xorbHex)
	fileHash, _ := ParseHex(strings.Repeat("dd", 32))
	sum := sha256.Sum256(content)
	sha, err := ParseHex(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	shard := buildShard(t,
		[]ShardFile{{
			FileHash: fileHash,
			SHA256:   sha,
			Terms: []Term{
				{XorbHash: xorbHash, UnpackedLen: 8, ChunkStart: 0, ChunkEnd: 1},
				{XorbHash: xorbHash, UnpackedLen: 8, ChunkStart: 1, ChunkEnd: 2},
			},
		}},
		[]ShardXorb{{XorbHash: xorbHash, NumChunks: 2, NumBytesInXorb: 16}},
	)
	if rec := casRequest(t, mux, http.MethodPost, "/xet/v1/shards", writeTok, shard); rec.Code != http.StatusOK {
		t.Fatalf("shard upload: %d %s", rec.Code, rec.Body)
	}

	query := func(rng string) reconstructionResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/xet/v1/reconstructions/"+fileHash.Hex(), nil)
		req.Header.Set("Authorization", "Bearer "+readTok)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Range %q: status %d %s", rng, rec.Code, rec.Body)
		}
		var resp reconstructionResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// A range starting exactly at the second term skips the first
	// entirely: no offset into it.
	resp := query("bytes=8-15")
	if len(resp.Terms) != 1 || resp.Terms[0].Range.Start != 1 || resp.OffsetIntoFirstRange != 0 {
		t.Fatalf("second-term range = %+v", resp)
	}
	// A range starting inside the second term reports the in-term offset.
	resp = query("bytes=10-15")
	if len(resp.Terms) != 1 || resp.OffsetIntoFirstRange != 2 {
		t.Fatalf("mid-second-term range = %+v", resp)
	}
	// A range ending exactly at the first byte of the second term still
	// needs both terms.
	resp = query("bytes=0-8")
	if len(resp.Terms) != 2 || resp.OffsetIntoFirstRange != 0 {
		t.Fatalf("cross-boundary range = %+v", resp)
	}
	// A range within the first term stops there.
	resp = query("bytes=0-7")
	if len(resp.Terms) != 1 || resp.Terms[0].Range.End != 1 {
		t.Fatalf("first-term range = %+v", resp)
	}
	// No range: everything.
	resp = query("")
	if len(resp.Terms) != 2 {
		t.Fatalf("full read = %+v", resp)
	}
}

// TestRejectedShardDump: with SHPIEL_XET_DEBUG_DIR set, unparseable shards
// are dumped for postmortem.
func TestRejectedShardDump(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHPIEL_XET_DEBUG_DIR", dir) // Setenv forbids t.Parallel
	svc, mux := newTestService(t, nil)
	repo, _ := hfapi.ParseRepoID("org/dump")
	writeTok, _ := svc.MintToken("write", hfapi.RepoKindModel, repo, "")

	if rec := casRequest(t, mux, http.MethodPost, "/xet/v1/shards", writeTok, []byte("not a shard")); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage shard = %d", rec.Code)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "rejected-shard-") {
		t.Fatalf("debug dir entries = %v", entries)
	}
}

func TestParseByteRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		header     string
		total      int64
		start, end int64
		ok         bool
	}{
		{"bytes=0-9", 100, 0, 9, true},
		{"bytes=10-", 100, 10, 99, true},
		{"bytes=0-0", 100, 0, 0, true},
		{"bytes=99-99", 100, 99, 99, true},
		{"bytes=50-200", 100, 50, 99, true}, // end clamped to length
		{"bytes=100-110", 100, 0, 0, false}, // start beyond end of file
		{"bytes=5-4", 100, 0, 0, false},
		{"bytes=-5", 100, 0, 0, false}, // suffix ranges unsupported per CAS spec
		{"bytes=a-b", 100, 0, 0, false},
		{"octets=0-5", 100, 0, 0, false},
		{"bytes=05", 100, 0, 0, false},
	}
	for _, tc := range cases {
		start, end, err := parseByteRange(tc.header, tc.total)
		if tc.ok != (err == nil) {
			t.Errorf("%q: err = %v, want ok=%v", tc.header, err, tc.ok)
			continue
		}
		if tc.ok && (start != tc.start || end != tc.end) {
			t.Errorf("%q: range = [%d, %d], want [%d, %d]", tc.header, start, end, tc.start, tc.end)
		}
	}
}

func TestHandleChunkQuery(t *testing.T) {
	t.Parallel()
	svc, mux := newTestService(t, nil)
	repo, _ := hfapi.ParseRepoID("org/repo")
	readTok, _ := svc.MintToken("read", hfapi.RepoKindModel, repo, "")

	path := "/xet/v1/chunks/default/" + strings.Repeat("ab", 32)
	if rec := casRequest(t, mux, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d", rec.Code)
	}
	// 404 is the spec's "no dedup info" answer.
	if rec := casRequest(t, mux, http.MethodGet, path, readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
