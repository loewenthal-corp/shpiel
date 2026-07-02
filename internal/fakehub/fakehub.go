// Package fakehub is a minimal in-process huggingface.co simulator used to
// test Shpiel's pull-through path hermetically. It implements just enough
// of the Hub API for the upstream client and real HF clients: revision
// info with blob details, resolve with correct headers and CDN-style
// redirects, whoami, and HF error semantics.
//
// It doubles as the "upstream" in the Tilt dev environment and e2e tests so
// nothing ever needs the public internet.
package fakehub

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// File is one fixture file.
type File struct {
	Content []byte
	LFS     bool
}

// Repo is a fixture model repository with one or more commits.
type Repo struct {
	ID hfapi.RepoID
	// Refs maps branch/tag names to commit SHAs.
	Refs map[string]string
	// Commits maps a commit SHA to its file set.
	Commits map[string]map[string]File
	Gated   bool
}

// Hub simulates huggingface.co.
type Hub struct {
	mu    sync.Mutex
	repos map[string]*Repo
	// counts tracks requests per "<method> <path>" for assertions about
	// pull-through behavior (e.g. singleflight collapsing).
	counts map[string]int
}

// New creates an empty hub.
func New() *Hub {
	return &Hub{repos: map[string]*Repo{}, counts: map[string]int{}}
}

// AddModel creates a repo at a deterministic commit SHA with the given
// files on "main". Files at or above lfsThreshold bytes (or listed in
// lfsPaths) are served as LFS. It returns the commit SHA.
func (h *Hub) AddModel(id string, files map[string][]byte, lfsPaths ...string) string {
	repoID, err := hfapi.ParseRepoID(id)
	if err != nil {
		panic(err)
	}
	lfs := map[string]bool{}
	for _, p := range lfsPaths {
		lfs[p] = true
	}
	commit := commitSHA(files)
	fileSet := map[string]File{}
	for p, content := range files {
		fileSet[p] = File{Content: content, LFS: lfs[p] || len(content) >= 1<<10}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	repo, ok := h.repos[repoID.String()]
	if !ok {
		repo = &Repo{ID: repoID, Refs: map[string]string{}, Commits: map[string]map[string]File{}}
		h.repos[repoID.String()] = repo
	}
	repo.Commits[commit] = fileSet
	repo.Refs["main"] = commit
	return commit
}

// SetRef points a ref at an existing commit (for testing moving branches).
func (h *Hub) SetRef(id, ref, commit string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.repos[id].Refs[ref] = commit
}

// SetGated marks a repo as gated (403 GatedRepo without a token).
func (h *Hub) SetGated(id string, gated bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.repos[id].Gated = gated
}

// Requests returns how many requests matched the "<method> <pathPrefix>".
func (h *Hub) Requests(method, pathPrefix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	total := 0
	for key, n := range h.counts {
		m, p, _ := strings.Cut(key, " ")
		if m == method && strings.HasPrefix(p, pathPrefix) {
			total += n
		}
	}
	return total
}

// commitSHA derives a deterministic 40-hex commit id from file contents.
func commitSHA(files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha1.New()
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(files[p]))
		h.Write(files[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// GitBlobOID computes the git blob object id for content, which is what
// the Hub uses as the ETag for non-LFS files.
func GitBlobOID(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// SHA256Hex computes the sha256 of content, the LFS OID.
func SHA256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// pointerSize is the size of the git-lfs pointer file for a blob.
func pointerSize(content []byte) int64 {
	return int64(len(fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n",
		SHA256Hex(content), len(content))))
}

// Handler returns the hub's HTTP handler. Like Shpiel's own server it
// dispatches on hfapi.ParseRoute — the Hub URL grammar overlaps too much
// for ServeMux patterns — with a /cdn/ prefix carved out for the redirect
// target host.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/whoami-v2", h.count(h.handleWhoAmI))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		// CDN-style content host path, target of resolve redirects. The
		// repo id is flattened ("org/name" -> "org--name") so bare repo
		// names parse unambiguously.
		mux.HandleFunc(method+" /cdn/{repodir}/{commit}/{path...}", h.count(h.handleCDN))
	}
	mux.HandleFunc("/", h.count(h.dispatch))
	return mux
}

func (h *Hub) dispatch(w http.ResponseWriter, r *http.Request) {
	route, ok := hfapi.ParseRoute(r.URL.EscapedPath())
	if !ok || route.RepoKind != hfapi.RepoKindModel {
		http.NotFound(w, r)
		return
	}
	switch route.Kind {
	case hfapi.RouteRepoInfo:
		h.handleModelInfo(w, r, route)
	case hfapi.RouteResolve:
		h.handleResolve(w, r, route)
	default:
		http.NotFound(w, r)
	}
}

func (h *Hub) count(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.counts[r.Method+" "+r.URL.Path]++
		h.mu.Unlock()
		next(w, r)
	}
}

func (h *Hub) lookup(r *http.Request, route hfapi.Route) (*Repo, string, map[string]File, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	repo, ok := h.repos[route.Repo.String()]
	if !ok {
		return nil, "", nil, errRepoNotFound
	}
	if repo.Gated && r.Header.Get("Authorization") == "" {
		return nil, "", nil, errGated
	}
	rev := route.Revision
	if rev == "" {
		rev = "main"
	}
	commit, ok := repo.Refs[rev]
	if !ok {
		if _, isCommit := repo.Commits[rev]; isCommit {
			commit = rev
		} else {
			return nil, "", nil, errRevisionNotFound
		}
	}
	return repo, commit, repo.Commits[commit], nil
}

var (
	errRepoNotFound     = fmt.Errorf("repo not found")
	errRevisionNotFound = fmt.Errorf("revision not found")
	errGated            = fmt.Errorf("gated")
)

func writeErr(w http.ResponseWriter, err error) {
	switch err {
	case errRepoNotFound:
		w.Header().Set(hfapi.HeaderErrorCode, hfapi.ErrorCodeRepoNotFound)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintln(w, `{"error":"Repository not found"}`)
	case errRevisionNotFound:
		w.Header().Set(hfapi.HeaderErrorCode, hfapi.ErrorCodeRevisionNotFound)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintln(w, `{"error":"Revision not found"}`)
	case errGated:
		w.Header().Set(hfapi.HeaderErrorCode, hfapi.ErrorCodeGatedRepo)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(w, `{"error":"Access to this repo is gated"}`)
	}
}

func (h *Hub) handleModelInfo(w http.ResponseWriter, r *http.Request, route hfapi.Route) {
	repo, commit, files, err := h.lookup(r, route)
	if err != nil {
		writeErr(w, err)
		return
	}
	withBlobs := r.URL.Query().Get("blobs") == "true"

	type lfsJSON struct {
		SHA256      string `json:"sha256"`
		Size        int64  `json:"size"`
		PointerSize int64  `json:"pointerSize"`
	}
	type siblingJSON struct {
		RFilename string   `json:"rfilename"`
		Size      *int64   `json:"size,omitempty"`
		BlobID    string   `json:"blobId,omitempty"`
		LFS       *lfsJSON `json:"lfs,omitempty"`
	}
	siblings := []siblingJSON{}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f := files[p]
		sib := siblingJSON{RFilename: p}
		if withBlobs {
			size := int64(len(f.Content))
			sib.Size = &size
			if f.LFS {
				// blobId for an LFS file is the git oid of the pointer.
				sib.BlobID = GitBlobOID(pointerContent(f.Content))
				sib.LFS = &lfsJSON{SHA256: SHA256Hex(f.Content), Size: size, PointerSize: pointerSize(f.Content)}
			} else {
				sib.BlobID = GitBlobOID(f.Content)
			}
		}
		siblings = append(siblings, sib)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"_id":          commit[:24],
		"id":           repo.ID.String(),
		"modelId":      repo.ID.String(),
		"sha":          commit,
		"lastModified": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Format(time.RFC3339),
		"createdAt":    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"private":      false,
		"gated":        repo.Gated,
		"disabled":     false,
		"downloads":    42,
		"likes":        7,
		"tags":         []string{},
		"siblings":     siblings,
	})
}

func pointerContent(content []byte) []byte {
	return []byte(fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n",
		SHA256Hex(content), len(content)))
}

// handleResolve mimics the Hub: metadata headers on both verbs; GET for
// LFS files answers 302 to a CDN path (headers still present, like S3
// offloading on the real Hub), regular files stream directly.
func (h *Hub) handleResolve(w http.ResponseWriter, r *http.Request, route hfapi.Route) {
	repo, commit, files, err := h.lookup(r, route)
	if err != nil {
		writeErr(w, err)
		return
	}
	p := route.Path
	f, ok := files[p]
	if !ok {
		w.Header().Set(hfapi.HeaderErrorCode, hfapi.ErrorCodeEntryNotFound)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintln(w, `{"error":"Entry not found"}`)
		return
	}

	hd := w.Header()
	hd.Set(hfapi.HeaderRepoCommit, commit)
	hd.Set("Accept-Ranges", "bytes")
	if f.LFS {
		hd.Set("ETag", `"`+SHA256Hex(f.Content)+`"`)
		hd.Set(hfapi.HeaderLinkedETag, `"`+SHA256Hex(f.Content)+`"`)
		hd.Set(hfapi.HeaderLinkedSize, strconv.Itoa(len(f.Content)))
	} else {
		hd.Set("ETag", `"`+GitBlobOID(f.Content)+`"`)
	}

	if r.Method == http.MethodHead {
		hd.Set("Content-Length", strconv.Itoa(len(f.Content)))
		w.WriteHeader(http.StatusOK)
		return
	}
	if f.LFS {
		// Real hub 302s LFS GETs to a CDN; exercise redirect-following in
		// clients and in Shpiel's upstream fetcher.
		cdnID := strings.ReplaceAll(repo.ID.String(), "/", "--")
		http.Redirect(w, r, fmt.Sprintf("/cdn/%s/%s/%s", cdnID, commit, p), http.StatusFound)
		return
	}
	hd.Set("Content-Length", strconv.Itoa(len(f.Content)))
	w.WriteHeader(http.StatusOK)
	w.Write(f.Content)
}

func (h *Hub) handleCDN(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	id := strings.ReplaceAll(r.PathValue("repodir"), "--", "/")
	repo, ok := h.repos[id]
	var f File
	var found bool
	if ok {
		if files, ok := repo.Commits[r.PathValue("commit")]; ok {
			f, found = files[r.PathValue("path")]
		}
	}
	h.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", `"`+SHA256Hex(f.Content)+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.Itoa(len(f.Content)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write(f.Content)
	}
}

func (h *Hub) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintln(w, `{"error":"Invalid credentials in Authorization header"}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "user", "name": "fakeuser", "fullname": "Fake Hub User",
		"auth": map[string]any{"type": "access_token", "accessToken": map[string]any{"displayName": "fake", "role": "write"}},
		"orgs": []any{},
	})
}
