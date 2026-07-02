// Package hfapi defines the Hugging Face Hub API wire contract: JSON payload
// types, HTTP headers, and error codes that HF clients (huggingface_hub, the
// hf CLI, vLLM, SGLang, TGI) depend on.
//
// Compatibility is the product: everything in this package mirrors observable
// behavior of huggingface.co, not an idealized redesign of it. Field names,
// header casing, and error-code strings are all part of the contract.
package hfapi

import "time"

// Header names the huggingface_hub client reads. The client treats these
// case-insensitively (per HTTP), but we emit the canonical casing the real
// Hub uses.
const (
	// HeaderRepoCommit carries the resolved commit SHA of the revision that
	// served the request. hf_hub_download stores this as the snapshot ID.
	HeaderRepoCommit = "X-Repo-Commit"

	// HeaderLinkedETag carries the content ETag for LFS files (the sha256 of
	// the actual bytes, not the pointer file). When present, clients prefer
	// it over the plain ETag as the file's cache key.
	HeaderLinkedETag = "X-Linked-Etag"

	// HeaderLinkedSize carries the byte size of the LFS content a resolve
	// URL points at.
	HeaderLinkedSize = "X-Linked-Size"

	// HeaderErrorCode carries a machine-readable error code on failure
	// responses. huggingface_hub raises typed exceptions based on it.
	HeaderErrorCode = "X-Error-Code"

	// HeaderErrorMessage optionally carries a human-readable error detail.
	HeaderErrorMessage = "X-Error-Message"
)

// Error codes recognized by huggingface_hub's hf_raise_for_status.
const (
	ErrorCodeRepoNotFound     = "RepoNotFound"
	ErrorCodeRevisionNotFound = "RevisionNotFound"
	ErrorCodeEntryNotFound    = "EntryNotFound"
	ErrorCodeGatedRepo        = "GatedRepo"
	ErrorCodeBadRequest       = "BadRequest"
)

// DefaultRevision is the ref served when a client does not specify one.
const DefaultRevision = "main"

// ErrorResponse is the JSON body the Hub returns for API errors.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ModelInfo is the payload of GET /api/models/{id} and
// GET /api/models/{id}/revision/{rev}.
//
// Only fields that real clients consume are guaranteed meaningful; the rest
// are populated with sensible defaults so strict parsers do not trip.
type ModelInfo struct {
	ID           string    `json:"id"`
	ModelID      string    `json:"modelId"`
	SHA          string    `json:"sha"`
	LastModified time.Time `json:"lastModified"`
	CreatedAt    time.Time `json:"createdAt"`
	Private      bool      `json:"private"`
	Gated        bool      `json:"gated"`
	Disabled     bool      `json:"disabled"`
	Downloads    int64     `json:"downloads"`
	Likes        int64     `json:"likes"`
	Tags         []string  `json:"tags"`
	PipelineTag  string    `json:"pipeline_tag,omitempty"`
	LibraryName  string    `json:"library_name,omitempty"`
	Siblings     []Sibling `json:"siblings"`
}

// Sibling is one file in a repo listing. rfilename is the path relative to
// the repo root.
type Sibling struct {
	RFilename string   `json:"rfilename"`
	Size      *int64   `json:"size,omitempty"`
	BlobID    string   `json:"blobId,omitempty"`
	LFS       *LFSInfo `json:"lfs,omitempty"`
}

// LFSInfo describes the LFS pointer for a large file.
type LFSInfo struct {
	// SHA256 of the actual file content (hex, no prefix). Serialized as
	// "sha256" in siblings and as "oid" in tree entries.
	SHA256      string `json:"sha256,omitempty"`
	OID         string `json:"oid,omitempty"`
	Size        int64  `json:"size"`
	PointerSize int64  `json:"pointerSize,omitempty"`
}

// TreeEntry is one entry in GET /api/models/{id}/tree/{rev}[/{path}].
type TreeEntry struct {
	Type string   `json:"type"` // "file" or "directory"
	OID  string   `json:"oid"`
	Size int64    `json:"size"`
	Path string   `json:"path"`
	LFS  *LFSInfo `json:"lfs,omitempty"`
}

// TreeEntryTypeFile and friends are the values of TreeEntry.Type.
const (
	TreeEntryTypeFile      = "file"
	TreeEntryTypeDirectory = "directory"
)

// WhoAmI is the payload of GET /api/whoami-v2.
type WhoAmI struct {
	Type      string      `json:"type"` // "user" or "org"
	Name      string      `json:"name"`
	Fullname  string      `json:"fullname,omitempty"`
	Email     string      `json:"email,omitempty"`
	Plan      string      `json:"plan,omitempty"`
	Auth      WhoAmIAuth  `json:"auth"`
	Orgs      []WhoAmIOrg `json:"orgs"`
	AvatarURL string      `json:"avatarUrl,omitempty"`
}

// WhoAmIAuth describes the token used for the whoami call.
type WhoAmIAuth struct {
	Type        string             `json:"type"` // "access_token"
	AccessToken *WhoAmIAccessToken `json:"accessToken,omitempty"`
}

// WhoAmIAccessToken describes an access token's display name and role.
type WhoAmIAccessToken struct {
	DisplayName string `json:"displayName,omitempty"`
	Role        string `json:"role,omitempty"` // "read" | "write" | "fineGrained"
}

// WhoAmIOrg is an organization membership in a whoami response.
type WhoAmIOrg struct {
	Type      string `json:"type"` // "org"
	Name      string `json:"name"`
	Fullname  string `json:"fullname,omitempty"`
	RoleInOrg string `json:"roleInOrg,omitempty"`
}
