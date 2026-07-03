// Package fakeregistry is an in-process OCI registry for tests that is
// strict the way Zot is strict about blob upload sessions.
//
// The lenient test registry from go-containerregistry accepts a closing
// PUT that carries data without a Content-Range no matter what the
// session holds — which is exactly how a chunked-upload bug survived unit
// tests and then failed against real Zot with 416 BLOB_UPLOAD_INVALID.
// This registry reproduces Zot's session semantics (mirroring
// UpdateBlobUpload/PatchBlobUpload in zot's pkg/api/routes.go):
//
//   - PATCH with Content-Length and Content-Range appends one chunk; the
//     range must be "<start>-<end>" with start equal to the bytes already
//     in the session and (end-start+1) equal to Content-Length, else 416.
//   - PATCH without Content-Range streams: bytes append at the session end.
//   - PUT with a body and no Content-Range is a monolithic upload from
//     offset 0: it is rejected with 416 once the session holds bytes.
//   - PUT with Content-Length: 0 finalizes whatever the session holds.
//   - PUT with neither Content-Length nor Content-Range is a 400.
//   - Finalizing verifies the content against the digest parameter.
//
// Everything else (blobs, manifests, tags) is a plain map so backend
// round-trips work; reads support open-ended Range requests.
package fakeregistry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry is an http.Handler implementing the distribution API subset
// Shpiel's ociclient speaks. Zero value is not usable; call New.
type Registry struct {
	mu        sync.Mutex
	blobs     map[string][]byte   // "<repo>@<digest>" -> content
	manifests map[string][]byte   // "<repo>:<ref>" -> manifest bytes
	tags      map[string][]string // repo -> sorted tags
	uploads   map[string]*upload  // session id -> state
}

type upload struct {
	repo string
	buf  []byte
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{
		blobs:     map[string][]byte{},
		manifests: map[string][]byte{},
		tags:      map[string][]string{},
		uploads:   map[string]*upload{},
	}
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/v2")
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch {
	case strings.HasSuffix(path, "/blobs/uploads/") && req.Method == http.MethodPost:
		r.startUpload(w, strings.TrimSuffix(path, "/blobs/uploads/"))
	case strings.Contains(path, "/blobs/uploads/"):
		repo, id, _ := strings.Cut(path, "/blobs/uploads/")
		r.serveUpload(w, req, repo, id)
	case strings.Contains(path, "/blobs/"):
		i := strings.LastIndex(path, "/blobs/")
		r.serveBlob(w, req, path[:i], path[i+len("/blobs/"):])
	case strings.HasSuffix(path, "/tags/list") && req.Method == http.MethodGet:
		r.serveTags(w, strings.TrimSuffix(path, "/tags/list"))
	case strings.Contains(path, "/manifests/"):
		i := strings.LastIndex(path, "/manifests/")
		r.serveManifest(w, req, path[:i], path[i+len("/manifests/"):])
	default:
		http.NotFound(w, req)
	}
}

func (r *Registry) startUpload(w http.ResponseWriter, repo string) {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	session := hex.EncodeToString(id)
	r.mu.Lock()
	r.uploads[session] = &upload{repo: repo}
	r.mu.Unlock()
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, session))
	w.WriteHeader(http.StatusAccepted)
}

// serveUpload handles PATCH (chunk) and PUT (finalize) on a session.
func (r *Registry) serveUpload(w http.ResponseWriter, req *http.Request, repo, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	up := r.uploads[id]
	if up == nil || up.repo != repo {
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "no such upload session")
		return
	}

	contentLen, err := strconv.ParseInt(req.Header.Get("Content-Length"), 10, 64)
	hasLen := err == nil
	contentRange := req.Header.Get("Content-Range")

	switch req.Method {
	case http.MethodPatch:
		if !hasLen || contentRange == "" {
			// Streamed upload: bytes append at the session end.
			body, err := io.ReadAll(req.Body)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
				return
			}
			up.buf = append(up.buf, body...)
		} else {
			from, to, ok := parseRange(contentRange)
			if !ok || to-from+1 != contentLen {
				writeError(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "bad content range")
				return
			}
			if !r.appendChunk(w, up, from, req.Body) {
				return
			}
		}
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, id))
		w.Header().Set("Range", fmt.Sprintf("0-%d", len(up.buf)-1))
		w.WriteHeader(http.StatusAccepted)

	case http.MethodPut:
		digest := req.URL.Query().Get("digest")
		if digest == "" {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest parameter required")
			return
		}
		if !hasLen && contentRange == "" {
			writeError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "need Content-Length or Content-Range")
			return
		}
		switch {
		case contentRange != "":
			from, to, ok := parseRange(contentRange)
			if !ok || (hasLen && to-from+1 != contentLen) {
				writeError(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "bad content range")
				return
			}
			if !r.appendChunk(w, up, from, req.Body) {
				return
			}
		case contentLen > 0:
			// Monolithic completion: Zot reads no-Content-Range as "from
			// offset 0" and rejects it once the session holds bytes.
			if !r.appendChunk(w, up, 0, req.Body) {
				return
			}
		}
		sum := sha256.Sum256(up.buf)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "content does not match digest "+digest)
			return
		}
		r.blobs[repo+"@"+digest] = up.buf
		delete(r.uploads, id)
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", repo, digest))
		w.WriteHeader(http.StatusCreated)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// appendChunk enforces Zot's contiguity check (PutBlobChunk: the chunk's
// start must equal the bytes already in the session) and appends. Reports
// whether the caller may proceed; on failure the 416 is already written.
func (r *Registry) appendChunk(w http.ResponseWriter, up *upload, from int64, body io.Reader) bool {
	if from != int64(len(up.buf)) {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID",
			fmt.Sprintf("chunk starts at %d, session has %d bytes", from, len(up.buf)))
		return false
	}
	data, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return false
	}
	up.buf = append(up.buf, data...)
	return true
}

func (r *Registry) serveBlob(w http.ResponseWriter, req *http.Request, repo, digest string) {
	r.mu.Lock()
	content, ok := r.blobs[repo+"@"+digest]
	r.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "no such blob")
		return
	}
	switch req.Method {
	case http.MethodHead:
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if rng := req.Header.Get("Range"); rng != "" {
			offset, ok := parseByteRangeStart(rng)
			if !ok || offset >= int64(len(content)) {
				writeError(w, http.StatusRequestedRangeNotSatisfiable, "RANGE_INVALID", "bad range")
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(content)-1, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[offset:])
			return
		}
		_, _ = w.Write(content)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (r *Registry) serveManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := repo + ":" + ref
	switch req.Method {
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(req.Body, 16<<20))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
			return
		}
		r.manifests[key] = body
		if !contains(r.tags[repo], ref) {
			r.tags[repo] = append(r.tags[repo], ref)
			sort.Strings(r.tags[repo])
		}
		sum := sha256.Sum256(body)
		w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet, http.MethodHead:
		m, ok := r.manifests[key]
		if !ok {
			writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "no such manifest")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Content-Length", strconv.Itoa(len(m)))
		if req.Method == http.MethodGet {
			_, _ = w.Write(m)
		}
	case http.MethodDelete:
		if _, ok := r.manifests[key]; !ok {
			writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "no such manifest")
			return
		}
		delete(r.manifests, key)
		r.tags[repo] = remove(r.tags[repo], ref)
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (r *Registry) serveTags(w http.ResponseWriter, repo string) {
	r.mu.Lock()
	tags := append([]string(nil), r.tags[repo]...)
	r.mu.Unlock()
	if len(tags) == 0 {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "no such repository")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
}

// parseRange parses the OCI upload Content-Range form "<start>-<end>"
// (no "bytes=" prefix), the grammar Zot's getContentRange accepts.
func parseRange(s string) (from, to int64, ok bool) {
	a, b, found := strings.Cut(s, "-")
	if !found {
		return 0, 0, false
	}
	from, err1 := strconv.ParseInt(a, 10, 64)
	to, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || from > to {
		return 0, 0, false
	}
	return from, to, true
}

// parseByteRangeStart parses "bytes=N-" (open-ended HTTP range), which is
// the only form ociclient sends on reads.
func parseByteRangeStart(s string) (int64, bool) {
	s, ok := strings.CutPrefix(s, "bytes=")
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, "-")
	if !ok || strings.Contains(s, ",") {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// writeError emits the distribution-spec error envelope; the code string
// (e.g. BLOB_UPLOAD_INVALID) is what shows up in client error logs.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{{"code": code, "message": message}},
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func remove(list []string, s string) []string {
	out := list[:0]
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
