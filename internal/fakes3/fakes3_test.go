package fakes3_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/fakes3"
	"github.com/loewenthal-corp/shpiel/internal/s3client"
)

const (
	testAccessKey = "AKIDFAKES3TEST"
	testSecretKey = "fake-secret-key/with+chars"
)

func newSigned(t *testing.T) (*s3client.Client, *fakes3.Server, string) {
	t.Helper()
	fake := fakes3.New("models", testAccessKey, testSecretKey)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := s3client.New(s3client.Options{
		Endpoint: srv.URL,
		Bucket:   "models",
		Region:   "us-east-1",
		Credentials: s3client.Credentials{
			AccessKeyID:     testAccessKey,
			SecretAccessKey: testSecretKey,
			SessionToken:    "session-token-123",
		},
	})
	if err != nil {
		t.Fatalf("s3client.New: %v", err)
	}
	return c, fake, srv.URL
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestSignedRoundtrip is the two-implementation handshake: s3client's
// signer against fakes3's independently written verifier, across every
// request shape the bucket backend uses.
func TestSignedRoundtrip(t *testing.T) {
	t.Parallel()
	c, fake, _ := newSigned(t)
	ctx := context.Background()

	content := []byte("weights weights weights")
	key := "models/org/name/blobs/" + sha256hex(content)
	if err := c.Put(ctx, key, strings.NewReader(string(content)), int64(len(content)), sha256hex(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, ok := fake.Object(key); !ok || string(got) != string(content) {
		t.Fatalf("stored object = %q, %v", got, ok)
	}

	size, err := c.Head(ctx, key)
	if err != nil || size != int64(len(content)) {
		t.Fatalf("Head = %d, %v", size, err)
	}

	rc, err := c.Get(ctx, key, 8)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(content[8:]) {
		t.Errorf("ranged Get = %q, want %q", got, content[8:])
	}

	keys, next, err := c.List(ctx, "models/org/name/", "", 10)
	if err != nil || next != "" || len(keys) != 1 || keys[0] != key {
		t.Errorf("List = %v, %q, %v", keys, next, err)
	}

	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}

	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Head(ctx, key); !errors.Is(err, s3client.ErrNotFound) {
		t.Errorf("Head after delete = %v, want ErrNotFound", err)
	}
	// Deleting again still succeeds (S3 semantics).
	if err := c.Delete(ctx, key); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// TestListPaginationSigned walks multiple pages through base64
// continuation tokens (whose '=' padding and '+'/'/' bytes must survive
// the query-string signing round-trip).
func TestListPaginationSigned(t *testing.T) {
	t.Parallel()
	c, _, _ := newSigned(t)
	ctx := context.Background()

	want := []string{
		"models/o/r/blobs/aa", "models/o/r/blobs/bb", "models/o/r/blobs/cc",
		"models/o/r/manifests/x.json", "models/o/r/refs/main",
	}
	for _, k := range want {
		if err := c.Put(ctx, k, strings.NewReader("v"), 1, sha256hex([]byte("v"))); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	var got []string
	token := ""
	pages := 0
	for {
		keys, next, err := c.List(ctx, "models/o/r/", token, 2)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		got = append(got, keys...)
		pages++
		if next == "" {
			break
		}
		token = next
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func TestRejectsBadCredentials(t *testing.T) {
	t.Parallel()
	_, _, url := newSigned(t)
	ctx := context.Background()

	cases := map[string]s3client.Credentials{
		"WrongSecret":    {AccessKeyID: testAccessKey, SecretAccessKey: "not-the-secret"},
		"WrongAccessKey": {AccessKeyID: "AKIDSOMEONEELSE", SecretAccessKey: testSecretKey},
		"Anonymous":      {},
	}
	for name, creds := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, err := s3client.New(s3client.Options{
				Endpoint: url, Bucket: "models", Region: "us-east-1", Credentials: creds,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Head(ctx, "anything"); err == nil || errors.Is(err, s3client.ErrNotFound) {
				t.Errorf("Head with %s credentials = %v, want auth error", name, err)
			}
			if err := c.Put(ctx, "k", strings.NewReader("v"), 1, sha256hex([]byte("v"))); err == nil {
				t.Errorf("Put with %s credentials accepted", name)
			}
		})
	}
}

func TestRejectsPayloadHashMismatch(t *testing.T) {
	t.Parallel()
	c, fake, _ := newSigned(t)
	err := c.Put(context.Background(), "k", strings.NewReader("actual body"), 11, sha256hex([]byte("claimed other body")))
	if err == nil || !strings.Contains(err.Error(), "XAmzContentSHA256Mismatch") {
		t.Errorf("Put with wrong payload hash = %v, want XAmzContentSHA256Mismatch", err)
	}
	if _, ok := fake.Object("k"); ok {
		t.Error("object stored despite payload hash mismatch")
	}
}

func TestRejectsTamperedRequest(t *testing.T) {
	t.Parallel()
	_, _, url := newSigned(t)
	// A correctly signed request whose path is then altered must fail:
	// grab a valid Authorization header by signing for one key, replay it
	// against another.
	c, err := s3client.New(s3client.Options{
		Endpoint: url, Bucket: "models", Region: "us-east-1",
		Credentials: s3client.Credentials{AccessKeyID: testAccessKey, SecretAccessKey: testSecretKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put(context.Background(), "legit", strings.NewReader("v"), 1, sha256hex([]byte("v"))); err != nil {
		t.Fatalf("legit Put: %v", err)
	}

	var captured http.Header
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(capture.Close)
	cc, err := s3client.New(s3client.Options{
		Endpoint: capture.URL, Bucket: "models", Region: "us-east-1",
		Credentials: s3client.Credentials{AccessKeyID: testAccessKey, SecretAccessKey: testSecretKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cc.Head(context.Background(), "original"); err != nil {
		t.Fatalf("capture Head: %v", err)
	}

	replay, err := http.NewRequest(http.MethodHead, url+"/models/tampered", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Authorization", "x-amz-date", "x-amz-content-sha256"} {
		replay.Header.Set(h, captured.Get(h))
	}
	resp, err := http.DefaultClient.Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("tampered replay status = %d, want 403", resp.StatusCode)
	}
}

func TestAnonymousMode(t *testing.T) {
	t.Parallel()
	fake := fakes3.New("b", "", "")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := s3client.New(s3client.Options{Endpoint: srv.URL, Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put(context.Background(), "k", strings.NewReader("v"), 1, sha256hex([]byte("v"))); err != nil {
		t.Errorf("anonymous Put: %v", err)
	}
	if keys := fake.Keys(); len(keys) != 1 || keys[0] != "k" {
		t.Errorf("Keys = %v", keys)
	}
}

func TestWrongBucket(t *testing.T) {
	t.Parallel()
	fake := fakes3.New("right", "", "")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := s3client.New(s3client.Options{Endpoint: srv.URL, Bucket: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Head(context.Background(), "k"); !errors.Is(err, s3client.ErrNotFound) {
		t.Errorf("Head on wrong bucket = %v, want ErrNotFound", err)
	}
	if _, _, err := c.List(context.Background(), "", "", 0); !errors.Is(err, s3client.ErrNotFound) {
		t.Errorf("List on wrong bucket = %v, want ErrNotFound", err)
	}
}

func TestRangeShapes(t *testing.T) {
	t.Parallel()
	fake := fakes3.New("b", "", "")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := s3client.New(s3client.Options{Endpoint: srv.URL, Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	content := "0123456789"
	if err := c.Put(ctx, "k", strings.NewReader(content), 10, sha256hex([]byte(content))); err != nil {
		t.Fatal(err)
	}

	get := func(rng string) (int, string, string) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/b/k", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), resp.Header.Get("Content-Range")
	}

	if status, body, _ := get(""); status != http.StatusOK || body != content {
		t.Errorf("full GET = %d %q", status, body)
	}
	if status, body, cr := get("bytes=4-"); status != http.StatusPartialContent || body != "456789" || cr != "bytes 4-9/10" {
		t.Errorf("open range = %d %q %q", status, body, cr)
	}
	if status, body, cr := get("bytes=2-5"); status != http.StatusPartialContent || body != "2345" || cr != "bytes 2-5/10" {
		t.Errorf("bounded range = %d %q %q", status, body, cr)
	}
	if status, body, _ := get("bytes=3-99"); status != http.StatusPartialContent || body != "3456789" {
		t.Errorf("overlong range = %d %q", status, body)
	}
	for _, bad := range []string{"bytes=99-", "bytes=5-2", "bytes=x-", "items=1-2"} {
		if status, _, _ := get(bad); status != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("range %q status = %d, want 416", bad, status)
		}
	}
}

func TestHeadHasNoBody(t *testing.T) {
	t.Parallel()
	fake := fakes3.New("b", "", "")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := s3client.New(s3client.Options{Endpoint: srv.URL, Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Put(ctx, "k", strings.NewReader("12345"), 5, sha256hex([]byte("12345"))); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Head(srv.URL + "/b/k")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.ContentLength != 5 {
		t.Errorf("HEAD Content-Length = %d, want 5", resp.ContentLength)
	}
}

func TestListValidation(t *testing.T) {
	t.Parallel()
	fake := fakes3.New("b", "", "")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	for _, q := range []string{"", "list-type=1", "list-type=2&max-keys=nope", "list-type=2&continuation-token=!!!"} {
		resp, err := http.Get(fmt.Sprintf("%s/b/?%s", srv.URL, q))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("list query %q status = %d, want 400", q, resp.StatusCode)
		}
	}
}
