package fakeregistry_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/fakeregistry"
)

// TestRejectsDataCarryingPutAfterChunk pins the strictness this fake
// exists for: the exact request sequence ociclient used to send — PATCH a
// chunk, then close with a PUT still carrying data but no Content-Range —
// must fail with 416 BLOB_UPLOAD_INVALID the way Zot fails it. If someone
// loosens the fake, this test (and the regression value of every test
// built on it) is what breaks.
func TestRejectsDataCarryingPutAfterChunk(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(fakeregistry.New())
	t.Cleanup(srv.Close)

	payload := []byte("hello world")
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	location := startUpload(t, srv.URL)
	patch(t, srv.URL, location, payload[:8], "0-7")

	// The buggy closing PUT: tail bytes, no Content-Range.
	resp := do(t, http.MethodPut, srv.URL+location+"?digest="+digest, payload[8:], "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("data-carrying PUT after PATCH: status %d, want 416", resp.StatusCode)
	}
	if !strings.Contains(string(body), "BLOB_UPLOAD_INVALID") {
		t.Fatalf("error body %q lacks BLOB_UPLOAD_INVALID", body)
	}

	// The correct closing sequence on a fresh session: PATCH everything,
	// then a bodyless PUT.
	location = startUpload(t, srv.URL)
	patch(t, srv.URL, location, payload[:8], "0-7")
	patch(t, srv.URL, location, payload[8:], "8-10")
	resp = do(t, http.MethodPut, srv.URL+location+"?digest="+digest, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bodyless closing PUT: status %d, want 201", resp.StatusCode)
	}
}

func startUpload(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Post(base+"/v2/org/model/blobs/uploads/", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("starting upload: status %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("no Location header on upload start")
	}
	return location
}

func patch(t *testing.T, base, location string, chunk []byte, contentRange string) {
	t.Helper()
	resp := do(t, http.MethodPatch, base+location, chunk, contentRange)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PATCH %s: status %d: %s", contentRange, resp.StatusCode, body)
	}
}

func do(t *testing.T, method, url string, body []byte, contentRange string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(body))
	if contentRange != "" {
		req.Header.Set("Content-Range", contentRange)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestChunkContiguity: out-of-order chunks are rejected like Zot rejects
// them (PutBlobChunk's from != session size check).
func TestChunkContiguity(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(fakeregistry.New())
	t.Cleanup(srv.Close)

	location := startUpload(t, srv.URL)
	patch(t, srv.URL, location, []byte("01234567"), "0-7")

	resp := do(t, http.MethodPatch, srv.URL+location, []byte("89"), "10-11")
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("gapped chunk: status %d, want 416", resp.StatusCode)
	}
}

// TestDigestVerification: finalizing with a digest that does not match the
// session content is rejected.
func TestDigestVerification(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(fakeregistry.New())
	t.Cleanup(srv.Close)

	location := startUpload(t, srv.URL)
	wrong := "sha256:" + strings.Repeat("0", 64)
	resp := do(t, http.MethodPut, fmt.Sprintf("%s%s?digest=%s", srv.URL, location, wrong), []byte("content"), "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "DIGEST_INVALID") {
		t.Fatalf("wrong digest: status %d body %s, want 400 DIGEST_INVALID", resp.StatusCode, body)
	}
}
