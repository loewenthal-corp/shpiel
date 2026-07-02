// Package upstream implements the client side of the Hugging Face Hub API,
// used for pull-through fetches from huggingface.co (or any HF-compatible
// endpoint, including another Shpiel).
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/buildinfo"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// Sentinel errors mapped from upstream error codes / statuses.
var (
	ErrRepoNotFound     = errors.New("upstream: repo not found")
	ErrRevisionNotFound = errors.New("upstream: revision not found")
	ErrEntryNotFound    = errors.New("upstream: entry not found")
	ErrUnauthorized     = errors.New("upstream: unauthorized")
)

// Client talks to one HF-compatible upstream endpoint.
type Client struct {
	endpoint string
	// orgToken is the server-side token used for fetches when set;
	// otherwise the caller's token (if any) is forwarded.
	orgToken string
	http     *http.Client
}

// New creates a client for endpoint. orgToken may be empty (anonymous or
// caller-token passthrough).
func New(endpoint, orgToken string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		orgToken: orgToken,
		http: &http.Client{
			// No overall timeout: file bodies can stream for a long time.
			// Dial/TLS bounds come from DefaultTransport; callers bound
			// metadata requests with contexts.
			Timeout: 0,
		},
	}
}

// Endpoint returns the configured upstream base URL.
func (c *Client) Endpoint() string { return c.endpoint }

func (c *Client) authorize(r *http.Request, callerToken string) {
	switch {
	case c.orgToken != "":
		r.Header.Set("Authorization", "Bearer "+c.orgToken)
	case callerToken != "":
		r.Header.Set("Authorization", "Bearer "+callerToken)
	}
	r.Header.Set("User-Agent", "shpiel/"+buildinfo.Version)
}

// GetModelInfo fetches repo metadata at a revision, with blob details
// (sizes, digests, LFS info) so a complete manifest can be built.
func (c *Client) GetModelInfo(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, revision, callerToken string) (*hfapi.ModelInfo, error) {
	u := fmt.Sprintf("%s/api/%s/%s/revision/%s?blobs=true", c.endpoint, kind.APIPrefix(), repo, url.PathEscape(revision))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req, callerToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: fetching %s@%s: %w", repo, revision, err)
	}
	defer resp.Body.Close()
	if err := errorFromResponse(resp); err != nil {
		return nil, err
	}
	var info hfapi.ModelInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&info); err != nil {
		return nil, fmt.Errorf("upstream: decoding model info for %s@%s: %w", repo, revision, err)
	}
	return &info, nil
}

// FileMeta is what a resolve HEAD tells us about a file.
type FileMeta struct {
	CommitSHA  string
	ETag       string // unquoted
	LinkedETag string // unquoted; set for LFS files
	Size       int64
}

// StatFile HEADs a resolve URL to learn a file's identity without
// downloading it.
func (c *Client) StatFile(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, revision, path, callerToken string) (FileMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.resolveURL(kind, repo, revision, path), nil)
	if err != nil {
		return FileMeta{}, err
	}
	c.authorize(req, callerToken)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.http.Do(req)
	if err != nil {
		return FileMeta{}, fmt.Errorf("upstream: stat %s@%s/%s: %w", repo, revision, path, err)
	}
	defer resp.Body.Close()
	if err := errorFromResponse(resp); err != nil {
		return FileMeta{}, err
	}
	return fileMetaFromHeaders(resp), nil
}

// OpenFile GETs a file's content at a revision, following the Hub's CDN
// redirects. The returned reader must be closed by the caller.
func (c *Client) OpenFile(ctx context.Context, kind hfapi.RepoKind, repo hfapi.RepoID, revision, path, callerToken string) (io.ReadCloser, FileMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolveURL(kind, repo, revision, path), nil)
	if err != nil {
		return nil, FileMeta{}, err
	}
	c.authorize(req, callerToken)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, FileMeta{}, fmt.Errorf("upstream: fetching %s@%s/%s: %w", repo, revision, path, err)
	}
	if err := errorFromResponse(resp); err != nil {
		resp.Body.Close()
		return nil, FileMeta{}, err
	}
	return resp.Body, fileMetaFromHeaders(resp), nil
}

// WhoAmI proxies a whoami-v2 call with the caller's token, returning the
// raw status code and body so passthrough auth preserves upstream
// semantics exactly.
func (c *Client) WhoAmI(ctx context.Context, callerToken string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/api/whoami-v2", nil)
	if err != nil {
		return 0, nil, err
	}
	if callerToken != "" {
		req.Header.Set("Authorization", "Bearer "+callerToken)
	}
	req.Header.Set("User-Agent", "shpiel/"+buildinfo.Version)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("upstream: whoami: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("upstream: reading whoami body: %w", err)
	}
	return resp.StatusCode, body, nil
}

// Ping checks upstream reachability (any HTTP response counts).
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.endpoint+"/api/models/none/none", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upstream: unreachable: %w", err)
	}
	resp.Body.Close()
	return nil
}

func (c *Client) resolveURL(kind hfapi.RepoKind, repo hfapi.RepoID, revision, path string) string {
	prefix := ""
	if kind == hfapi.RepoKindDataset {
		prefix = "/datasets"
	}
	return fmt.Sprintf("%s%s/%s/resolve/%s/%s", c.endpoint, prefix, repo, url.PathEscape(revision), escapePath(path))
}

// escapePath escapes each segment of a file path but keeps the slashes.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func fileMetaFromHeaders(resp *http.Response) FileMeta {
	meta := FileMeta{
		CommitSHA:  resp.Header.Get(hfapi.HeaderRepoCommit),
		ETag:       unquoteETag(resp.Header.Get("ETag")),
		LinkedETag: unquoteETag(resp.Header.Get(hfapi.HeaderLinkedETag)),
	}
	if v := resp.Header.Get(hfapi.HeaderLinkedSize); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			meta.Size = n
		}
	} else if resp.ContentLength >= 0 {
		meta.Size = resp.ContentLength
	}
	return meta
}

func unquoteETag(s string) string {
	s = strings.TrimPrefix(s, "W/")
	return strings.Trim(s, `"`)
}

// errorFromResponse maps upstream error responses to sentinel errors using
// the X-Error-Code header, falling back to status codes.
func errorFromResponse(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	code := resp.Header.Get(hfapi.HeaderErrorCode)
	switch code {
	case hfapi.ErrorCodeRepoNotFound:
		return ErrRepoNotFound
	case hfapi.ErrorCodeRevisionNotFound:
		return ErrRevisionNotFound
	case hfapi.ErrorCodeEntryNotFound:
		return ErrEntryNotFound
	case hfapi.ErrorCodeGatedRepo:
		return fmt.Errorf("%w: gated repo", ErrUnauthorized)
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrEntryNotFound
	}
	return fmt.Errorf("upstream: unexpected status %d for %s", resp.StatusCode, resp.Request.URL.Path)
}

// ManifestFromModelInfo converts upstream repo metadata into a Shpiel
// manifest. Files whose digests cannot be determined from the listing get
// zero digests; the relay backfills them via StatFile before serving.
func ManifestFromModelInfo(kind hfapi.RepoKind, repo hfapi.RepoID, info *hfapi.ModelInfo, fetchedAt time.Time) (*backend.Manifest, error) {
	if info.SHA == "" {
		return nil, fmt.Errorf("upstream: model info for %s has no commit sha", repo)
	}
	m := &backend.Manifest{
		Repo:      repo,
		Kind:      kind,
		CommitSHA: info.SHA,
		FetchedAt: fetchedAt,
		CreatedAt: info.LastModified,
	}
	for _, s := range info.Siblings {
		entry := backend.FileEntry{Path: s.RFilename}
		if s.Size != nil {
			entry.Size = *s.Size
		}
		entry.OID = s.BlobID
		if s.LFS != nil {
			hex := s.LFS.SHA256
			if hex == "" {
				hex = s.LFS.OID
			}
			if hex != "" {
				entry.Digest = backend.SHA256Digest(hex)
				entry.LFS = &hfapi.LFSInfo{
					SHA256:      hex,
					OID:         hex,
					Size:        s.LFS.Size,
					PointerSize: s.LFS.PointerSize,
				}
				if s.LFS.Size > 0 {
					entry.Size = s.LFS.Size
				}
			}
		} else if s.BlobID != "" {
			entry.Digest = backend.SHA1Digest(s.BlobID)
		}
		m.Files = append(m.Files, entry)
	}
	return m, nil
}
