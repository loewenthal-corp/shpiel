// Package ociclient is a minimal OCI distribution-spec client covering
// exactly what Shpiel's OCI backend needs: blob HEAD/GET (with ranges),
// monolithic and chunked blob upload, manifest put/get/delete, and tag
// listing. Auth supports anonymous, basic, and the bearer token challenge
// flow (docker registry v2 auth).
//
// Hand-rolled instead of oras-go: serving model weights needs ranged blob
// reads and digest-computed-at-the-end streaming uploads, both of which
// are awkward through higher-level libraries. The distribution API is
// small and stable.
package ociclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ErrNotFound is returned for missing blobs, manifests, and repositories.
var ErrNotFound = errors.New("ociclient: not found")

// OCI media types used by the backend.
const (
	MediaTypeOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIConfig   = "application/vnd.oci.image.config.v1+json"
	MediaTypeOCILayerTar = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeEmptyJSON   = "application/vnd.oci.empty.v1+json"
)

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Manifest is an OCI image/artifact manifest.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Client talks to one registry host.
type Client struct {
	base     string // e.g. "https://zot.internal:5000" or "http://zot:5000"
	username string
	password string
	http     *http.Client

	mu     sync.Mutex
	tokens map[string]string // auth scope -> bearer token
}

// New creates a client for a registry base URL (scheme required; use
// http:// for plaintext in-cluster registries).
func New(baseURL, username, password string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("ociclient: invalid registry url %q", baseURL)
	}
	return &Client{
		base:     strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		// A private transport: sharing http.DefaultTransport means anyone
		// calling CloseIdleConnections on it (httptest servers do on
		// Close) can break this client's in-flight requests.
		http:   &http.Client{Transport: cloneDefaultTransport()},
		tokens: map[string]string{},
	}, nil
}

func cloneDefaultTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return http.DefaultTransport
}

// SHA256Digest returns the OCI digest string for content.
func SHA256Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// do sends a request with auth, handling one bearer challenge round-trip.
func (c *Client) do(req *http.Request, scope string) (*http.Response, error) {
	c.authorize(req, scope)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	if !strings.HasPrefix(challenge, "Bearer ") {
		// Basic (or no) auth was already attached; a 401 is final.
		return nil, fmt.Errorf("ociclient: unauthorized for %s", req.URL.Path)
	}
	token, err := c.fetchToken(req.Context(), challenge)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.tokens[scope] = token
	c.mu.Unlock()

	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		retry.Body = body
	} else if req.Body != nil {
		return nil, fmt.Errorf("ociclient: cannot retry streaming request after auth challenge")
	}
	retry.Header.Set("Authorization", "Bearer "+token)
	return c.http.Do(retry)
}

func (c *Client) authorize(req *http.Request, scope string) {
	c.mu.Lock()
	token := c.tokens[scope]
	c.mu.Unlock()
	switch {
	case token != "":
		req.Header.Set("Authorization", "Bearer "+token)
	case c.username != "" || c.password != "":
		req.SetBasicAuth(c.username, c.password)
	}
}

// fetchToken implements the bearer challenge: parse realm/service/scope,
// hit the token endpoint (with basic creds when configured).
func (c *Client) fetchToken(ctx context.Context, challenge string) (string, error) {
	params := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(challenge, "Bearer "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			params[k] = strings.Trim(v, `"`)
		}
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("ociclient: bearer challenge without realm: %q", challenge)
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("ociclient: invalid token realm %q", realm)
	}
	q := u.Query()
	if params["service"] != "" {
		q.Set("service", params["service"])
	}
	if params["scope"] != "" {
		q.Set("scope", params["scope"])
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ociclient: token endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ociclient: token endpoint status %d", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("ociclient: decoding token: %w", err)
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	return tok.AccessToken, nil
}

// Ping verifies the registry speaks the v2 API.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req, "ping")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ociclient: /v2/ status %d", resp.StatusCode)
	}
	return nil
}

// HeadBlob reports a blob's size, or ErrNotFound.
func (c *Client) HeadBlob(ctx context.Context, repo, digest string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		fmt.Sprintf("%s/v2/%s/blobs/%s", c.base, repo, digest), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.do(req, "pull:"+repo)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.ContentLength, nil
	case http.StatusNotFound:
		return 0, ErrNotFound
	default:
		return 0, fmt.Errorf("ociclient: HEAD blob %s: status %d", digest, resp.StatusCode)
	}
}

// GetBlob streams a blob from offset (0 for the whole blob). Registries
// without usable Range support (ignored ranges, or 416 on open-ended
// ranges) get the prefix discarded client-side, so callers can rely on
// offset semantics either way.
func (c *Client) GetBlob(ctx context.Context, repo, digest string, offset int64) (io.ReadCloser, error) {
	rc, err := c.getBlobAttempt(ctx, repo, digest, offset, true)
	if errors.Is(err, errRangeUnsupported) {
		rc, err = c.getBlobAttempt(ctx, repo, digest, offset, false)
	}
	return rc, err
}

var errRangeUnsupported = errors.New("ociclient: range not supported")

func (c *Client) getBlobAttempt(ctx context.Context, repo, digest string, offset int64, useRange bool) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v2/%s/blobs/%s", c.base, repo, digest), nil)
	if err != nil {
		return nil, err
	}
	if offset > 0 && useRange {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.do(req, "pull:"+repo)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return resp.Body, nil
	case http.StatusOK:
		if offset > 0 {
			if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("ociclient: skipping to offset %d: %w", offset, err)
			}
		}
		return resp.Body, nil
	case http.StatusRequestedRangeNotSatisfiable:
		resp.Body.Close()
		if useRange {
			return nil, errRangeUnsupported
		}
		return nil, fmt.Errorf("ociclient: GET blob %s: status 416 without range", digest)
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrNotFound
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("ociclient: GET blob %s: status %d", digest, resp.StatusCode)
	}
}

// PutBlob uploads a blob with a known digest. size < 0 streams chunked.
// Idempotent: existing blobs are not re-uploaded.
func (c *Client) PutBlob(ctx context.Context, repo, digest string, content io.Reader, size int64) error {
	if _, err := c.HeadBlob(ctx, repo, digest); err == nil {
		return nil
	}
	location, err := c.startUpload(ctx, repo)
	if err != nil {
		return err
	}
	// Monolithic PUT with ?digest=.
	u, err := appendDigestParam(location, digest)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, content)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if size >= 0 {
		req.ContentLength = size
	}
	resp, err := c.do(req, "push:"+repo)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("ociclient: PUT blob %s: status %d: %s", digest, resp.StatusCode, body)
	}
	return nil
}

// BlobWriter streams an upload whose digest is only known at the end —
// content like tar layers, where the layer bytes (and so the digest) are
// produced while writing. Data is shipped in chunked PATCH requests so
// memory stays bounded regardless of layer size.
type BlobWriter struct {
	c         *Client
	ctx       context.Context
	repo      string
	location  string
	hasher    hash.Hash
	buf       bytes.Buffer
	offset    int64
	chunkSize int
}

// NewBlobWriter starts a chunked upload session.
func (c *Client) NewBlobWriter(ctx context.Context, repo string) (*BlobWriter, error) {
	location, err := c.startUpload(ctx, repo)
	if err != nil {
		return nil, err
	}
	return &BlobWriter{
		c:         c,
		ctx:       ctx,
		repo:      repo,
		location:  location,
		hasher:    sha256.New(),
		chunkSize: 8 << 20,
	}, nil
}

func (w *BlobWriter) Write(p []byte) (int, error) {
	w.hasher.Write(p)
	n, err := w.buf.Write(p)
	if err != nil {
		return n, err
	}
	if w.buf.Len() >= w.chunkSize {
		if err := w.flush(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// flush PATCHes the buffered chunk to the upload session.
func (w *BlobWriter) flush() error {
	if w.buf.Len() == 0 {
		return nil
	}
	chunk := w.buf.Bytes()
	req, err := http.NewRequestWithContext(w.ctx, http.MethodPatch, w.location, bytes.NewReader(chunk))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", w.offset, w.offset+int64(len(chunk))-1))
	req.ContentLength = int64(len(chunk))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(chunk)), nil }
	resp, err := w.c.do(req, "push:"+w.repo)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("ociclient: PATCH chunk at %d: status %d: %s", w.offset, resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if strings.HasPrefix(loc, "/") {
			loc = w.c.base + loc
		}
		w.location = loc
	}
	w.offset += int64(len(chunk))
	w.buf.Reset()
	return nil
}

// Commit finalizes the upload and returns the digest and total size.
func (w *BlobWriter) Commit() (string, int64, error) {
	digest := "sha256:" + hex.EncodeToString(w.hasher.Sum(nil))
	total := w.offset + int64(w.buf.Len())

	// The final PUT carries any remaining buffered bytes.
	u, err := appendDigestParam(w.location, digest)
	if err != nil {
		return "", 0, err
	}
	tail := w.buf.Bytes()
	req, err := http.NewRequestWithContext(w.ctx, http.MethodPut, u, bytes.NewReader(tail))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(tail))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(tail)), nil }
	resp, err := w.c.do(req, "push:"+w.repo)
	if err != nil {
		return "", 0, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return "", 0, fmt.Errorf("ociclient: committing blob: status %d: %s", resp.StatusCode, body)
	}
	return digest, total, nil
}

func (c *Client) startUpload(ctx context.Context, repo string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v2/%s/blobs/uploads/", c.base, repo), http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := c.do(req, "push:"+repo)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("ociclient: starting upload for %s: status %d: %s", repo, resp.StatusCode, body)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("ociclient: upload start returned no Location")
	}
	if strings.HasPrefix(location, "/") {
		location = c.base + location
	}
	return location, nil
}

func appendDigestParam(location, digest string) (string, error) {
	u, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("ociclient: invalid upload location %q", location)
	}
	q := u.Query()
	q.Set("digest", digest)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// PutManifest pushes a manifest under a tag (or digest reference) and
// returns its digest.
func (c *Client) PutManifest(ctx context.Context, repo, reference string, m *Manifest) (string, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/v2/%s/manifests/%s", c.base, repo, reference), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
	req.Header.Set("Content-Type", m.MediaType)
	resp, err := c.do(req, "push:"+repo)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ociclient: PUT manifest %s:%s: status %d: %s", repo, reference, resp.StatusCode, body)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		return d, nil
	}
	return SHA256Digest(payload), nil
}

// GetManifest fetches a manifest by tag or digest.
func (c *Client) GetManifest(ctx context.Context, repo, reference string) (*Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v2/%s/manifests/%s", c.base, repo, reference), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", MediaTypeOCIManifest)
	resp, err := c.do(req, "pull:"+repo)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("ociclient: GET manifest %s:%s: status %d", repo, reference, resp.StatusCode)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("ociclient: decoding manifest %s:%s: %w", repo, reference, err)
	}
	return &m, nil
}

// DeleteManifest removes a tag (registries treat tag delete as manifest
// dereference; content GC is the registry's concern).
func (c *Client) DeleteManifest(ctx context.Context, repo, reference string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/v2/%s/manifests/%s", c.base, repo, reference), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req, "push:"+repo)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("ociclient: DELETE manifest %s:%s: status %d", repo, reference, resp.StatusCode)
	}
}

// ListTags returns all tags in a repository; missing repositories yield an
// empty list.
func (c *Client) ListTags(ctx context.Context, repo string) ([]string, error) {
	var tags []string
	next := fmt.Sprintf("%s/v2/%s/tags/list?n=1000", c.base, repo)
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.do(req, "pull:"+repo)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return nil, nil
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("ociclient: tags list for %s: status %d", repo, resp.StatusCode)
		}
		var page struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&page)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("ociclient: decoding tags list: %w", err)
		}
		tags = append(tags, page.Tags...)
		next = parseNextLink(link, c.base)
	}
	return tags, nil
}

func parseNextLink(header, base string) string {
	if header == "" {
		return ""
	}
	start := strings.Index(header, "<")
	end := strings.Index(header, ">")
	if start < 0 || end <= start {
		return ""
	}
	link := header[start+1 : end]
	if strings.HasPrefix(link, "/") {
		return base + link
	}
	return link
}
