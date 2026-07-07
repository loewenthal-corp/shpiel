package server

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/buildinfo"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/xet"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// revision returns the requested revision, defaulting to main.
func revision(route hfapi.Route) string {
	if route.Revision == "" {
		return hfapi.DefaultRevision
	}
	return route.Revision
}

// handleModelInfo serves GET /api/models/{id} and
// GET /api/models/{id}/revision/{rev}.
func (s *Server) handleModelInfo(w http.ResponseWriter, r *http.Request) {
	route := routeFrom(r)
	m, err := s.relay.ResolveManifest(r.Context(), route.RepoKind, route.Repo, revision(route), bearerToken(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, modelInfoFromManifest(m))
}

func modelInfoFromManifest(m *backend.Manifest) hfapi.ModelInfo {
	info := hfapi.ModelInfo{
		ID:           m.Repo.String(),
		ModelID:      m.Repo.String(),
		SHA:          m.CommitSHA,
		LastModified: m.CreatedAt,
		CreatedAt:    m.CreatedAt,
		Tags:         []string{},
		Siblings:     []hfapi.Sibling{},
	}
	for i := range m.Files {
		f := &m.Files[i]
		sib := hfapi.Sibling{RFilename: f.Path, BlobID: f.OID}
		if f.Size > 0 {
			size := f.Size
			sib.Size = &size
		}
		if f.LFS != nil {
			// Emit both "sha256" (siblings contract) and "oid" (tree
			// contract) spellings; strict clients read one and ignore the
			// other.
			sib.LFS = &hfapi.LFSInfo{
				SHA256:      f.LFS.SHA256,
				OID:         f.LFS.SHA256,
				Size:        f.LFS.Size,
				PointerSize: f.LFS.PointerSize,
			}
		}
		info.Siblings = append(info.Siblings, sib)
	}
	sort.Slice(info.Siblings, func(i, j int) bool { return info.Siblings[i].RFilename < info.Siblings[j].RFilename })
	return info
}

// handleTree serves GET /api/models/{id}/tree/{rev}[/{path}] with cursor
// pagination compatible with huggingface_hub's list_repo_tree.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	route := routeFrom(r)
	m, err := s.relay.ResolveManifest(r.Context(), route.RepoKind, route.Repo, revision(route), bearerToken(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	subpath := strings.Trim(route.Path, "/")
	recursive := r.URL.Query().Get("recursive") == "true" || r.URL.Query().Get("recursive") == "True"
	limit := 1000
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	offset := 0
	if cur := r.URL.Query().Get("cursor"); cur != "" {
		if decoded, err := base64.URLEncoding.DecodeString(cur); err == nil {
			if v, err := strconv.Atoi(string(decoded)); err == nil && v >= 0 {
				offset = v
			}
		}
	}

	entries := treeEntries(m, subpath, recursive)
	if subpath != "" && len(entries) == 0 {
		writeHFError(w, http.StatusNotFound, hfapi.ErrorCodeEntryNotFound, "Entry not found.")
		return
	}

	end := min(offset+limit, len(entries))
	if offset > len(entries) {
		offset = len(entries)
	}
	page := entries[offset:end]
	if end < len(entries) {
		next := *r.URL
		q := next.Query()
		q.Set("cursor", base64.URLEncoding.EncodeToString([]byte(strconv.Itoa(end))))
		q.Set("limit", strconv.Itoa(limit))
		next.RawQuery = q.Encode()
		next.Scheme, next.Host = requestSchemeHost(r)
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next.String()))
	}
	writeJSON(w, http.StatusOK, page)
}

// treeEntries lists manifest files under prefix. Non-recursive listings
// collapse deeper paths into synthesized directory entries, matching the
// Hub's tree semantics.
func treeEntries(m *backend.Manifest, prefix string, recursive bool) []hfapi.TreeEntry {
	var out []hfapi.TreeEntry
	dirs := map[string]bool{}
	for i := range m.Files {
		f := &m.Files[i]
		rel := f.Path
		if prefix != "" {
			var ok bool
			rel, ok = strings.CutPrefix(f.Path, prefix+"/")
			if !ok {
				continue
			}
		}
		head, rest, nested := strings.Cut(rel, "/")
		if nested && !recursive {
			dirPath := head
			if prefix != "" {
				dirPath = prefix + "/" + head
			}
			if !dirs[dirPath] {
				dirs[dirPath] = true
				out = append(out, hfapi.TreeEntry{
					Type: hfapi.TreeEntryTypeDirectory,
					OID:  syntheticOID(m.CommitSHA, dirPath),
					Path: dirPath,
				})
			}
			continue
		}
		_ = rest
		entry := hfapi.TreeEntry{
			Type: hfapi.TreeEntryTypeFile,
			OID:  f.OID,
			Size: f.Size,
			Path: f.Path,
		}
		if entry.OID == "" {
			entry.OID = f.Digest.Hex()
		}
		if f.LFS != nil {
			entry.LFS = &hfapi.LFSInfo{
				OID:         f.LFS.SHA256,
				SHA256:      f.LFS.SHA256,
				Size:        f.LFS.Size,
				PointerSize: f.LFS.PointerSize,
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// syntheticOID derives a stable pseudo tree-oid for directory entries; the
// Hub sends real git tree ids, clients only require presence and stability.
func syntheticOID(commitSHA, dirPath string) string {
	sum := sha1.Sum([]byte(commitSHA + ":" + dirPath))
	return hex.EncodeToString(sum[:])
}

// handleResolve serves HEAD and GET /{id}/resolve/{rev}/{path...} — the
// endpoint hf_hub_download, vLLM, and safetensors lazy loading live on.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	route := routeFrom(r)
	filePath := route.Path
	token := bearerToken(r)

	m, err := s.relay.ResolveManifest(r.Context(), route.RepoKind, route.Repo, revision(route), token)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	entry, err := s.relay.EnsureEntry(r.Context(), route.RepoKind, route.Repo, m, filePath, token)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	h := w.Header()
	h.Set(hfapi.HeaderRepoCommit, m.CommitSHA)
	h.Set("ETag", `"`+entry.ETag()+`"`)
	if entry.LFS != nil {
		h.Set(hfapi.HeaderLinkedETag, `"`+entry.LFS.SHA256+`"`)
		h.Set(hfapi.HeaderLinkedSize, strconv.FormatInt(entry.Size, 10))
		// Advertise chunk-level xet download when we hold a reconstruction
		// for this exact content; hf_xet clients then fetch through the
		// CAS API, everyone else ignores the headers.
		if s.xet != nil {
			if fileHash, ok := s.xet.Store().FileHashBySHA256(r.Context(), entry.LFS.SHA256); ok {
				scheme, host := requestSchemeHost(r)
				h.Set(xet.HeaderHash, fileHash)
				h.Set(xet.HeaderRefreshRoute, fmt.Sprintf("%s://%s/api/%s/%s/xet-read-token/%s",
					scheme, host, route.RepoKind.APIPrefix(), route.Repo, m.CommitSHA))
			}
		}
	}
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", contentTypeFor(entry.Path))
	h.Set("Content-Disposition", `inline; filename*=UTF-8''`+url.PathEscape(path.Base(entry.Path)))

	// HEAD answers from metadata alone: clients probe files (etag, size,
	// commit) without forcing a pull-through download.
	if r.Method == http.MethodHead {
		h.Set("Content-Length", strconv.FormatInt(entry.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := s.acquireDownloadSlot(r.Context()); err != nil {
		writeHFError(w, http.StatusServiceUnavailable, "", "Server is at its concurrent download limit.")
		return
	}
	defer s.releaseDownloadSlot()

	content, err := s.relay.OpenFile(r.Context(), route.RepoKind, route.Repo, m, filePath, token)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer content.Close()

	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, "", m.CreatedAt, content)
	s.metrics.DownloadBytes.WithLabelValues(content.Source).Add(float64(cw.bytes))
}

type countingWriter struct {
	http.ResponseWriter
	bytes int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func contentTypeFor(p string) string {
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" && path.Ext(p) == ".json" {
		return ct
	}
	return "application/octet-stream"
}

// handleXetToken serves GET /api/{type}s/{id}/xet-{read,write}-token/{rev}.
// When xet is enabled it mints a CAS token (connection info rides in
// response headers, where huggingface_hub looks); when disabled it answers
// the actionable 404 — hub 1.x uploads have no LFS fallback, so the
// message tells users exactly what to set.
func (s *Server) handleXetToken(w http.ResponseWriter, r *http.Request) {
	if s.xet == nil {
		writeHFError(w, http.StatusNotFound, "",
			"Xet is not enabled on this endpoint. Set HF_HUB_DISABLE_XET=1 to upload via LFS, or enable xet in the Shpiel config.")
		return
	}
	route := routeFrom(r)
	scope := route.Path // "read" or "write", from the URL keyword
	actor := "anonymous"
	if scope == "write" {
		var ok bool
		actor, ok = s.authorizeWrite(w, r)
		if !ok {
			return
		}
	}
	// Reads follow the read path's openness: mode none serves anonymously,
	// passthrough validates the caller's token upstream.
	if scope == "read" && s.cfg.Auth.Mode == "passthrough" {
		token := bearerToken(r)
		ok, name, err := s.validateToken(r.Context(), token)
		if err != nil || !ok {
			writeHFError(w, http.StatusUnauthorized, "", "Invalid user token.")
			return
		}
		actor = name
	}
	s.xet.WriteTokenResponse(w, r, scope, route.RepoKind, route.Repo, actor)
}

// handleWhoAmI serves GET /api/whoami-v2. In passthrough mode the call is
// proxied verbatim so upstream token semantics are preserved; otherwise a
// synthetic local identity is returned.
func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth.Mode == "passthrough" && s.upstream != nil {
		token := bearerToken(r)
		if token == "" {
			writeHFError(w, http.StatusUnauthorized, "", "Invalid credentials in Authorization header")
			return
		}
		status, body, err := s.upstream.WhoAmI(r.Context(), token)
		if err != nil {
			writeHFError(w, http.StatusBadGateway, "", "Upstream whoami failed.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
		return
	}
	writeJSON(w, http.StatusOK, hfapi.WhoAmI{
		Type:     "user",
		Name:     "shpiel",
		Fullname: "Shpiel Relay",
		Auth: hfapi.WhoAmIAuth{
			Type:        "access_token",
			AccessToken: &hfapi.WhoAmIAccessToken{DisplayName: "shpiel", Role: "write"},
		},
		Orgs: []hfapi.WhoAmIOrg{},
	})
}

// handleValidateYAML serves POST /api/validate-yaml, which huggingface_hub
// calls before committing a README/model card: on 1.x it is
// HfApi._validate_yaml — upload_folder and friends hit it whenever a
// README.md is among the files — and on 0.x RepoCard.push_to_hub. The Hub
// validates the card's YAML metadata block against its card schema;
// Shpiel validates that the block parses as a YAML mapping — enough that
// real cards pass and broken metadata fails.
//
// The response contract comes from HfApi._validate_yaml: a JSON body —
// which the client parses unconditionally, on the 400 path too — with
// "warnings" and "errors" lists of {"message": ...}; 200 means
// committable, 400 means rejected. 1.x sends JSON {"repoType","content"};
// 0.x releases form-encoded the same keys.
func (s *Server) handleValidateYAML(w http.ResponseWriter, r *http.Request) {
	var content string
	body := http.MaxBytesReader(w, r.Body, maxInlineFileSize)
	if strings.Contains(r.Header.Get("Content-Type"), "json") {
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, "Invalid JSON body.")
			return
		}
		content = req.Content
	} else {
		r.Body = body
		if err := r.ParseForm(); err != nil {
			writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, "Invalid form body.")
			return
		}
		content = r.PostFormValue("content")
	}

	type note struct {
		Message string `json:"message"`
	}
	resp := struct {
		Warnings []note `json:"warnings"`
		Errors   []note `json:"errors"`
	}{Warnings: []note{}, Errors: []note{}}

	warning, err := validateCardYAML(content)
	if warning != "" {
		resp.Warnings = append(resp.Warnings, note{Message: warning})
	}
	if err != nil {
		resp.Errors = append(resp.Errors, note{Message: err.Error()})
		w.Header().Set(hfapi.HeaderErrorCode, hfapi.ErrorCodeBadRequest)
		writeJSON(w, http.StatusBadRequest, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// cardYAMLBlock mirrors huggingface_hub's REGEX_YAML_BLOCK: the metadata
// is a leading "---" line through the next "---" line.
var cardYAMLBlock = regexp.MustCompile(`\A(\s*---[\r\n]+)([\S\s]*?)([\r\n]+---(?:\r\n|\n|\z))`)

// validateCardYAML checks a card's metadata block: absent or empty
// metadata is a warning (the Hub warns the same way), unparseable or
// non-mapping metadata is an error.
func validateCardYAML(content string) (warning string, err error) {
	const emptyMetadata = "empty or missing yaml metadata in card"
	m := cardYAMLBlock.FindStringSubmatch(content)
	if m == nil {
		return emptyMetadata, nil
	}
	var meta any
	if err := yaml.Unmarshal([]byte(m[2]), &meta); err != nil {
		return "", fmt.Errorf("invalid YAML in card metadata: %v", err)
	}
	if meta == nil {
		return emptyMetadata, nil
	}
	if _, ok := meta.(map[string]any); !ok {
		return "", fmt.Errorf("card metadata must be a YAML mapping, got %T", meta)
	}
	return "", nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.relay.Ping(r.Context()); err != nil {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ready")
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "shpiel",
		"version": buildinfo.Version,
		"docs":    "https://github.com/loewenthal-corp/shpiel",
		"hint":    "set HF_ENDPOINT to this URL",
	})
}

func requestSchemeHost(r *http.Request) (string, string) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	return scheme, r.Host
}
