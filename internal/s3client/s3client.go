// Package s3client is a minimal S3 REST client covering exactly what
// Shpiel's bucket backend needs: object HEAD/GET (with ranges), PUT,
// DELETE, and ListObjectsV2 — authenticated with AWS Signature Version 4.
// It speaks to anything with an S3-compatible API: AWS S3, GCS in interop
// mode, MinIO, Ceph RGW, Cloudflare R2.
//
// Hand-rolled instead of aws-sdk-go-v2: the needed surface is five
// requests, and serving model weights wants ranged reads and exact control
// over payload hashing for streaming PUTs — small and stable, like the
// distribution API is for ociclient.
package s3client

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned for missing objects (and missing buckets).
var ErrNotFound = errors.New("s3client: not found")

// Options configure a client. Bucket is required; everything else has a
// usable default.
type Options struct {
	// Endpoint overrides the AWS endpoint for S3-compatible services
	// (scheme required). When set, requests are path-style
	// (<endpoint>/<bucket>/<key>); when empty, virtual-hosted AWS style
	// (https://<bucket>.s3.<region>.amazonaws.com/<key>).
	Endpoint string
	Bucket   string
	// Region is the SigV4 signing region. Defaults to us-east-1, which is
	// what S3-compatible services conventionally expect.
	Region      string
	Credentials Credentials
	// Provider supplies rotating credentials (IRSA/web identity); when
	// set it takes precedence over the static Credentials.
	Provider CredentialsProvider
}

// Client talks to one bucket.
type Client struct {
	scheme     string
	host       string
	pathPrefix string // "" (virtual-hosted) or "/<bucket>" (path-style)
	region     string
	creds      CredentialsProvider
	http       *http.Client
	now        func() time.Time
}

// New creates a client for one bucket.
func New(opts Options) (*Client, error) {
	if opts.Bucket == "" {
		return nil, errors.New("s3client: bucket is required")
	}
	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}
	creds := opts.Provider
	if creds == nil {
		creds = StaticCredentials(opts.Credentials)
	}
	c := &Client{
		region: region,
		creds:  creds,
		// A private transport: sharing http.DefaultTransport means anyone
		// calling CloseIdleConnections on it (httptest servers do on
		// Close) can break this client's in-flight requests.
		http: &http.Client{Transport: cloneDefaultTransport()},
		now:  time.Now,
	}
	if opts.Endpoint == "" {
		c.scheme = "https"
		c.host = opts.Bucket + ".s3." + region + ".amazonaws.com"
		return c, nil
	}
	u, err := url.Parse(opts.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("s3client: invalid endpoint %q", opts.Endpoint)
	}
	c.scheme = u.Scheme
	c.host = u.Host
	c.pathPrefix = strings.TrimRight(u.Path, "/") + "/" + opts.Bucket
	return c, nil
}

func cloneDefaultTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return http.DefaultTransport
}

// newRequest builds a request for key ("" targets the bucket). The caller
// adds headers and then sends it through do, which signs.
func (c *Client) newRequest(ctx context.Context, method, key string, query url.Values, body io.Reader) (*http.Request, error) {
	path := c.pathPrefix + "/" + key
	u := &url.URL{
		Scheme:   c.scheme,
		Host:     c.host,
		Path:     path,
		RawPath:  uriEncode(path, false),
		RawQuery: canonicalQuery(query),
	}
	return http.NewRequestWithContext(ctx, method, u.String(), body)
}

// do resolves credentials, signs (unless anonymous), and sends a request.
func (c *Client) do(req *http.Request, payloadHash string) (*http.Response, error) {
	creds, err := c.creds.Credentials(req.Context())
	if err != nil {
		return nil, fmt.Errorf("s3client: resolving credentials: %w", err)
	}
	if !creds.IsZero() {
		sign(req, creds, c.region, payloadHash, c.now())
	}
	return c.http.Do(req)
}

// Head reports an object's size, or ErrNotFound.
func (c *Client) Head(ctx context.Context, key string) (int64, error) {
	req, err := c.newRequest(ctx, http.MethodHead, key, nil, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.do(req, EmptyPayloadSHA256)
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
		return 0, fmt.Errorf("s3client: HEAD %s: status %d", key, resp.StatusCode)
	}
}

// Get streams an object from offset (0 for the whole object). Servers
// without usable Range support get the prefix discarded client-side, so
// callers can rely on offset semantics either way.
func (c *Client) Get(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	req, err := c.newRequest(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.do(req, EmptyPayloadSHA256)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return resp.Body, nil
	case http.StatusOK:
		if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("s3client: skipping to offset %d: %w", offset, err)
		}
		return resp.Body, nil
	default:
		return nil, drainError(resp, "GET", key)
	}
}

// Put uploads an object. Size must be known (S3 has no unsized monolithic
// PUT); payloadSHA256 is the hex sha256 of the content, signed into the
// request so the service verifies integrity end to end.
func (c *Client) Put(ctx context.Context, key string, content io.Reader, size int64, payloadSHA256 string) error {
	req, err := c.newRequest(ctx, http.MethodPut, key, nil, content)
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := c.do(req, payloadSHA256)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return drainError(resp, "PUT", key)
	}
	resp.Body.Close()
	return nil
}

// Delete removes an object. Deleting a missing object succeeds (S3
// semantics).
func (c *Client) Delete(ctx context.Context, key string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req, EmptyPayloadSHA256)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return drainError(resp, "DELETE", key)
	}
	resp.Body.Close()
	return nil
}

// List returns one page of keys under prefix in lexicographic order
// (ListObjectsV2). A non-empty continuation token resumes a previous page;
// a non-empty next token means more pages exist. maxKeys <= 0 uses the
// service default (1000).
func (c *Client) List(ctx context.Context, prefix, continuationToken string, maxKeys int) (keys []string, next string, err error) {
	query := url.Values{"list-type": {"2"}}
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if continuationToken != "" {
		query.Set("continuation-token", continuationToken)
	}
	if maxKeys > 0 {
		query.Set("max-keys", strconv.Itoa(maxKeys))
	}
	req, err := c.newRequest(ctx, http.MethodGet, "", query, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.do(req, EmptyPayloadSHA256)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", readError(resp, "LIST", prefix)
	}
	var page struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
		IsTruncated           bool   `xml:"IsTruncated"`
		NextContinuationToken string `xml:"NextContinuationToken"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("s3client: decoding list response: %w", err)
	}
	for _, obj := range page.Contents {
		keys = append(keys, obj.Key)
	}
	if page.IsTruncated {
		next = page.NextContinuationToken
		if next == "" {
			return nil, "", errors.New("s3client: truncated list without continuation token")
		}
	}
	return keys, next, nil
}

// Ping verifies the bucket is reachable and the credentials can list it.
func (c *Client) Ping(ctx context.Context) error {
	if _, _, err := c.List(ctx, "", "", 1); err != nil {
		return fmt.Errorf("s3client: bucket not reachable: %w", err)
	}
	return nil
}

// xmlError is the S3 error response body.
type xmlError struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// drainError consumes the response body and maps it onto an error;
// closing is included.
func drainError(resp *http.Response, op, key string) error {
	defer resp.Body.Close()
	return readError(resp, op, key)
}

// readError maps an S3 error response onto ErrNotFound or a descriptive
// error. The caller owns the body.
func readError(resp *http.Response, op, key string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var e xmlError
	_ = xml.Unmarshal(body, &e)
	switch {
	case e.Code == "NoSuchKey" || e.Code == "NoSuchBucket" || (e.Code == "" && resp.StatusCode == http.StatusNotFound):
		return ErrNotFound
	case e.Code != "":
		return fmt.Errorf("s3client: %s %s: %s: %s (status %d)", op, key, e.Code, e.Message, resp.StatusCode)
	default:
		return fmt.Errorf("s3client: %s %s: status %d", op, key, resp.StatusCode)
	}
}
