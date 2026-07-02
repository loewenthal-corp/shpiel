package server

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/loewenthal-corp/shpiel/internal/audit"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/relay"
)

// maxInlineFileSize bounds base64-inlined commit files; larger content must
// go through the LFS flow (preupload steers clients accordingly).
const maxInlineFileSize = 10 << 20

// lfsExtensions mirrors the Hub's default .gitattributes: these always
// upload as LFS regardless of size.
var lfsExtensions = map[string]bool{
	".safetensors": true, ".bin": true, ".pt": true, ".pth": true,
	".ckpt": true, ".gguf": true, ".onnx": true, ".h5": true,
	".msgpack": true, ".parquet": true, ".arrow": true, ".npy": true,
	".npz": true, ".tflite": true, ".pb": true, ".pickle": true,
	".pkl": true, ".model": true, ".zip": true, ".tar": true, ".gz": true,
}

// handleCreateRepo serves POST /api/repos/create.
func (s *Server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorizeWrite(w, r)
	if !ok {
		return
	}
	var req hfapi.CreateRepoRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, "Invalid JSON body.")
		return
	}
	kind, ok := repoKindFromType(req.Type)
	if !ok {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest,
			fmt.Sprintf("Repo type %q is not supported.", req.Type))
		return
	}
	repo, err := repoFromCreatePayload(req.Name, req.Organization)
	if err != nil {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, err.Error())
		return
	}

	scheme, host := requestSchemeHost(r)
	repoURL := fmt.Sprintf("%s://%s/%s", scheme, host, repo)
	if err := s.relay.CreateRepo(r.Context(), kind, repo); err != nil {
		if errors.Is(err, relay.ErrRepoExists) {
			// The Hub's 409 body carries the repo url alongside the error;
			// huggingface_hub's exist_ok=True parses it even on conflict.
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "You already created this model repo",
				"url":   repoURL,
				"name":  repo.String(),
				"id":    repo.String(),
			})
			return
		}
		s.writeError(w, r, err)
		return
	}
	s.audit.Record(audit.Event{Action: "repo_create", Actor: actor, Repo: repo.String()})
	writeJSON(w, http.StatusOK, hfapi.CreateRepoResponse{
		URL:  repoURL,
		Name: repo.String(),
		ID:   repo.String(),
	})
}

// handleDeleteRepo serves DELETE /api/repos/delete.
func (s *Server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorizeWrite(w, r)
	if !ok {
		return
	}
	var req hfapi.DeleteRepoRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, "Invalid JSON body.")
		return
	}
	kind, ok := repoKindFromType(req.Type)
	if !ok {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest,
			fmt.Sprintf("Repo type %q is not supported.", req.Type))
		return
	}
	repo, err := repoFromCreatePayload(req.Name, req.Organization)
	if err != nil {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, err.Error())
		return
	}
	if err := s.relay.DeleteRepo(r.Context(), kind, repo); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit.Record(audit.Event{Action: "repo_delete", Actor: actor, Repo: repo.String()})
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// repoFromCreatePayload accepts both payload dialects huggingface_hub has
// used: a fully-qualified name ("org/name") or name + organization.
func repoFromCreatePayload(name, organization string) (hfapi.RepoID, error) {
	if organization != "" && !strings.Contains(name, "/") {
		name = organization + "/" + name
	}
	return hfapi.ParseRepoID(name)
}

// handlePreupload serves POST /api/{type}s/{id}/preupload/{rev}: the server
// tells the client which files ship inline in the commit versus through
// the LFS flow.
func (s *Server) handlePreupload(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeWrite(w, r); !ok {
		return
	}
	route := routeFrom(r)
	// Resolve the target so unknown repos/revisions fail here with proper
	// error codes, before the client starts shipping bytes.
	if _, err := s.relay.ResolveManifest(r.Context(), route.RepoKind, route.Repo, revision(route), bearerToken(r)); err != nil {
		s.writeError(w, r, err)
		return
	}

	var req hfapi.PreuploadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, "Invalid JSON body.")
		return
	}
	resp := hfapi.PreuploadResponse{Files: make([]hfapi.PreuploadResponseFile, 0, len(req.Files))}
	for _, f := range req.Files {
		resp.Files = append(resp.Files, hfapi.PreuploadResponseFile{
			Path:       f.Path,
			UploadMode: uploadModeFor(f.Path, f.Size, f.Sample),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// uploadModeFor mirrors the Hub's steering: known binary/model extensions
// and big or binary-sniffed content go LFS; small text ships inline.
func uploadModeFor(p string, size int64, sampleB64 string) string {
	if lfsExtensions[strings.ToLower(path.Ext(p))] {
		return hfapi.UploadModeLFS
	}
	if size > maxInlineFileSize {
		return hfapi.UploadModeLFS
	}
	if sample, err := base64.StdEncoding.DecodeString(sampleB64); err == nil && strings.ContainsRune(string(sample), 0) {
		return hfapi.UploadModeLFS
	}
	return hfapi.UploadModeRegular
}

// handleLFSBatch serves POST /{id}.git/info/lfs/objects/batch. Objects the
// backend already holds get no actions — the client skips those uploads,
// which is blob-level dedup across commits and fine-tunes.
func (s *Server) handleLFSBatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeWrite(w, r); !ok {
		return
	}
	route := routeFrom(r)
	var req hfapi.LFSBatchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, "Invalid JSON body.")
		return
	}
	if req.Operation != "upload" {
		writeHFError(w, http.StatusUnprocessableEntity, "", "Only the upload operation is supported.")
		return
	}

	scheme, host := requestSchemeHost(r)
	resp := hfapi.LFSBatchResponse{Transfer: "basic", HashAlgo: "sha256"}
	for _, obj := range req.Objects {
		out := hfapi.LFSBatchResponseObject{OID: obj.OID, Size: obj.Size}
		if !isSHA256Hex(obj.OID) {
			out.Error = &hfapi.LFSBatchResponseObjError{Code: http.StatusUnprocessableEntity, Message: "oid must be 64 hex chars (sha256)"}
		} else if !s.relay.HasLFSBlob(r.Context(), route.RepoKind, route.Repo, obj.OID) {
			action := &hfapi.LFSAction{
				Href: fmt.Sprintf("%s://%s/shpiel-lfs/%s/%s/%s?size=%d",
					scheme, host, route.RepoKind.APIPrefix(), route.Repo, obj.OID, obj.Size),
			}
			// The client sends action headers verbatim on the PUT; echo
			// the caller's credentials so passthrough auth holds there.
			if auth := r.Header.Get("Authorization"); auth != "" {
				action.Header = map[string]string{"Authorization": auth}
			}
			out.Actions = map[string]*hfapi.LFSAction{"upload": action}
		}
		resp.Objects = append(resp.Objects, out)
	}
	w.Header().Set("Content-Type", hfapi.LFSContentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// handleLFSUpload serves PUT /shpiel-lfs/{models|datasets}/{id}/{oid}, the
// href minted by the batch API. Content is digest-verified by the backend.
func (s *Server) handleLFSUpload(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorizeWrite(w, r)
	if !ok {
		return
	}
	segs := strings.Split(strings.Trim(r.PathValue("rest"), "/"), "/")
	if len(segs) < 2 || len(segs) > 3 {
		writeHFError(w, http.StatusNotFound, "", "Not found.")
		return
	}
	var kind hfapi.RepoKind
	switch r.PathValue("kind") {
	case "models":
		kind = hfapi.RepoKindModel
	case "datasets":
		kind = hfapi.RepoKindDataset
	default:
		writeHFError(w, http.StatusNotFound, "", "Not found.")
		return
	}
	oid := segs[len(segs)-1]
	repo, err := hfapi.ParseRepoID(strings.Join(segs[:len(segs)-1], "/"))
	if err != nil || !isSHA256Hex(oid) {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, "Invalid LFS upload path.")
		return
	}
	size := int64(-1)
	if v, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64); err == nil {
		size = v
	}

	if err := s.acquireUploadSlot(r.Context()); err != nil {
		writeHFError(w, http.StatusServiceUnavailable, "", "Server is at its concurrent upload limit.")
		return
	}
	defer s.releaseUploadSlot()

	if err := s.relay.PutLFSBlob(r.Context(), kind, repo, oid, size, r.Body); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit.Record(audit.Event{
		Action: "lfs_upload", Actor: actor, Repo: repo.String(),
		Digest: "sha256:" + oid, Detail: map[string]any{"bytes": size},
	})
	w.WriteHeader(http.StatusOK)
}

// handleCommit serves POST /api/{type}s/{id}/commit/{rev} (NDJSON).
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorizeWrite(w, r)
	if !ok {
		return
	}
	route := routeFrom(r)
	if r.URL.Query().Get("create_pr") == "1" {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, "Pull requests are not supported by Shpiel.")
		return
	}

	ops, err := parseCommitNDJSON(r)
	if err != nil {
		writeHFError(w, http.StatusBadRequest, hfapi.ErrorCodeBadRequest, err.Error())
		return
	}

	result, err := s.relay.Commit(r.Context(), route.RepoKind, route.Repo, revision(route), ops)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.metrics.Commits.WithLabelValues("ok").Inc()
	s.audit.Record(audit.Event{
		Action: "commit", Actor: actor, Repo: route.Repo.String(),
		Revision: revision(route), Commit: result.CommitSHA,
		Detail: map[string]any{
			"summary": ops.Summary, "files": len(ops.Files) + len(ops.LFSFiles),
			"deleted": len(ops.DeletedFiles) + len(ops.DeletedFolders),
		},
	})
	scheme, host := requestSchemeHost(r)
	writeJSON(w, http.StatusOK, hfapi.CommitResponse{
		CommitURL:  fmt.Sprintf("%s://%s/%s/commit/%s", scheme, host, route.Repo, result.CommitSHA),
		CommitOID:  result.CommitSHA,
		HookOutput: "",
		Success:    true,
	})
}

// parseCommitNDJSON decodes the commit payload: one JSON object per line.
func parseCommitNDJSON(r *http.Request) (*relay.CommitOps, error) {
	ops := &relay.CommitOps{}
	sc := bufio.NewScanner(http.MaxBytesReader(nil, r.Body, 512<<20))
	// Inline files arrive base64 in a single line; size the line buffer for
	// maxInlineFileSize plus base64 and JSON overhead.
	sc.Buffer(make([]byte, 0, 64<<10), maxInlineFileSize*3/2+(1<<20))

	sawHeader := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("invalid NDJSON line: %v", err)
		}
		switch raw.Key {
		case "header":
			var h hfapi.CommitHeader
			if err := json.Unmarshal(raw.Value, &h); err != nil {
				return nil, fmt.Errorf("invalid commit header: %v", err)
			}
			ops.Summary, ops.ParentCommit = h.Summary, h.ParentCommit
			sawHeader = true
		case "file":
			var f hfapi.CommitFile
			if err := json.Unmarshal(raw.Value, &f); err != nil {
				return nil, fmt.Errorf("invalid file line: %v", err)
			}
			if f.Encoding != "" && f.Encoding != "base64" {
				return nil, fmt.Errorf("unsupported file encoding %q", f.Encoding)
			}
			content, err := base64.StdEncoding.DecodeString(f.Content)
			if err != nil {
				return nil, fmt.Errorf("file %s: invalid base64 content", f.Path)
			}
			if len(content) > maxInlineFileSize {
				return nil, fmt.Errorf("file %s exceeds the inline limit; upload it via LFS", f.Path)
			}
			ops.Files = append(ops.Files, relay.CommitOpFile{Path: f.Path, Content: content})
		case "lfsFile":
			var f hfapi.CommitLFSFile
			if err := json.Unmarshal(raw.Value, &f); err != nil {
				return nil, fmt.Errorf("invalid lfsFile line: %v", err)
			}
			if f.Algo != "" && f.Algo != "sha256" {
				return nil, fmt.Errorf("lfsFile %s: unsupported hash algo %q", f.Path, f.Algo)
			}
			if !isSHA256Hex(f.OID) {
				return nil, fmt.Errorf("lfsFile %s: invalid oid", f.Path)
			}
			ops.LFSFiles = append(ops.LFSFiles, f)
		case "deletedFile":
			var d hfapi.CommitDeleted
			if err := json.Unmarshal(raw.Value, &d); err != nil {
				return nil, fmt.Errorf("invalid deletedFile line: %v", err)
			}
			ops.DeletedFiles = append(ops.DeletedFiles, d.Path)
		case "deletedFolder":
			var d hfapi.CommitDeleted
			if err := json.Unmarshal(raw.Value, &d); err != nil {
				return nil, fmt.Errorf("invalid deletedFolder line: %v", err)
			}
			ops.DeletedFolders = append(ops.DeletedFolders, d.Path)
		default:
			return nil, fmt.Errorf("unknown commit line key %q", raw.Key)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading commit payload: %v", err)
	}
	if !sawHeader {
		return nil, fmt.Errorf("commit payload has no header line")
	}
	return ops, nil
}
