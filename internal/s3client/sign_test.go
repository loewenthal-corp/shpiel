package s3client

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The reference vectors below are AWS's published SigV4 examples
// ("Examples: Signature Calculations" in the S3 API reference,
// sig-v4-header-based-auth.html): example credentials, a fixed timestamp,
// bucket "examplebucket" in us-east-1, virtual-hosted style. They pin the
// signer against AWS's own oracle, not this package's math.
var (
	awsExampleCreds = Credentials{ // #nosec G101 -- AWS's published example credentials, not real ones
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	awsExampleTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
)

func TestSignAWSExampleVectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		method      string
		url         string
		headers     map[string]string
		payloadHash string
		wantAuth    string
	}{
		{
			name:        "GetObject",
			method:      http.MethodGet,
			url:         "https://examplebucket.s3.amazonaws.com/test.txt",
			headers:     map[string]string{"Range": "bytes=0-9"},
			payloadHash: EmptyPayloadSHA256,
			wantAuth: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
				"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date," +
				"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name:   "PutObject",
			method: http.MethodPut,
			url:    "https://examplebucket.s3.amazonaws.com/test$file.text",
			headers: map[string]string{
				"Date":                "Fri, 24 May 2013 00:00:00 GMT",
				"x-amz-storage-class": "REDUCED_REDUNDANCY",
			},
			// sha256("Welcome to Amazon S3.")
			payloadHash: "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			wantAuth: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
				"SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class," +
				"Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name:        "GetBucketLifecycle",
			method:      http.MethodGet,
			url:         "https://examplebucket.s3.amazonaws.com/?lifecycle",
			payloadHash: EmptyPayloadSHA256,
			wantAuth: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
				"SignedHeaders=host;x-amz-content-sha256;x-amz-date," +
				"Signature=fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name:        "ListObjects",
			method:      http.MethodGet,
			url:         "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J",
			payloadHash: EmptyPayloadSHA256,
			wantAuth: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
				"SignedHeaders=host;x-amz-content-sha256;x-amz-date," +
				"Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(tt.method, tt.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			sign(req, awsExampleCreds, "us-east-1", tt.payloadHash, awsExampleTime)
			if got := req.Header.Get("Authorization"); got != tt.wantAuth {
				t.Errorf("Authorization =\n  %s\nwant\n  %s", got, tt.wantAuth)
			}
			if got := req.Header.Get("x-amz-date"); got != "20130524T000000Z" {
				t.Errorf("x-amz-date = %q", got)
			}
			if got := req.Header.Get("x-amz-content-sha256"); got != tt.payloadHash {
				t.Errorf("x-amz-content-sha256 = %q, want %q", got, tt.payloadHash)
			}
		})
	}
}

func TestSignSessionToken(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	creds := awsExampleCreds
	creds.SessionToken = "the-token"
	sign(req, creds, "us-east-1", EmptyPayloadSHA256, awsExampleTime)
	if got := req.Header.Get("x-amz-security-token"); got != "the-token" {
		t.Errorf("x-amz-security-token = %q", got)
	}
	auth := req.Header.Get("Authorization")
	if want := "SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token,"; !strings.Contains(auth, want) {
		t.Errorf("Authorization %q does not sign the security token", auth)
	}
}

func TestURIEncode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"models/org/name/blobs/abc", false, "models/org/name/blobs/abc"},
		{"a b", false, "a%20b"},
		{"a+b", false, "a%2Bb"},
		{"test$file.text", false, "test%24file.text"},
		{"a/b", true, "a%2Fb"},
		{"A-Za-z0-9-._~", true, "A-Za-z0-9-._~"},
		{"café", false, "caf%C3%A9"},
		{"", false, ""},
	}
	for _, tt := range tests {
		if got := uriEncode(tt.in, tt.encodeSlash); got != tt.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", tt.in, tt.encodeSlash, got, tt.want)
		}
	}
}

func TestCanonicalQuery(t *testing.T) {
	t.Parallel()
	q := url.Values{
		"prefix":             {"models/a b"},
		"list-type":          {"2"},
		"continuation-token": {"x+y/z="},
	}
	want := "continuation-token=x%2By%2Fz%3D&list-type=2&prefix=models%2Fa%20b"
	if got := canonicalQuery(q); got != want {
		t.Errorf("canonicalQuery = %q, want %q", got, want)
	}
	if got := canonicalQuery(url.Values{}); got != "" {
		t.Errorf("empty canonicalQuery = %q, want \"\"", got)
	}
	// Multiple values for one key are sorted by value.
	multi := url.Values{"k": {"b", "a"}}
	if got := canonicalQuery(multi); got != "k=a&k=b" {
		t.Errorf("multi-value canonicalQuery = %q, want k=a&k=b", got)
	}
}

func TestCanonicalizeHeadersCollapsesWhitespace(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequest(http.MethodGet, "https://h.example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Custom", "  a   b  ")
	names, canonical := canonicalizeHeaders(req)
	if len(names) != 2 || names[0] != "host" || names[1] != "x-custom" {
		t.Fatalf("names = %v", names)
	}
	want := "host:h.example.com\nx-custom:a b\n"
	if canonical != want {
		t.Errorf("canonical = %q, want %q", canonical, want)
	}
}
