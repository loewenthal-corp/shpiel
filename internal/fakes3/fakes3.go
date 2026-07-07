// Package fakes3 is an in-process S3 server for tests that is strict the
// way AWS is strict about request authentication.
//
// When credentials are configured, every request must carry a valid AWS
// Signature Version 4: the signature is recomputed from the wire request
// and compared, the payload hash is checked against the actual body, and
// the credential scope (region, service, date) is validated. The verifier
// is written from the SigV4 specification independently of
// internal/s3client's signer — sharing that code would let a
// canonicalization bug pass its own reflection — so tests through this
// fake are a genuine two-implementation handshake, pinned to AWS's
// published vectors on the client side.
//
// The API subset is what s3client speaks: object GET (with ranges), HEAD,
// PUT, DELETE, and ListObjectsV2 with prefix + continuation-token
// pagination. Errors use the S3 XML shape. Continuation tokens are opaque
// base64 (deliberately: they exercise query-string encoding of '+', '/',
// and '=').
package fakes3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Server is an http.Handler for one bucket. Zero value is not usable;
// call New.
type Server struct {
	bucket    string
	accessKey string
	secretKey string

	mu      sync.Mutex
	objects map[string][]byte
}

// New creates an empty bucket. Empty accessKey disables signature
// verification (anonymous mode).
func New(bucket, accessKey, secretKey string) *Server {
	return &Server{
		bucket:    bucket,
		accessKey: accessKey,
		secretKey: secretKey,
		objects:   map[string][]byte{},
	}
}

// Seed stores an object directly, bypassing HTTP (test setup).
func (s *Server) Seed(key string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = content
}

// Keys returns all object keys, sorted (test inspection).
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Object returns a stored object's content (test inspection).
func (s *Server) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	return b, ok
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "IncompleteBody", "reading body")
		return
	}

	if s.accessKey != "" {
		if code, msg := s.verify(req, body); code != "" {
			writeErr(w, http.StatusForbidden, code, msg)
			return
		}
	}

	bucket, key, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/"), "/")
	if !ok {
		bucket, key = strings.TrimPrefix(req.URL.Path, "/"), ""
	}
	if bucket != s.bucket {
		writeErr(w, http.StatusNotFound, "NoSuchBucket", "no bucket "+bucket)
		return
	}

	switch {
	case key == "" && req.Method == http.MethodGet:
		s.list(w, req)
	case req.Method == http.MethodGet, req.Method == http.MethodHead:
		s.get(w, req, key)
	case req.Method == http.MethodPut:
		s.put(w, key, body)
	case req.Method == http.MethodDelete:
		s.delete(w, key)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed", req.Method)
	}
}

func (s *Server) get(w http.ResponseWriter, req *http.Request, key string) {
	s.mu.Lock()
	content, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "NoSuchKey", "no key "+key)
		return
	}
	from, to, ok := parseRange(req.Header.Get("Range"), int64(len(content)))
	if !ok {
		writeErr(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", req.Header.Get("Range"))
		return
	}
	status := http.StatusOK
	if req.Header.Get("Range") != "" {
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(content)))
	}
	w.Header().Set("Content-Length", strconv.Itoa(int(to-from+1)))
	w.WriteHeader(status)
	if req.Method != http.MethodHead {
		w.Write(content[from : to+1])
	}
}

func (s *Server) put(w http.ResponseWriter, key string, body []byte) {
	s.mu.Lock()
	s.objects[key] = body
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) delete(w http.ResponseWriter, key string) {
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	// S3 answers 204 whether or not the key existed.
	w.WriteHeader(http.StatusNoContent)
}

// list implements ListObjectsV2: prefix filter, lexicographic order,
// max-keys, opaque base64 continuation tokens.
func (s *Server) list(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	if q.Get("list-type") != "2" {
		writeErr(w, http.StatusBadRequest, "InvalidArgument", "only list-type=2 is supported")
		return
	}
	prefix := q.Get("prefix")
	maxKeys := 1000
	if mk := q.Get("max-keys"); mk != "" {
		n, err := strconv.Atoi(mk)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "InvalidArgument", "bad max-keys")
			return
		}
		maxKeys = n
	}
	startAfter := ""
	if tok := q.Get("continuation-token"); tok != "" {
		decoded, err := base64.StdEncoding.DecodeString(tok)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "InvalidArgument", "bad continuation-token")
			return
		}
		startAfter = string(decoded)
	}

	var matched []string
	s.mu.Lock()
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) && k > startAfter {
			matched = append(matched, k)
		}
	}
	s.mu.Unlock()
	sort.Strings(matched)

	truncated := len(matched) > maxKeys
	if truncated {
		matched = matched[:maxKeys]
	}

	type contents struct {
		Key string `xml:"Key"`
	}
	result := struct {
		XMLName               xml.Name   `xml:"ListBucketResult"`
		IsTruncated           bool       `xml:"IsTruncated"`
		Contents              []contents `xml:"Contents"`
		NextContinuationToken string     `xml:"NextContinuationToken,omitempty"`
	}{IsTruncated: truncated}
	for _, k := range matched {
		result.Contents = append(result.Contents, contents{Key: k})
	}
	if truncated {
		result.NextContinuationToken = base64.StdEncoding.EncodeToString([]byte(matched[len(matched)-1]))
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

// parseRange handles "bytes=N-" and "bytes=N-M" (all s3client sends).
// Empty means the whole object.
func parseRange(header string, size int64) (from, to int64, ok bool) {
	if header == "" {
		return 0, size - 1, true
	}
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found {
		return 0, 0, false
	}
	fromStr, toStr, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	from, err := strconv.ParseInt(fromStr, 10, 64)
	if err != nil || from < 0 || from >= size {
		return 0, 0, false
	}
	to = size - 1
	if toStr != "" {
		to, err = strconv.ParseInt(toStr, 10, 64)
		if err != nil || to < from {
			return 0, 0, false
		}
		if to > size-1 {
			to = size - 1
		}
	}
	return from, to, true
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	type xmlError struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}
	_ = xml.NewEncoder(w).Encode(xmlError{Code: code, Message: message})
}

// --- SigV4 verification ---

// verify recomputes the request's SigV4 signature and returns an error
// code + message on any mismatch ("" means authorized).
func (s *Server) verify(req *http.Request, body []byte) (code, msg string) {
	auth := req.Header.Get("Authorization")
	if auth == "" {
		return "AccessDenied", "anonymous request to a credentialed bucket"
	}
	rest, found := strings.CutPrefix(auth, "AWS4-HMAC-SHA256 ")
	if !found {
		return "AccessDenied", "unsupported authorization scheme"
	}
	fields := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		fields[k] = v
	}
	credential, signedHeaders, signature := fields["Credential"], fields["SignedHeaders"], fields["Signature"]
	if credential == "" || signedHeaders == "" || signature == "" {
		return "AuthorizationHeaderMalformed", "missing Credential, SignedHeaders, or Signature"
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 {
		return "AuthorizationHeaderMalformed", "credential is not accessKey/date/region/service/aws4_request"
	}
	accessKey, scopeDate, region, service, terminator := credParts[0], credParts[1], credParts[2], credParts[3], credParts[4]
	if accessKey != s.accessKey {
		return "InvalidAccessKeyId", "unknown access key " + accessKey
	}
	if service != "s3" || terminator != "aws4_request" {
		return "SignatureDoesNotMatch", "credential scope must end in /s3/aws4_request"
	}

	amzDate := req.Header.Get("x-amz-date")
	if !strings.HasPrefix(amzDate, scopeDate) {
		return "SignatureDoesNotMatch", "x-amz-date does not match credential scope date"
	}

	payloadHash := req.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		return "InvalidRequest", "missing x-amz-content-sha256"
	}
	if payloadHash != "UNSIGNED-PAYLOAD" {
		sum := sha256.Sum256(body)
		if payloadHash != hex.EncodeToString(sum[:]) {
			return "XAmzContentSHA256Mismatch", "payload hash does not match body"
		}
	}

	// Canonical request, rebuilt from the wire.
	names := strings.Split(signedHeaders, ";")
	var canonHeaders strings.Builder
	for _, name := range names {
		var value string
		if name == "host" {
			value = req.Host
		} else {
			value = strings.Join(req.Header.Values(name), ",")
		}
		canonHeaders.WriteString(name + ":" + strings.Join(strings.Fields(value), " ") + "\n")
	}
	canonicalRequest := strings.Join([]string{
		req.Method,
		encodePath(req.URL.Path),
		encodeQuery(req.URL.Query()),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	crSum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		strings.Join([]string{scopeDate, region, "s3", "aws4_request"}, "/"),
		hex.EncodeToString(crSum[:]),
	}, "\n")

	key := []byte("AWS4" + s.secretKey)
	for _, part := range []string{scopeDate, region, "s3", "aws4_request"} {
		key = hmacSum(key, part)
	}
	want := hex.EncodeToString(hmacSum(key, stringToSign))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return "SignatureDoesNotMatch", "signature mismatch"
	}
	return "", ""
}

func hmacSum(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// encodePath percent-encodes a decoded URL path per SigV4 (slash kept,
// unreserved kept, the rest %XX uppercase).
func encodePath(path string) string {
	var b strings.Builder
	for i := range len(path) {
		c := path[i]
		if c == '/' || isUnreserved(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// encodeQuery renders the canonical query string: sorted keys, strict
// RFC3986 encoding.
func encodeQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, encodeComponent(k)+"="+encodeComponent(v))
		}
	}
	return strings.Join(parts, "&")
}

func encodeComponent(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if isUnreserved(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}
