package hfapi

// Write-path wire types: repo creation, preupload negotiation, the git-lfs
// batch protocol, and the NDJSON commit payload. Shapes mirror what
// huggingface_hub's create_repo / upload_folder / create_commit send.

// CreateRepoRequest is the body of POST /api/repos/create.
type CreateRepoRequest struct {
	Name         string `json:"name"`
	Organization string `json:"organization,omitempty"`
	Type         string `json:"type,omitempty"` // "model" (default) | "dataset" | "space"
	Private      bool   `json:"private,omitempty"`
	// SDK sends extra fields (sdk, license, ...); they are ignored.
}

// CreateRepoResponse is the body returned by POST /api/repos/create.
type CreateRepoResponse struct {
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
}

// DeleteRepoRequest is the body of DELETE /api/repos/delete.
type DeleteRepoRequest struct {
	Name         string `json:"name"`
	Organization string `json:"organization,omitempty"`
	Type         string `json:"type,omitempty"`
}

// PreuploadRequest is the body of POST /api/{type}s/{id}/preupload/{rev}.
type PreuploadRequest struct {
	Files []PreuploadFile `json:"files"`
	// GitAttributes / gitIgnore content the client may send; unused.
	GitAttributes string `json:"gitAttributes,omitempty"`
	GitIgnore     string `json:"gitIgnore,omitempty"`
}

// PreuploadFile describes one candidate upload.
type PreuploadFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Sample is base64 of the first bytes, used for binary sniffing.
	Sample string `json:"sample,omitempty"`
}

// PreuploadResponse tells the client how to ship each file.
type PreuploadResponse struct {
	Files []PreuploadResponseFile `json:"files"`
}

// PreuploadResponseFile is the server's decision for one file.
type PreuploadResponseFile struct {
	Path         string `json:"path"`
	UploadMode   string `json:"uploadMode"` // "lfs" | "regular"
	ShouldIgnore bool   `json:"shouldIgnore"`
	// OID of the existing file at this path, when unchanged detection is
	// supported. Empty means the client uploads unconditionally.
	OID string `json:"oid,omitempty"`
}

// Upload modes.
const (
	UploadModeLFS     = "lfs"
	UploadModeRegular = "regular"
)

// LFSBatchRequest is the git-lfs batch API request
// (POST /{id}.git/info/lfs/objects/batch).
type LFSBatchRequest struct {
	Operation string           `json:"operation"` // "upload" | "download"
	Transfers []string         `json:"transfers,omitempty"`
	Objects   []LFSBatchObject `json:"objects"`
	HashAlgo  string           `json:"hash_algo,omitempty"`
	Ref       *LFSBatchRef     `json:"ref,omitempty"`
}

// LFSBatchRef names the ref an LFS operation targets.
type LFSBatchRef struct {
	Name string `json:"name"`
}

// LFSBatchObject identifies one blob by sha256.
type LFSBatchObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// LFSBatchResponse is the batch API response.
type LFSBatchResponse struct {
	Transfer string                   `json:"transfer,omitempty"` // adapter chosen: "basic"
	Objects  []LFSBatchResponseObject `json:"objects"`
	HashAlgo string                   `json:"hash_algo,omitempty"`
}

// LFSBatchResponseObject carries per-object actions. A missing "actions"
// means the server already has the blob (dedup: client skips the upload).
type LFSBatchResponseObject struct {
	OID     string                    `json:"oid"`
	Size    int64                     `json:"size"`
	Actions map[string]*LFSAction     `json:"actions,omitempty"`
	Error   *LFSBatchResponseObjError `json:"error,omitempty"`
}

// LFSAction is one endpoint the client should hit ("upload", "verify").
type LFSAction struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header,omitempty"`
	// ExpiresIn seconds; 0 means no expiry communicated.
	ExpiresIn int `json:"expires_in,omitempty"`
}

// LFSBatchResponseObjError reports a per-object failure.
type LFSBatchResponseObjError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// LFSContentType is the git-lfs protocol media type.
const LFSContentType = "application/vnd.git-lfs+json"

// Commit NDJSON payload: one JSON object per line, dispatched on "key".

// CommitLine is one line of the NDJSON commit body.
type CommitLine struct {
	Key   string `json:"key"` // "header" | "file" | "lfsFile" | "deletedFile" | "deletedFolder"
	Value any    `json:"value"`
}

// CommitHeader is the value of the "header" line.
type CommitHeader struct {
	Summary      string `json:"summary"`
	Description  string `json:"description,omitempty"`
	ParentCommit string `json:"parentCommit,omitempty"`
}

// CommitFile is the value of a "file" line: small file content inline.
type CommitFile struct {
	Path     string `json:"path"`
	Content  string `json:"content"` // base64
	Encoding string `json:"encoding,omitempty"`
}

// CommitLFSFile is the value of an "lfsFile" line: a pointer to a blob
// previously uploaded via the LFS batch flow.
type CommitLFSFile struct {
	Path string `json:"path"`
	Algo string `json:"algo,omitempty"` // "sha256"
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// CommitDeleted is the value of "deletedFile" / "deletedFolder" lines.
type CommitDeleted struct {
	Path string `json:"path"`
}

// CommitResponse is returned by POST /api/{type}s/{id}/commit/{rev}.
type CommitResponse struct {
	CommitURL string `json:"commitUrl"`
	CommitOID string `json:"commitOid"`
	// HookOutput mirrors the Hub's field; always empty for Shpiel.
	HookOutput string `json:"hookOutput"`
	Success    bool   `json:"success"`
}
