package ociclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/fakeregistry"
)

// requestLog records requests hitting a scripted test server.
type requestLog struct {
	mu   sync.Mutex
	list []string
}

func (l *requestLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, r.Method+" "+r.URL.Path)
}

func (l *requestLog) count(method string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.list {
		if strings.HasPrefix(e, method+" ") {
			n++
		}
	}
	return n
}

func mustClient(t *testing.T, url, user, pass string) *Client {
	t.Helper()
	c, err := New(url, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRejectsBadURL(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"", "zot.internal:5000", "http://", "://x"} {
		if _, err := New(u, "", ""); err == nil {
			t.Errorf("New(%q) accepted", u)
		}
	}
}

// TestGetBlobOffsets pins the Range negotiation: no Range header at offset
// zero, honored ranges pass through, and registries that ignore ranges or
// 416 open-ended ones still yield correct offset semantics client-side.
func TestGetBlobOffsets(t *testing.T) {
	t.Parallel()
	content := testPayload(1000)
	for _, mode := range []string{"honor", "ignore", "reject416"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var sawRange []string
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rng := r.Header.Get("Range")
				mu.Lock()
				sawRange = append(sawRange, rng)
				mu.Unlock()
				if rng == "" || mode == "ignore" {
					_, _ = w.Write(content)
					return
				}
				if mode == "reject416" {
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
				var off int64
				if _, err := fmt.Sscanf(rng, "bytes=%d-", &off); err != nil {
					t.Errorf("unparseable range %q", rng)
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(content[off:])
			}))
			t.Cleanup(srv.Close)
			c := mustClient(t, srv.URL, "", "")
			ctx := context.Background()

			// Offset zero must not send a Range header at all.
			rc, err := c.GetBlob(ctx, "org/repo", "sha256:irrelevant", 0)
			if err != nil {
				t.Fatalf("GetBlob(0): %v", err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(got, content) {
				t.Fatalf("GetBlob(0) returned %d bytes, want %d", len(got), len(content))
			}
			mu.Lock()
			first := sawRange[0]
			mu.Unlock()
			if first != "" {
				t.Fatalf("offset 0 sent Range %q", first)
			}

			rc, err = c.GetBlob(ctx, "org/repo", "sha256:irrelevant", 100)
			if err != nil {
				t.Fatalf("GetBlob(100): %v", err)
			}
			got, _ = io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(got, content[100:]) {
				t.Fatalf("GetBlob(100) = %d bytes (first %x), want suffix of %d", len(got), got[:4], len(content)-100)
			}
		})
	}
}

func TestGetBlobErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := mustClient(t, srv.URL, "", "")
	if _, err := c.GetBlob(context.Background(), "r", "sha256:missing", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing blob: err = %v, want ErrNotFound", err)
	}
	if _, err := c.GetBlob(context.Background(), "r", "sha256:boom", 0); err == nil {
		t.Fatal("500 response accepted")
	}
	if _, err := c.HeadBlob(context.Background(), "r", "sha256:missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing HEAD: err = %v, want ErrNotFound", err)
	}
	if _, err := c.HeadBlob(context.Background(), "r", "sha256:boom"); err == nil {
		t.Fatal("HEAD 500 accepted")
	}
}

// plainReader hides the underlying bytes.Reader type so net/http cannot
// infer ContentLength on its own — the client must set it.
type plainReader struct{ r io.Reader }

func (p plainReader) Read(b []byte) (int, error) { return p.r.Read(b) }

// TestPutBlobKnownSizeProtocol asserts the monolithic upload shape: one
// POST, one PUT carrying the digest param and an explicit Content-Length.
func TestPutBlobKnownSizeProtocol(t *testing.T) {
	t.Parallel()
	content := testPayload(600)
	digest := SHA256Digest(content)
	var log requestLog
	var gotLen int64
	var gotDigest string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPost:
			w.Header().Set("Location", "/v2/org/repo/blobs/uploads/session-1")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPut:
			gotLen = r.ContentLength
			gotDigest = r.URL.Query().Get("digest")
			body, _ := io.ReadAll(r.Body)
			if !bytes.Equal(body, content) {
				t.Error("PUT body does not match content")
			}
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	t.Cleanup(srv.Close)
	c := mustClient(t, srv.URL, "", "")
	if err := c.PutBlob(context.Background(), "org/repo", digest, plainReader{bytes.NewReader(content)}, int64(len(content))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if gotLen != int64(len(content)) {
		t.Fatalf("PUT Content-Length = %d, want %d", gotLen, len(content))
	}
	if gotDigest != digest {
		t.Fatalf("PUT digest param = %q, want %q", gotDigest, digest)
	}
	if log.count(http.MethodPut) != 1 || log.count(http.MethodPatch) != 0 {
		t.Fatalf("requests = %v", log.list)
	}
}

func TestPutBlobSkipsExisting(t *testing.T) {
	t.Parallel()
	var log requestLog
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "3")
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
	}))
	t.Cleanup(srv.Close)
	c := mustClient(t, srv.URL, "", "")
	if err := c.PutBlob(context.Background(), "org/repo", "sha256:abc", strings.NewReader("xyz"), 3); err != nil {
		t.Fatalf("PutBlob existing: %v", err)
	}
	if len(log.list) != 1 {
		t.Fatalf("requests = %v, want only the HEAD", log.list)
	}
}

// TestPutBlobStatusTolerance: 204 on the closing PUT is success; a 401
// without a bearer challenge is a hard error, not silence.
func TestPutBlobStatusTolerance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		putStatus int
		wantErr   bool
	}{
		{"created", http.StatusCreated, false},
		{"no content", http.StatusNoContent, false},
		{"unauthorized", http.StatusUnauthorized, true},
		{"conflict", http.StatusConflict, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodHead:
					w.WriteHeader(http.StatusNotFound)
				case http.MethodPost:
					w.Header().Set("Location", "/v2/r/blobs/uploads/s1")
					w.WriteHeader(http.StatusAccepted)
				default:
					_, _ = io.Copy(io.Discard, r.Body)
					w.WriteHeader(tc.putStatus)
				}
			}))
			t.Cleanup(srv.Close)
			c := mustClient(t, srv.URL, "", "")
			err := c.PutBlob(context.Background(), "r", "sha256:d", strings.NewReader("data"), 4)
			if tc.wantErr != (err != nil) {
				t.Fatalf("PutBlob err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// blobWriterServer scripts the chunked upload session, asserting protocol
// invariants and recording traffic.
func blobWriterServer(t *testing.T, patchStatus, putStatus int, sendLocation bool, log *requestLog) *httptest.Server {
	t.Helper()
	var received bytes.Buffer
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", "/v2/r/blobs/uploads/s1")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPatch:
			if r.Header.Get("Content-Range") == "" {
				t.Error("PATCH without Content-Range")
			}
			mu.Lock()
			_, _ = io.Copy(&received, r.Body)
			mu.Unlock()
			if sendLocation {
				w.Header().Set("Location", "/v2/r/blobs/uploads/s1")
			}
			w.WriteHeader(patchStatus)
		case http.MethodPut:
			mu.Lock()
			_, _ = io.Copy(&received, r.Body)
			mu.Unlock()
			if r.URL.Query().Get("digest") == "" {
				t.Error("closing PUT without digest param")
			}
			w.WriteHeader(putStatus)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
}

// TestBlobWriterFlushBoundary: a write of exactly chunkSize bytes PATCHes
// immediately — not on the next write, not once per byte.
func TestBlobWriterFlushBoundary(t *testing.T) {
	t.Parallel()
	var log requestLog
	srv := blobWriterServer(t, http.StatusAccepted, http.StatusCreated, true, &log)
	t.Cleanup(srv.Close)
	c := mustClient(t, srv.URL, "", "")
	w, err := c.NewBlobWriter(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	w.chunkSize = 100

	payload := testPayload(150)
	if _, err := w.Write(payload[:100]); err != nil {
		t.Fatal(err)
	}
	if got := log.count(http.MethodPatch); got != 1 {
		t.Fatalf("PATCHes after exact-chunk write = %d, want 1", got)
	}
	if _, err := w.Write(payload[100:]); err != nil {
		t.Fatal(err)
	}
	if got := log.count(http.MethodPatch); got != 1 {
		t.Fatalf("PATCHes after sub-chunk write = %d, want still 1", got)
	}
	digest, total, err := w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if total != 150 || digest != SHA256Digest(payload) {
		t.Fatalf("Commit = %s, %d", digest, total)
	}
	// Tail goes out as a second PATCH; the closing PUT is bodyless.
	if got := log.count(http.MethodPatch); got != 2 {
		t.Fatalf("PATCHes after Commit = %d, want 2", got)
	}
}

// TestBlobWriterStatusAndLocationTolerance: 204 responses and missing
// Location headers on PATCH are within spec tolerance.
func TestBlobWriterStatusAndLocationTolerance(t *testing.T) {
	t.Parallel()
	var log requestLog
	srv := blobWriterServer(t, http.StatusNoContent, http.StatusNoContent, false, &log)
	t.Cleanup(srv.Close)
	c := mustClient(t, srv.URL, "", "")
	w, err := c.NewBlobWriter(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	w.chunkSize = 64
	payload := testPayload(200)
	// A full chunk then a tail: both PATCHes must go to the same session
	// URL, since the server never re-sends Location.
	if _, err := w.Write(payload[:150]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload[150:]); err != nil {
		t.Fatal(err)
	}
	digest, total, err := w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if total != 200 || digest != SHA256Digest(payload) {
		t.Fatalf("Commit = %s, %d", digest, total)
	}
	if got := log.count(http.MethodPatch); got != 2 {
		t.Fatalf("PATCH count = %d, want 2", got)
	}
}

func TestBlobWriterPatchFailureSurfacesInWrite(t *testing.T) {
	t.Parallel()
	var log requestLog
	srv := blobWriterServer(t, http.StatusInternalServerError, http.StatusCreated, true, &log)
	t.Cleanup(srv.Close)
	c := mustClient(t, srv.URL, "", "")
	w, err := c.NewBlobWriter(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	w.chunkSize = 10
	if _, err := w.Write(testPayload(32)); err == nil {
		t.Fatal("Write swallowed a failed PATCH")
	}
}

// TestBearerAuthFlow covers the docker v2 token dance: 401 challenge,
// token fetch with basic creds, retry, and token reuse on later calls.
func TestBearerAuthFlow(t *testing.T) {
	t.Parallel()
	for _, tokenField := range []string{"token", "access_token"} {
		t.Run(tokenField, func(t *testing.T) {
			t.Parallel()
			var challenges, tokenCalls int
			var mu sync.Mutex
			mux := http.NewServeMux()
			var srv *httptest.Server
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				tokenCalls++
				mu.Unlock()
				if u, p, ok := r.BasicAuth(); !ok || u != "user" || p != "pass" {
					t.Errorf("token endpoint credentials = %q/%q", u, p)
				}
				if r.URL.Query().Get("service") != "reg" || r.URL.Query().Get("scope") == "" {
					t.Errorf("token endpoint query = %s", r.URL.RawQuery)
				}
				_ = json.NewEncoder(w).Encode(map[string]string{tokenField: "tok123"})
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer tok123" {
					mu.Lock()
					challenges++
					mu.Unlock()
					w.Header().Set("WWW-Authenticate",
						fmt.Sprintf(`Bearer realm="%s/token",service="reg",scope="repository:r:pull"`, srv.URL))
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Length", "5")
				w.WriteHeader(http.StatusOK)
			})
			srv = httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			c := mustClient(t, srv.URL, "user", "pass")
			size, err := c.HeadBlob(context.Background(), "r", "sha256:x")
			if err != nil {
				t.Fatalf("HeadBlob through challenge: %v", err)
			}
			if size != 5 {
				t.Fatalf("size = %d, want 5", size)
			}
			// Second call reuses the cached token: no new challenge.
			if _, err := c.HeadBlob(context.Background(), "r", "sha256:y"); err != nil {
				t.Fatalf("HeadBlob with cached token: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if challenges != 1 || tokenCalls != 1 {
				t.Fatalf("challenges = %d, tokenCalls = %d, want 1 and 1", challenges, tokenCalls)
			}
		})
	}
}

// TestBearerAuthAnonymous: a client without credentials completes the
// token dance without inventing basic auth.
func TestBearerAuthAnonymous(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			t.Error("anonymous client sent basic auth to the token endpoint")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "anon-tok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer anon-tok" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token"`, srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusOK)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if _, err := mustClient(t, srv.URL, "", "").HeadBlob(context.Background(), "r", "sha256:x"); err != nil {
		t.Fatalf("anonymous bearer flow: %v", err)
	}
}

// TestPutManifestResponses: a 200 counts as stored, and the registry's
// Docker-Content-Digest header outranks the locally computed digest.
func TestPutManifestResponses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		status       int
		digestHeader string
		wantHeader   bool
	}{
		{"created with digest header", http.StatusCreated, "sha256:from-the-registry", true},
		{"ok without digest header", http.StatusOK, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.digestHeader != "" {
					w.Header().Set("Docker-Content-Digest", tc.digestHeader)
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			m := &Manifest{SchemaVersion: 2, MediaType: MediaTypeOCIManifest}
			digest, err := mustClient(t, srv.URL, "", "").PutManifest(context.Background(), "r", "tag", m)
			if err != nil {
				t.Fatalf("PutManifest: %v", err)
			}
			payload, _ := json.Marshal(m)
			want := SHA256Digest(payload)
			if tc.wantHeader {
				want = tc.digestHeader
			}
			if digest != want {
				t.Fatalf("digest = %q, want %q", digest, want)
			}
		})
	}
}

func TestBearerAuthFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		challenge string
		tokenCode int
	}{
		{"basic 401 is final", "", 0},
		{"challenge without realm", `Bearer service="reg"`, 0},
		{"token endpoint failure", "REALM", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			var srv *httptest.Server
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.tokenCode)
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				ch := tc.challenge
				if ch == "REALM" {
					ch = fmt.Sprintf(`Bearer realm="%s/token"`, srv.URL)
				}
				if ch != "" {
					w.Header().Set("WWW-Authenticate", ch)
				}
				w.WriteHeader(http.StatusUnauthorized)
			})
			srv = httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			c := mustClient(t, srv.URL, "", "")
			if _, err := c.HeadBlob(context.Background(), "r", "sha256:x"); err == nil {
				t.Fatal("auth failure not surfaced")
			}
		})
	}
}

func TestPing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(fakeregistry.New())
	t.Cleanup(srv.Close)
	if err := mustClient(t, srv.URL, "", "").Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(bad.Close)
	if err := mustClient(t, bad.URL, "", "").Ping(context.Background()); err == nil {
		t.Fatal("Ping accepted 503")
	}
}

func TestManifestLifecycle(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(fakeregistry.New())
	t.Cleanup(srv.Close)
	c := mustClient(t, srv.URL, "", "")
	ctx := context.Background()

	m := &Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config:        Descriptor{MediaType: MediaTypeOCIConfig, Digest: "sha256:cfg", Size: 2},
		Layers:        []Descriptor{{MediaType: MediaTypeOCILayerTar, Digest: "sha256:l1", Size: 10}},
		Annotations:   map[string]string{"org.example": "v"},
	}
	digest, err := c.PutManifest(ctx, "org/repo", "main", m)
	if err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	payload, _ := json.Marshal(m)
	if digest != SHA256Digest(payload) {
		t.Fatalf("digest = %s, want %s", digest, SHA256Digest(payload))
	}

	back, err := c.GetManifest(ctx, "org/repo", "main")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if back.Config.Digest != "sha256:cfg" || len(back.Layers) != 1 || back.Layers[0].Digest != "sha256:l1" {
		t.Fatalf("manifest roundtrip = %+v", back)
	}

	if _, err := c.GetManifest(ctx, "org/repo", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing manifest err = %v, want ErrNotFound", err)
	}

	if _, err := c.PutManifest(ctx, "org/repo", "v2", m); err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTags(ctx, "org/repo")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 2 || tags[0] != "main" || tags[1] != "v2" {
		t.Fatalf("tags = %v", tags)
	}
	// Missing repositories are an empty list, not an error.
	tags, err = c.ListTags(ctx, "org/void")
	if err != nil || tags != nil {
		t.Fatalf("missing repo tags = %v, %v", tags, err)
	}

	if err := c.DeleteManifest(ctx, "org/repo", "v2"); err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}
	if _, err := c.GetManifest(ctx, "org/repo", "v2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted manifest err = %v, want ErrNotFound", err)
	}
	if err := c.DeleteManifest(ctx, "org/repo", "v2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete err = %v, want ErrNotFound", err)
	}
}

// TestListTagsPagination follows both relative and absolute Link headers.
func TestListTagsPagination(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last := r.URL.Query().Get("last")
		w.Header().Set("Content-Type", "application/json")
		switch last {
		case "":
			w.Header().Set("Link", `</v2/org/repo/tags/list?last=a&n=1000>; rel="next"`)
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"a"}})
		case "a":
			w.Header().Set("Link", fmt.Sprintf(`<%s/v2/org/repo/tags/list?last=b&n=1000>; rel="next"`, srv.URL))
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"b"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"c"}})
		}
	}))
	t.Cleanup(srv.Close)
	tags, err := mustClient(t, srv.URL, "", "").ListTags(context.Background(), "org/repo")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 3 || tags[0] != "a" || tags[1] != "b" || tags[2] != "c" {
		t.Fatalf("tags = %v", tags)
	}
}

func TestParseNextLink(t *testing.T) {
	t.Parallel()
	cases := []struct {
		header, want string
	}{
		{"", ""},
		{`</v2/r/tags/list?last=x>; rel="next"`, "http://base/v2/r/tags/list?last=x"},
		{`<https://other/v2/r/tags/list>; rel="next"`, "https://other/v2/r/tags/list"},
		{"garbage", ""},
		{">backwards<", ""},
	}
	for _, tc := range cases {
		if got := parseNextLink(tc.header, "http://base"); got != tc.want {
			t.Errorf("parseNextLink(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}
