package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

func testRepo(t *testing.T) (*httptest.Server, hfapi.RepoID, map[string][]byte) {
	t.Helper()
	hub := fakehub.New()
	files := map[string][]byte{
		"config.json":       []byte(`{"model_type":"tiny"}`),
		"model.safetensors": bytes.Repeat([]byte{3}, 4096),
	}
	hub.AddModel("org/up", files, "model.safetensors")
	srv := httptest.NewServer(hub.Handler())
	t.Cleanup(srv.Close)
	repo, _ := hfapi.ParseRepoID("org/up")
	return srv, repo, files
}

func TestGetModelInfo(t *testing.T) {
	t.Parallel()
	srv, repo, files := testRepo(t)
	c := New(srv.URL+"/", "") // trailing slash must be trimmed
	if c.Endpoint() != srv.URL {
		t.Errorf("endpoint = %q", c.Endpoint())
	}

	info, err := c.GetModelInfo(context.Background(), hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatalf("GetModelInfo: %v", err)
	}
	if info.SHA == "" || len(info.Siblings) != len(files) {
		t.Fatalf("info = sha %q, %d siblings", info.SHA, len(info.Siblings))
	}
	var lfs *hfapi.Sibling
	for i := range info.Siblings {
		if info.Siblings[i].RFilename == "model.safetensors" {
			lfs = &info.Siblings[i]
		}
	}
	if lfs == nil || lfs.LFS == nil || lfs.LFS.Size != 4096 {
		t.Fatalf("lfs sibling = %+v", lfs)
	}

	if _, err := c.GetModelInfo(context.Background(), hfapi.RepoKindModel, mustRepo(t, "org/ghost"), "main", ""); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("ghost repo err = %v, want ErrRepoNotFound", err)
	}
	if _, err := c.GetModelInfo(context.Background(), hfapi.RepoKindModel, repo, "no-such-branch", ""); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("bad revision err = %v, want ErrRevisionNotFound", err)
	}
}

func mustRepo(t *testing.T, s string) hfapi.RepoID {
	t.Helper()
	id, err := hfapi.ParseRepoID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStatAndOpenFile(t *testing.T) {
	t.Parallel()
	srv, repo, files := testRepo(t)
	c := New(srv.URL, "")
	ctx := context.Background()

	meta, err := c.StatFile(ctx, hfapi.RepoKindModel, repo, "main", "model.safetensors", "")
	if err != nil {
		t.Fatalf("StatFile: %v", err)
	}
	if meta.CommitSHA == "" || meta.Size != 4096 {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.LinkedETag != fakehub.SHA256Hex(files["model.safetensors"]) {
		t.Fatalf("linked etag = %q", meta.LinkedETag)
	}

	body, meta, err := c.OpenFile(ctx, hfapi.RepoKindModel, repo, "main", "model.safetensors", "")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, files["model.safetensors"]) {
		t.Fatalf("read %d bytes, want %d", len(got), len(files["model.safetensors"]))
	}
	if meta.Size != 4096 {
		t.Fatalf("open meta = %+v", meta)
	}

	if _, err := c.StatFile(ctx, hfapi.RepoKindModel, repo, "main", "missing.txt", ""); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("missing file err = %v, want ErrEntryNotFound", err)
	}
	if _, _, err := c.OpenFile(ctx, hfapi.RepoKindModel, repo, "main", "missing.txt", ""); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("missing open err = %v, want ErrEntryNotFound", err)
	}
}

// TestAuthorizationPrecedence: an org token outranks the caller's token;
// otherwise the caller's token forwards; anonymous sends nothing. The
// User-Agent identifies shpiel either way.
func TestAuthorizationPrecedence(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var auth, ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth, ua = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"sha":"0123456789012345678901234567890123456789"}`))
	}))
	t.Cleanup(srv.Close)
	repo := mustRepo(t, "org/x")
	ctx := context.Background()

	cases := []struct {
		name        string
		orgToken    string
		callerToken string
		want        string
	}{
		{"org token wins", "org-secret", "caller", "Bearer org-secret"},
		{"caller token forwards", "", "caller", "Bearer caller"},
		{"anonymous", "", "", ""},
	}
	for _, tc := range cases {
		c := New(srv.URL, tc.orgToken)
		if _, err := c.GetModelInfo(ctx, hfapi.RepoKindModel, repo, "main", tc.callerToken); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		mu.Lock()
		gotAuth, gotUA := auth, ua
		mu.Unlock()
		if gotAuth != tc.want {
			t.Errorf("%s: Authorization = %q, want %q", tc.name, gotAuth, tc.want)
		}
		if !strings.HasPrefix(gotUA, "shpiel/") {
			t.Errorf("%s: User-Agent = %q", tc.name, gotUA)
		}
	}
}

// TestErrorMapping pins the X-Error-Code and status fallbacks.
func TestErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		code   string
		status int
		check  func(error) bool
	}{
		{"repo not found code", hfapi.ErrorCodeRepoNotFound, 404, func(e error) bool { return errors.Is(e, ErrRepoNotFound) }},
		{"revision not found code", hfapi.ErrorCodeRevisionNotFound, 404, func(e error) bool { return errors.Is(e, ErrRevisionNotFound) }},
		{"entry not found code", hfapi.ErrorCodeEntryNotFound, 404, func(e error) bool { return errors.Is(e, ErrEntryNotFound) }},
		{"gated repo code", hfapi.ErrorCodeGatedRepo, 403, func(e error) bool { return errors.Is(e, ErrUnauthorized) }},
		{"bare 401", "", 401, func(e error) bool { return errors.Is(e, ErrUnauthorized) }},
		{"bare 403", "", 403, func(e error) bool { return errors.Is(e, ErrUnauthorized) }},
		{"bare 404", "", 404, func(e error) bool { return errors.Is(e, ErrEntryNotFound) }},
		{"bare 500", "", 500, func(e error) bool {
			return e != nil && strings.Contains(e.Error(), "unexpected status 500")
		}},
		// 400 is the first error status: not success, not a mapped code.
		{"bare 400", "", 400, func(e error) bool {
			return e != nil && strings.Contains(e.Error(), "unexpected status 400")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.code != "" {
					w.Header().Set(hfapi.HeaderErrorCode, tc.code)
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			c := New(srv.URL, "")
			_, err := c.GetModelInfo(context.Background(), hfapi.RepoKindModel, mustRepo(t, "org/x"), "main", "")
			if !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestWhoAmIPassthrough(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/whoami-v2" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "Bearer good" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"name":"alice"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad token"}`))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "")

	status, body, err := c.WhoAmI(context.Background(), "good")
	if err != nil || status != http.StatusOK || !strings.Contains(string(body), "alice") {
		t.Fatalf("whoami = %d, %s, %v", status, body, err)
	}
	status, body, err = c.WhoAmI(context.Background(), "")
	if err != nil || status != http.StatusUnauthorized || !strings.Contains(string(body), "bad token") {
		t.Fatalf("anon whoami = %d, %s, %v", status, body, err)
	}
}

func TestPing(t *testing.T) {
	t.Parallel()
	// Any HTTP answer, even a 404, counts as reachable.
	srv := httptest.NewServer(http.NotFoundHandler())
	if err := New(srv.URL, "").Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	srv.Close()
	if err := New(srv.URL, "").Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded against a dead server")
	}
}

// TestResolveURLEscaping: dataset prefixing plus per-segment escaping of
// revisions and paths.
func TestResolveURLEscaping(t *testing.T) {
	t.Parallel()
	c := New("http://hub", "")
	repo := mustRepo(t, "org/name")
	got := c.resolveURL(hfapi.RepoKindModel, repo, "refs/pr/1", "vae/model weights.bin")
	want := "http://hub/org/name/resolve/refs%2Fpr%2F1/vae/model%20weights.bin"
	if got != want {
		t.Errorf("model url = %q, want %q", got, want)
	}
	got = c.resolveURL(hfapi.RepoKindDataset, repo, "main", "data.parquet")
	want = "http://hub/datasets/org/name/resolve/main/data.parquet"
	if got != want {
		t.Errorf("dataset url = %q, want %q", got, want)
	}
}

func TestFileMetaFromHeaders(t *testing.T) {
	t.Parallel()
	resp := &http.Response{Header: http.Header{}, ContentLength: 42}
	resp.Header.Set(hfapi.HeaderRepoCommit, "sha123")
	resp.Header.Set("ETag", `W/"abcdef"`)
	meta := fileMetaFromHeaders(resp)
	if meta.CommitSHA != "sha123" || meta.ETag != "abcdef" || meta.Size != 42 {
		t.Fatalf("meta = %+v", meta)
	}
	// X-Linked-Size wins over Content-Length (pointer vs content size).
	resp.Header.Set(hfapi.HeaderLinkedSize, "4096")
	resp.Header.Set(hfapi.HeaderLinkedETag, `"linked"`)
	meta = fileMetaFromHeaders(resp)
	if meta.Size != 4096 || meta.LinkedETag != "linked" {
		t.Fatalf("linked meta = %+v", meta)
	}
}

func TestManifestFromModelInfo(t *testing.T) {
	t.Parallel()
	repo := mustRepo(t, "org/conv")
	size := int64(7)
	lfsSize := int64(4096)
	fetched := time.Now().UTC()
	info := &hfapi.ModelInfo{
		SHA:          "0123456789012345678901234567890123456789",
		LastModified: fetched.Add(-time.Hour),
		Siblings: []hfapi.Sibling{
			{RFilename: "config.json", Size: &size, BlobID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{RFilename: "weights.bin", LFS: &hfapi.LFSInfo{SHA256: strings.Repeat("ab", 32), Size: lfsSize, PointerSize: 134}},
			{RFilename: "legacy.bin", LFS: &hfapi.LFSInfo{OID: strings.Repeat("cd", 32), Size: lfsSize}},
			// LFS record without a size: the sibling-level size stands.
			{RFilename: "sizeless-lfs.bin", Size: &size, LFS: &hfapi.LFSInfo{SHA256: strings.Repeat("ef", 32)}},
			{RFilename: "unknown.txt"}, // no digest info at all
		},
	}
	m, err := ManifestFromModelInfo(hfapi.RepoKindModel, repo, info, fetched)
	if err != nil {
		t.Fatal(err)
	}
	if m.CommitSHA != info.SHA || !m.FetchedAt.Equal(fetched) || !m.CreatedAt.Equal(info.LastModified) {
		t.Fatalf("manifest header = %+v", m)
	}
	byPath := map[string]int{}
	for i, f := range m.Files {
		byPath[f.Path] = i
	}

	cfg := m.Files[byPath["config.json"]]
	if cfg.Digest.Algo() != "sha1" || cfg.Size != 7 || cfg.OID == "" {
		t.Errorf("regular entry = %+v", cfg)
	}
	w := m.Files[byPath["weights.bin"]]
	if w.Digest.Algo() != "sha256" || w.Size != lfsSize || w.LFS == nil || w.LFS.PointerSize != 134 {
		t.Errorf("lfs entry = %+v", w)
	}
	// A legacy OID-only LFS record still keys by sha256.
	l := m.Files[byPath["legacy.bin"]]
	if l.Digest.Algo() != "sha256" || l.Digest.Hex() != strings.Repeat("cd", 32) {
		t.Errorf("legacy lfs entry = %+v", l)
	}
	// An LFS record without a size keeps the sibling-level size.
	sl := m.Files[byPath["sizeless-lfs.bin"]]
	if sl.Digest.Algo() != "sha256" || sl.Size != size {
		t.Errorf("sizeless lfs entry = %+v, want sibling size %d", sl, size)
	}
	// No digest info leaves a zero digest for the relay to backfill.
	u := m.Files[byPath["unknown.txt"]]
	if !u.Digest.IsZero() {
		t.Errorf("unknown entry = %+v", u)
	}

	if _, err := ManifestFromModelInfo(hfapi.RepoKindModel, repo, &hfapi.ModelInfo{}, fetched); err == nil {
		t.Fatal("model info without sha accepted")
	}
}
