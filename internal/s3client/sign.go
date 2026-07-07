package s3client

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// EmptyPayloadSHA256 is sha256("") — the payload hash of bodyless requests.
const EmptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Credentials are static AWS-style credentials. Zero value means anonymous
// (requests go out unsigned).
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	// SessionToken is set for temporary credentials (STS, IRSA-minted).
	SessionToken string
}

// IsZero reports whether no credentials are configured.
func (c Credentials) IsZero() bool { return c.AccessKeyID == "" && c.SecretAccessKey == "" }

const (
	signAlgorithm = "AWS4-HMAC-SHA256"
	signService   = "s3"
	timeFormat    = "20060102T150405Z"
	dateFormat    = "20060102"
)

// sign adds AWS Signature Version 4 headers to req: x-amz-date,
// x-amz-content-sha256 (payloadHash), x-amz-security-token when a session
// token is present, and Authorization. Every header already on the request
// is signed, so callers must set headers before signing and the transport
// must send them verbatim (Go's http does).
func sign(req *http.Request, creds Credentials, region, payloadHash string, t time.Time) {
	t = t.UTC()
	req.Header.Set("x-amz-date", t.Format(timeFormat))
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("x-amz-security-token", creds.SessionToken)
	}

	names, canonicalHeaders := canonicalizeHeaders(req)
	signedHeaders := strings.Join(names, ";")
	canonicalRequest := strings.Join([]string{
		req.Method,
		uriEncode(req.URL.Path, false),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{t.Format(dateFormat), region, signService, "aws4_request"}, "/")
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		signAlgorithm,
		t.Format(timeFormat),
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretAccessKey, t, region), []byte(stringToSign)))
	req.Header.Set("Authorization", signAlgorithm+
		" Credential="+creds.AccessKeyID+"/"+scope+
		",SignedHeaders="+signedHeaders+
		",Signature="+signature)
}

// signingKey derives the SigV4 signing key via the HMAC chain.
func signingKey(secret string, t time.Time, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(t.UTC().Format(dateFormat)))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(signService))
	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// canonicalizeHeaders renders every request header (plus host) in SigV4
// canonical form: lowercase names sorted, values trimmed with internal
// whitespace runs collapsed, one "name:value\n" line each.
func canonicalizeHeaders(req *http.Request) (names []string, canonical string) {
	// http.NewRequest populates req.Host from the URL; the cmp.Or covers
	// hand-constructed requests.
	values := map[string]string{"host": cmp.Or(req.Host, req.URL.Host)}
	for name, vals := range req.Header {
		trimmed := make([]string, len(vals))
		for i, v := range vals {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		values[strings.ToLower(name)] = strings.Join(trimmed, ",")
	}
	names = make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(values[name])
		b.WriteByte('\n')
	}
	return names, b.String()
}

// canonicalQuery renders query parameters in SigV4 canonical form: keys
// sorted, keys and values strictly RFC3986-encoded, bare keys rendered as
// "key=".
func canonicalQuery(q url.Values) string {
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
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode percent-encodes s per the SigV4 rules: unreserved characters
// (A-Za-z0-9, '-', '.', '_', '~') stay literal, everything else becomes
// uppercase %XX per byte. Slashes stay literal in paths (encodeSlash
// false) and are encoded in query strings (encodeSlash true).
func uriEncode(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~',
			c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
		}
	}
	return b.String()
}
