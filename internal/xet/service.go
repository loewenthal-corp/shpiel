package xet

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// Protocol size caps: xorbs max out at 64 MiB by construction; shards are
// metadata and stay far smaller.
const (
	maxXorbSize  = 96 << 20
	maxShardSize = 64 << 20
	tokenTTL     = time.Hour
	dataURLTTL   = 15 * time.Minute
)

// Token response headers the huggingface_hub client reads
// (parse_xet_connection_info_from_headers). These are header NAMES from
// the protocol, not credentials.
const (
	HeaderCasURL          = "X-Xet-Cas-Url"
	HeaderAccessToken     = "X-Xet-Access-Token"     //nolint:gosec // header name, not a credential
	HeaderTokenExpiration = "X-Xet-Token-Expiration" //nolint:gosec // header name, not a credential
	// Resolve-response headers advertising xet availability for a file.
	HeaderHash         = "X-Xet-Hash"
	HeaderRefreshRoute = "X-Xet-Refresh-Route"
)

// Materializer is what the service needs from the relay: a way to check
// for and store reconstructed file content in the routed backend, keyed by
// sha256 — after which the normal read path (and every backend) serves the
// file with no xet involvement.
type Materializer interface {
	HasLFSBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, oid string) bool
	PutLFSBlob(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, oid string, size int64, body io.Reader) error
}

// Service implements the Xet CAS HTTP API: token issuance, xorb and shard
// ingest (with materialization), and the reconstruction API for chunk-level
// downloads.
type Service struct {
	store  *Store
	mat    Materializer
	log    *slog.Logger
	secret []byte
}

// NewService creates a Service. The signing secret is per-process: tokens
// and data URLs die on restart, which is fine — clients refresh through
// the token endpoints.
func NewService(store *Store, mat Materializer, log *slog.Logger) (*Service, error) {
	if log == nil {
		log = slog.Default()
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("xet: generating signing secret: %w", err)
	}
	return &Service{store: store, mat: mat, log: log, secret: secret}, nil
}

// Store exposes the underlying store (resolve headers use it).
func (s *Service) Store() *Store { return s.store }

// --- tokens ---

type tokenClaims struct {
	Scope string         `json:"s"` // "read" | "write"
	Kind  hfapi.RepoKind `json:"k"`
	Repo  string         `json:"r"`
	Exp   int64          `json:"e"`
}

// MintToken issues a CAS access token scoped to a repo; scope is "read" or
// "write" (write implies read).
func (s *Service) MintToken(scope string, kind hfapi.RepoKind, repo hfapi.RepoID) (token string, expiry int64) {
	claims := tokenClaims{Scope: scope, Kind: kind, Repo: repo.String(), Exp: time.Now().Add(tokenTTL).Unix()}
	payload, _ := json.Marshal(claims)
	body := base64.RawURLEncoding.EncodeToString(payload)
	return "sxet1." + body + "." + s.sign(body), claims.Exp
}

func (s *Service) verifyBearer(r *http.Request, needScope string) (tokenClaims, bool) {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return tokenClaims{}, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "sxet1" {
		return tokenClaims{}, false
	}
	if !hmac.Equal([]byte(s.sign(parts[1])), []byte(parts[2])) {
		return tokenClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokenClaims{}, false
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return tokenClaims{}, false
	}
	if time.Now().Unix() > claims.Exp {
		return tokenClaims{}, false
	}
	if needScope == "write" && claims.Scope != "write" {
		return tokenClaims{}, false
	}
	return claims, true
}

func (s *Service) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// WriteTokenResponse answers a xet-{read,write}-token request: connection
// info rides in response headers (that is where huggingface_hub looks),
// with a JSON body mirroring it for debuggability.
func (s *Service) WriteTokenResponse(w http.ResponseWriter, r *http.Request, scope string, kind hfapi.RepoKind, repo hfapi.RepoID) {
	token, exp := s.MintToken(scope, kind, repo)
	casURL := requestBaseURL(r) + "/xet"
	h := w.Header()
	h.Set(HeaderCasURL, casURL)
	h.Set(HeaderAccessToken, token)
	h.Set(HeaderTokenExpiration, strconv.FormatInt(exp, 10))
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"casUrl": casURL, "accessToken": token, "exp": exp,
	})
}

// --- CAS HTTP handlers (mounted under /xet/) ---

func writeCASError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// HandleXorbUpload serves POST /xet/v1/xorbs/{prefix}/{hash}.
func (s *Service) HandleXorbUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifyBearer(r, "write"); !ok {
		writeCASError(w, http.StatusUnauthorized, "invalid or expired CAS token")
		return
	}
	if r.PathValue("prefix") != "default" {
		writeCASError(w, http.StatusBadRequest, "unknown xorb prefix")
		return
	}
	hash, err := ParseHex(r.PathValue("hash"))
	if err != nil {
		writeCASError(w, http.StatusBadRequest, "malformed xorb hash")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxXorbSize))
	if err != nil {
		writeCASError(w, http.StatusBadRequest, "reading xorb body: "+err.Error())
		return
	}
	inserted, err := s.store.PutXorb(hash, body)
	if err != nil {
		writeCASError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.InfoContext(r.Context(), "xet xorb ingested",
		"hash", hash.Hex(), "bytes", len(body), "inserted", inserted)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"was_inserted": inserted})
}

// HandleShardUpload serves POST /xet/v1/shards: parse the shard, record
// file reconstructions, and materialize each file into the routed backend
// so the entire non-xet world can serve it.
func (s *Service) HandleShardUpload(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.verifyBearer(r, "write")
	if !ok {
		writeCASError(w, http.StatusUnauthorized, "invalid or expired CAS token")
		return
	}
	repo, err := hfapi.ParseRepoID(claims.Repo)
	if err != nil {
		writeCASError(w, http.StatusBadRequest, "token repo invalid")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxShardSize))
	if err != nil {
		writeCASError(w, http.StatusBadRequest, "reading shard body: "+err.Error())
		return
	}
	shard, err := ParseShard(body)
	if err != nil {
		if dir := os.Getenv("SHPIEL_XET_DEBUG_DIR"); dir != "" {
			// Operator-provided debug dir; the file name is fully
			// server-generated.
			dump := filepath.Join(filepath.Clean(dir), fmt.Sprintf("rejected-shard-%d.bin", time.Now().UnixNano()))
			_ = os.WriteFile(dump, body, 0o644)
			s.log.WarnContext(r.Context(), "rejected shard dumped", "path", dump)
		}
		s.log.WarnContext(r.Context(), "xet shard rejected", "error", err, "bytes", len(body))
		writeCASError(w, http.StatusBadRequest, err.Error())
		return
	}

	for i := range shard.Files {
		file := &shard.Files[i]
		rec := recordFromShardFile(file)
		if err := s.store.PutFile(rec); err != nil {
			writeCASError(w, http.StatusInternalServerError, "storing file record: "+err.Error())
			return
		}
		if err := s.materialize(r.Context(), claims.Kind, repo, rec); err != nil {
			s.log.ErrorContext(r.Context(), "xet materialization failed",
				"file", rec.FileHash, "sha256", rec.SHA256, "error", err)
			writeCASError(w, http.StatusBadRequest, fmt.Sprintf("materializing %s: %v", rec.FileHash, err))
			return
		}
		s.log.InfoContext(r.Context(), "xet file registered",
			"file", rec.FileHash, "sha256", rec.SHA256, "bytes", rec.TotalLen, "terms", len(rec.Terms))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"result": 1})
}

func recordFromShardFile(f *ShardFile) *FileRecord {
	rec := &FileRecord{FileHash: f.FileHash.Hex()}
	if !f.SHA256.IsZero() {
		// The sha256 rides in a DataHash, whose canonical hex form is the
		// standard sha256 hex (the client parsed it from hex the same way).
		rec.SHA256 = f.SHA256.Hex()
	}
	for _, t := range f.Terms {
		rec.TotalLen += t.UnpackedLen
		rec.Terms = append(rec.Terms, TermRecord{
			Xorb: t.XorbHash.Hex(), ChunkStart: t.ChunkStart, ChunkEnd: t.ChunkEnd, UnpackedLen: t.UnpackedLen,
		})
	}
	return rec
}

// materialize reconstructs a file from its terms and streams it into the
// routed backend keyed by sha256. The backend verifies the digest while
// writing, so a reconstruction bug cannot corrupt the store.
func (s *Service) materialize(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, rec *FileRecord) error {
	if rec.SHA256 == "" {
		// No sha256 mapping: the file is xet-readable but cannot join the
		// regular blob world. Hub clients always attach one.
		return nil
	}
	if s.mat == nil || s.mat.HasLFSBlob(ctx, kind, repo, rec.SHA256) {
		return nil
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(s.reconstruct(rec, pw))
	}()
	if err := s.mat.PutLFSBlob(ctx, kind, repo, rec.SHA256, rec.TotalLen, pr); err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	return nil
}

// reconstruct streams the file's bytes term by term.
func (s *Service) reconstruct(rec *FileRecord, w io.Writer) error {
	for _, term := range rec.Terms {
		xorbHash, err := ParseHex(term.Xorb)
		if err != nil {
			return err
		}
		xorb, err := s.store.ReadXorb(xorbHash)
		if err != nil {
			return fmt.Errorf("term references xorb %s: %w", term.Xorb, err)
		}
		chunks, err := s.store.XorbChunks(xorbHash)
		if err != nil {
			return err
		}
		if err := DecodeChunkRange(xorb, chunks, term.ChunkStart, term.ChunkEnd, w); err != nil {
			return err
		}
	}
	return nil
}

// --- reconstruction API ---

// reconstructionResponse mirrors QueryReconstructionResponse from the CAS
// OpenAPI spec.
type reconstructionResponse struct {
	OffsetIntoFirstRange int64                  `json:"offset_into_first_range"`
	Terms                []reconstructionTerm   `json:"terms"`
	FetchInfo            map[string][]fetchInfo `json:"fetch_info"`
}

type reconstructionTerm struct {
	Hash           string     `json:"hash"`
	UnpackedLength int64      `json:"unpacked_length"`
	Range          indexRange `json:"range"`
}

type indexRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type byteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // inclusive
}

type fetchInfo struct {
	Range    indexRange `json:"range"`
	URL      string     `json:"url"`
	URLRange byteRange  `json:"url_range"`
}

// HandleReconstruction serves GET /xet/v1/reconstructions/{file_id},
// honoring the optional end-inclusive Range header.
func (s *Service) HandleReconstruction(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifyBearer(r, "read"); !ok {
		writeCASError(w, http.StatusUnauthorized, "invalid or expired CAS token")
		return
	}
	fileHash, err := ParseHex(r.PathValue("file_id"))
	if err != nil {
		writeCASError(w, http.StatusBadRequest, "malformed file id")
		return
	}
	rec, err := s.store.File(fileHash)
	if err != nil {
		writeCASError(w, http.StatusNotFound, "file not found")
		return
	}

	start, end := int64(0), rec.TotalLen-1
	if rng := r.Header.Get("Range"); rng != "" {
		start, end, err = parseByteRange(rng, rec.TotalLen)
		if err != nil {
			writeCASError(w, http.StatusRequestedRangeNotSatisfiable, err.Error())
			return
		}
	}

	resp := reconstructionResponse{FetchInfo: map[string][]fetchInfo{}}
	base := requestBaseURL(r)
	var acc int64
	for _, term := range rec.Terms {
		termEnd := acc + term.UnpackedLen // exclusive
		if termEnd <= start {
			acc = termEnd
			continue
		}
		if acc > end {
			break
		}
		if len(resp.Terms) == 0 {
			resp.OffsetIntoFirstRange = start - acc
		}
		xorbHash, err := ParseHex(term.Xorb)
		if err != nil {
			writeCASError(w, http.StatusInternalServerError, "corrupt term")
			return
		}
		chunks, err := s.store.XorbChunks(xorbHash)
		if err != nil {
			writeCASError(w, http.StatusInternalServerError, "xorb layout missing")
			return
		}
		if term.ChunkEnd > len(chunks) {
			writeCASError(w, http.StatusInternalServerError, "term chunk range exceeds xorb")
			return
		}
		chunkRange := indexRange{Start: term.ChunkStart, End: term.ChunkEnd}
		resp.Terms = append(resp.Terms, reconstructionTerm{
			Hash:           term.Xorb,
			UnpackedLength: term.UnpackedLen,
			Range:          chunkRange,
		})
		resp.FetchInfo[term.Xorb] = append(resp.FetchInfo[term.Xorb], fetchInfo{
			Range: chunkRange,
			URL:   s.signedDataURL(base, term.Xorb),
			URLRange: byteRange{
				Start: chunks[term.ChunkStart].Start,
				End:   chunks[term.ChunkEnd-1].End - 1, // inclusive
			},
		})
		acc = termEnd
	}
	if len(resp.Terms) == 0 {
		writeCASError(w, http.StatusRequestedRangeNotSatisfiable, "range beyond end of file")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// parseByteRange parses "bytes=a-b" (end-inclusive, both bounds required
// per the CAS spec).
func parseByteRange(header string, total int64) (int64, int64, error) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return 0, 0, fmt.Errorf("unsupported range %q", header)
	}
	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, fmt.Errorf("unsupported range %q", header)
	}
	start, err1 := strconv.ParseInt(startStr, 10, 64)
	end := total - 1
	var err2 error
	if endStr != "" {
		end, err2 = strconv.ParseInt(endStr, 10, 64)
	}
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return 0, 0, fmt.Errorf("invalid range %q", header)
	}
	if start >= total {
		return 0, 0, fmt.Errorf("range start %d beyond file length %d", start, total)
	}
	return start, min(end, total-1), nil
}

// HandleChunkQuery serves GET /xet/v1/chunks/{prefix}/{hash}: global
// deduplication is not implemented, and 404 is the spec-defined "no dedup
// info" answer clients proceed happily from.
func (s *Service) HandleChunkQuery(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifyBearer(r, "read"); !ok {
		writeCASError(w, http.StatusUnauthorized, "invalid or expired CAS token")
		return
	}
	writeCASError(w, http.StatusNotFound, "chunk not tracked by global deduplication")
}

// --- xorb data serving (fetch_info target) ---

// signedDataURL mints the fetch URL for a xorb: clients call it without
// auth headers (presigned-URL pattern), so a short-lived HMAC rides in the
// query string.
func (s *Service) signedDataURL(base, xorbHex string) string {
	exp := time.Now().Add(dataURLTTL).Unix()
	sig := s.sign(fmt.Sprintf("data.%s.%d", xorbHex, exp))
	return fmt.Sprintf("%s/xet/data/%s?e=%d&s=%s", base, xorbHex, exp, sig)
}

// HandleXorbData serves GET /xet/data/{hash}: raw xorb bytes with Range
// support (http.ServeContent), exactly what fetch_info URLs promise.
func (s *Service) HandleXorbData(w http.ResponseWriter, r *http.Request) {
	xorbHex := r.PathValue("hash")
	exp, err := strconv.ParseInt(r.URL.Query().Get("e"), 10, 64)
	sig := r.URL.Query().Get("s")
	if err != nil || time.Now().Unix() > exp ||
		!hmac.Equal([]byte(s.sign(fmt.Sprintf("data.%s.%d", xorbHex, exp))), []byte(sig)) {
		writeCASError(w, http.StatusForbidden, "invalid or expired data url")
		return
	}
	hash, err := ParseHex(xorbHex)
	if err != nil {
		writeCASError(w, http.StatusBadRequest, "malformed xorb hash")
		return
	}
	f, err := s.store.OpenXorb(hash)
	if err != nil {
		writeCASError(w, http.StatusNotFound, "xorb not found")
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, f)
}

// requestBaseURL reconstructs the externally-visible base URL.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	return scheme + "://" + r.Host
}
