package s3client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(Options{Endpoint: srv.URL, Bucket: "bkt", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{}); err == nil {
		t.Error("New without bucket accepted")
	}
	if _, err := New(Options{Bucket: "b", Endpoint: "://bad"}); err == nil {
		t.Error("New with invalid endpoint accepted")
	}
	if _, err := New(Options{Bucket: "b", Endpoint: "no-scheme"}); err == nil {
		t.Error("New with scheme-less endpoint accepted")
	}
}

// TestRequestURLs pins how keys map onto request URLs for both styles.
func TestRequestURLs(t *testing.T) {
	t.Parallel()
	aws, err := New(Options{Bucket: "models", Region: "eu-west-2"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := aws.newRequest(context.Background(), http.MethodGet, "a/b c", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := req.URL.String(), "https://models.s3.eu-west-2.amazonaws.com/a/b%20c"; got != want {
		t.Errorf("virtual-hosted URL = %s, want %s", got, want)
	}

	custom, err := New(Options{Bucket: "models", Endpoint: "http://minio:9000/base/"})
	if err != nil {
		t.Fatal(err)
	}
	req, err = custom.newRequest(context.Background(), http.MethodGet, "k", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := req.URL.String(), "http://minio:9000/base/models/k"; got != want {
		t.Errorf("path-style URL = %s, want %s", got, want)
	}
}

func TestHead(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s", r.Method)
		}
		switch r.URL.Path {
		case "/bkt/there":
			w.Header().Set("Content-Length", "42")
		case "/bkt/broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	size, err := c.Head(context.Background(), "there")
	if err != nil || size != 42 {
		t.Errorf("Head(there) = %d, %v, want 42, nil", size, err)
	}
	if _, err := c.Head(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Head(missing) = %v, want ErrNotFound", err)
	}
	if _, err := c.Head(context.Background(), "broken"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Head(broken) = %v, want non-NotFound error", err)
	}
}

func TestGetRanged(t *testing.T) {
	t.Parallel()
	content := []byte("0123456789abcdef")
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if r.URL.Path == "/bkt/whole" {
			// Offset 0 must be a plain GET: no Range header, no 206.
			if rng != "" {
				t.Errorf("offset-0 GET sent Range %q", rng)
			}
			w.Write(content)
			return
		}
		if rng == "" {
			w.Write(content)
			return
		}
		var offset int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-", &offset); err != nil {
			t.Errorf("bad range %q", rng)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(content)-1, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[offset:])
	}))
	rc, err := c.Get(context.Background(), "whole", 0)
	if err != nil {
		t.Fatalf("Get(0): %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(content) {
		t.Errorf("Get(0) = %q", got)
	}
	rc, err = c.Get(context.Background(), "obj", 10)
	if err != nil {
		t.Fatalf("Get(10): %v", err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "abcdef" {
		t.Errorf("Get(10) = %q, want abcdef", got)
	}
}

// TestGetRangeIgnored proves offset semantics hold against servers that
// answer 200 to ranged requests: the prefix is discarded client-side.
func TestGetRangeIgnored(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0123456789"))
	}))
	rc, err := c.Get(context.Background(), "obj", 7)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "789" {
		t.Errorf("Get(7) with ignored range = %q, want 789", got)
	}
}

func TestGetErrors(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bkt/missing":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>nope</Message></Error>`)
		case "/bkt/forbidden":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>denied</Message></Error>`)
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	if _, err := c.Get(context.Background(), "missing", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
	_, err := c.Get(context.Background(), "forbidden", 0)
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("Get(forbidden) = %v, want AccessDenied error", err)
	}
	_, err = c.Get(context.Background(), "weird", 0)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("Get(weird) = %v, want status error", err)
	}
}

func TestPut(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	var gotLen int64
	var gotHash string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/bkt/dir/obj" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		gotLen = r.ContentLength
		gotHash = r.Header.Get("x-amz-content-sha256")
	}))
	content := "hello world"
	err := c.Put(context.Background(), "dir/obj", strings.NewReader(content), int64(len(content)), "abc123")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if string(gotBody) != content || gotLen != int64(len(content)) {
		t.Errorf("body = %q (len %d)", gotBody, gotLen)
	}
	// Anonymous requests carry no SigV4 headers at all.
	if gotHash != "" {
		t.Errorf("anonymous Put sent x-amz-content-sha256 = %q", gotHash)
	}
}

func TestPutError(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>ro</Message></Error>`)
	}))
	err := c.Put(context.Background(), "k", strings.NewReader("x"), 1, "h")
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("Put = %v, want AccessDenied error", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path == "/bkt/boom" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := c.Delete(context.Background(), "gone"); err != nil {
		t.Errorf("Delete = %v", err)
	}
	if err := c.Delete(context.Background(), "boom"); err == nil {
		t.Error("Delete(boom) succeeded, want error")
	}
}

func TestListPagination(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("list-type") != "2" {
			t.Errorf("list-type = %q", q.Get("list-type"))
		}
		if q.Get("prefix") != "models/" {
			t.Errorf("prefix = %q", q.Get("prefix"))
		}
		if q.Get("max-keys") != "2" {
			t.Errorf("max-keys = %q", q.Get("max-keys"))
		}
		switch q.Get("continuation-token") {
		case "":
			fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult>
				<IsTruncated>true</IsTruncated>
				<Contents><Key>models/a</Key></Contents>
				<Contents><Key>models/b</Key></Contents>
				<NextContinuationToken>tok+1=</NextContinuationToken>
			</ListBucketResult>`)
		case "tok+1=":
			fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult>
				<IsTruncated>false</IsTruncated>
				<Contents><Key>models/c</Key></Contents>
			</ListBucketResult>`)
		default:
			t.Errorf("continuation-token = %q", q.Get("continuation-token"))
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	keys, next, err := c.List(context.Background(), "models/", "", 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 || keys[0] != "models/a" || keys[1] != "models/b" || next != "tok+1=" {
		t.Fatalf("page 1 = %v, next %q", keys, next)
	}
	keys, next, err = c.List(context.Background(), "models/", next, 2)
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(keys) != 1 || keys[0] != "models/c" || next != "" {
		t.Errorf("page 2 = %v, next %q", keys, next)
	}
}

func TestListErrors(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// maxKeys <= 0 must not send max-keys at all (service default).
		if r.URL.Query().Has("max-keys") {
			t.Errorf("default-page List sent max-keys=%q", r.URL.Query().Get("max-keys"))
		}
		switch r.URL.Query().Get("prefix") {
		case "no-token/":
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><Contents><Key>a</Key></Contents></ListBucketResult>`)
		case "bad-xml/":
			fmt.Fprint(w, `not xml at all <<<`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>NoSuchBucket</Code><Message>gone</Message></Error>`)
		}
	}))
	if _, _, err := c.List(context.Background(), "no-token/", "", 0); err == nil {
		t.Error("truncated list without token accepted")
	}
	if _, _, err := c.List(context.Background(), "bad-xml/", "", 0); err == nil {
		t.Error("malformed XML accepted")
	}
	if _, _, err := c.List(context.Background(), "gone/", "", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("List on missing bucket = %v, want ErrNotFound", err)
	}
}

func TestPing(t *testing.T) {
	t.Parallel()
	ok, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("max-keys") != "1" {
			t.Errorf("Ping max-keys = %q, want 1", r.URL.Query().Get("max-keys"))
		}
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	if err := ok.Ping(context.Background()); err != nil {
		t.Errorf("Ping = %v", err)
	}
	bad, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	if err := bad.Ping(context.Background()); err == nil {
		t.Error("Ping on 403 succeeded")
	}
}
