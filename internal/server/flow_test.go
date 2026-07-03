package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/metrics"
	"github.com/loewenthal-corp/shpiel/internal/relay"
)

// serverHarness exposes the Server internals alongside its test listener.
type serverHarness struct {
	server  *Server
	http    *httptest.Server
	metrics *metrics.Metrics
	root    string // fs backend root
}

func newServerHarness(t *testing.T, mutate func(*config.Config), opts Options) *serverHarness {
	t.Helper()
	cfg := config.Default()
	root := t.TempDir()
	cfg.Backends = map[string]config.BackendConfig{"fs": {Type: "fs", Path: root}}
	cfg.Routes = []config.Route{{Match: "*", Primary: "fs"}}
	if mutate != nil {
		mutate(&cfg)
	}
	bk, err := fsbackend.New("fs", cfg.Backends["fs"].Path)
	if err != nil {
		t.Fatal(err)
	}
	router, err := relay.NewRouter(cfg.Routes, map[string]backend.Backend{"fs": bk})
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	rl := relay.New(relay.Options{Router: router, Metrics: m})
	s := New(cfg, rl, m, opts)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &serverHarness{server: s, http: srv, metrics: m, root: root}
}

// httpResult is the drained form of a response: do closes the body, so
// callers only ever see status and headers.
type httpResult struct {
	StatusCode int
	Header     http.Header
}

func (h *serverHarness) do(t *testing.T, method, path string, headers map[string]string, body string) (httpResult, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, h.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResult{StatusCode: resp.StatusCode, Header: resp.Header}, data
}

// ndjson builds a commit payload from JSON-encoded lines.
func ndjson(t *testing.T, lines ...string) string {
	t.Helper()
	return strings.Join(lines, "\n") + "\n"
}

// TestHFWriteReadFlow drives the full HTTP surface a huggingface_hub
// upload/download session touches: create, preupload, LFS batch + upload,
// commit, repo info, tree, and resolve.
func TestHFWriteReadFlow(t *testing.T) {
	t.Parallel()
	h := newServerHarness(t, nil, Options{})

	// --- create ---
	resp, body := h.do(t, http.MethodPost, "/api/repos/create", nil, `{"name":"org/flow","type":"model"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create = %d: %s", resp.StatusCode, body)
	}
	var created hfapi.CreateRepoResponse
	if err := json.Unmarshal(body, &created); err != nil || created.Name != "org/flow" || !strings.Contains(created.URL, "/org/flow") {
		t.Fatalf("create response = %s", body)
	}
	// Conflict carries the url for exist_ok=True.
	resp, body = h.do(t, http.MethodPost, "/api/repos/create", nil, `{"name":"org/flow","type":"model"}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "url") {
		t.Fatalf("second create = %d: %s", resp.StatusCode, body)
	}
	// The name+organization dialect and single-segment names both parse.
	resp, body = h.do(t, http.MethodPost, "/api/repos/create", nil, `{"name":"dialect","organization":"org","type":"model"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("org-dialect create = %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &created); err != nil || created.Name != "org/dialect" {
		t.Fatalf("org-dialect create response = %s, want name org/dialect", body)
	}
	resp, _ = h.do(t, http.MethodPost, "/api/repos/create", nil, `{"name":"solo-model","type":"model"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single-name create = %d", resp.StatusCode)
	}
	// Malformed creates.
	for name, payload := range map[string]string{
		"bad type":   `{"name":"org/x","type":"space"}`,
		"bad json":   `{"name":`,
		"bad name":   `{"name":"###","type":"model"}`,
		"dataset ok": ``, // placeholder, tested below
	} {
		if name == "dataset ok" {
			continue
		}
		resp, _ = h.do(t, http.MethodPost, "/api/repos/create", nil, payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s create = %d, want 400", name, resp.StatusCode)
		}
	}

	// --- preupload steering ---
	pre := `{"files":[
		{"path":"README.md","size":100,"sample":"` + base64.StdEncoding.EncodeToString([]byte("# hi")) + `"},
		{"path":"model.safetensors","size":100},
		{"path":"huge.txt","size":99999999},
		{"path":"blob.dat","size":10,"sample":"` + base64.StdEncoding.EncodeToString([]byte("a\x00b")) + `"}]}`
	resp, body = h.do(t, http.MethodPost, "/api/models/org/flow/preupload/main", nil, pre)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preupload = %d: %s", resp.StatusCode, body)
	}
	var preResp hfapi.PreuploadResponse
	if err := json.Unmarshal(body, &preResp); err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for _, f := range preResp.Files {
		modes[f.Path] = f.UploadMode
	}
	want := map[string]string{
		"README.md":         hfapi.UploadModeRegular,
		"model.safetensors": hfapi.UploadModeLFS,
		"huge.txt":          hfapi.UploadModeLFS,
		"blob.dat":          hfapi.UploadModeLFS,
	}
	for p, m := range want {
		if modes[p] != m {
			t.Errorf("preupload mode for %s = %q, want %q", p, modes[p], m)
		}
	}
	// Preupload against an unknown repo fails before any bytes move.
	resp, _ = h.do(t, http.MethodPost, "/api/models/org/ghost/preupload/main", nil, pre)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("preupload unknown repo = %d, want 404", resp.StatusCode)
	}

	// --- LFS batch + upload ---
	weights := bytes.Repeat([]byte{9, 8, 7}, 2048)
	oid := fakehub.SHA256Hex(weights)
	batch := fmt.Sprintf(`{"operation":"upload","transfers":["basic"],"objects":[{"oid":"%s","size":%d},{"oid":"nothex","size":1}]}`, oid, len(weights))
	resp, body = h.do(t, http.MethodPost, "/org/flow.git/info/lfs/objects/batch",
		map[string]string{"Authorization": "Bearer tok", "X-Forwarded-Proto": "https"}, batch)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lfs batch = %d: %s", resp.StatusCode, body)
	}
	var batchResp hfapi.LFSBatchResponse
	if err := json.Unmarshal(body, &batchResp); err != nil {
		t.Fatal(err)
	}
	if len(batchResp.Objects) != 2 {
		t.Fatalf("batch objects = %d", len(batchResp.Objects))
	}
	action := batchResp.Objects[0].Actions["upload"]
	if action == nil {
		t.Fatalf("no upload action: %+v", batchResp.Objects[0])
	}
	if !strings.HasPrefix(action.Href, "https://") {
		t.Errorf("href %q does not honor X-Forwarded-Proto", action.Href)
	}
	if action.Header["Authorization"] != "Bearer tok" {
		t.Errorf("action headers = %+v, want caller auth echoed", action.Header)
	}
	if batchResp.Objects[1].Error == nil || batchResp.Objects[1].Error.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad-oid object = %+v, want 422 error", batchResp.Objects[1])
	}
	// Non-upload operations are refused.
	resp, _ = h.do(t, http.MethodPost, "/org/flow.git/info/lfs/objects/batch", nil, `{"operation":"download","objects":[]}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("download batch = %d, want 422", resp.StatusCode)
	}

	// Upload to the minted href (rewritten to the test server's base).
	uploadPath := "/shpiel-lfs/models/org/flow/" + oid + fmt.Sprintf("?size=%d", len(weights))
	req, _ := http.NewRequest(http.MethodPut, h.http.URL+uploadPath, bytes.NewReader(weights))
	upResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		t.Fatalf("lfs upload = %d", upResp.StatusCode)
	}
	// Batch again: object present now, no actions (dedup).
	resp, body = h.do(t, http.MethodPost, "/org/flow.git/info/lfs/objects/batch", nil,
		fmt.Sprintf(`{"operation":"upload","objects":[{"oid":"%s","size":%d}]}`, oid, len(weights)))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	var rebatch hfapi.LFSBatchResponse
	if err := json.Unmarshal(body, &rebatch); err != nil {
		t.Fatal(err)
	}
	if len(rebatch.Objects[0].Actions) != 0 {
		t.Fatalf("uploaded object still has actions: %+v", rebatch.Objects[0].Actions)
	}
	// Corrupt uploads are rejected.
	req, _ = http.NewRequest(http.MethodPut, h.http.URL+"/shpiel-lfs/models/org/flow/"+fakehub.SHA256Hex([]byte("other"))+"?size=5", strings.NewReader("wrong"))
	upResp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	upResp.Body.Close()
	if upResp.StatusCode < 400 {
		t.Fatalf("corrupt lfs upload accepted: %d", upResp.StatusCode)
	}
	// Malformed upload paths.
	for path, wantCode := range map[string]int{
		"/shpiel-lfs/spaces/org/flow/" + oid: http.StatusNotFound,
		"/shpiel-lfs/models/org/flow/zzz":    http.StatusBadRequest,
		"/shpiel-lfs/models/" + oid:          http.StatusNotFound, // repo segment missing entirely
	} {
		req, _ = http.NewRequest(http.MethodPut, h.http.URL+path, strings.NewReader("x"))
		upResp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		upResp.Body.Close()
		if upResp.StatusCode != wantCode {
			t.Errorf("PUT %s = %d, want %d", path, upResp.StatusCode, wantCode)
		}
	}

	// --- commit ---
	readme := []byte("# flow model\n")
	commit := ndjson(t,
		`{"key":"header","value":{"summary":"initial","description":""}}`,
		`{"key":"file","value":{"path":"README.md","encoding":"base64","content":"`+base64.StdEncoding.EncodeToString(readme)+`"}}`,
		`{"key":"file","value":{"path":"nested/config.json","encoding":"base64","content":"`+base64.StdEncoding.EncodeToString([]byte(`{"a":1}`))+`"}}`,
		fmt.Sprintf(`{"key":"lfsFile","value":{"path":"model.safetensors","algo":"sha256","oid":"%s","size":%d}}`, oid, len(weights)),
	)
	resp, body = h.do(t, http.MethodPost, "/api/models/org/flow/commit/main", nil, commit)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commit = %d: %s", resp.StatusCode, body)
	}
	var commitResp hfapi.CommitResponse
	if err := json.Unmarshal(body, &commitResp); err != nil || !commitResp.Success || commitResp.CommitOID == "" {
		t.Fatalf("commit response = %s", body)
	}

	// Commit payload validation.
	for name, payload := range map[string]string{
		"no header":   ndjson(t, `{"key":"file","value":{"path":"x","content":""}}`),
		"unknown key": ndjson(t, `{"key":"header","value":{"summary":"s"}}`, `{"key":"wat","value":{}}`),
		"bad base64":  ndjson(t, `{"key":"header","value":{"summary":"s"}}`, `{"key":"file","value":{"path":"x","content":"!!"}}`),
		"bad lfs oid": ndjson(t, `{"key":"header","value":{"summary":"s"}}`, `{"key":"lfsFile","value":{"path":"x","oid":"zz"}}`),
		"bad line":    "not json\n",
	} {
		resp, _ = h.do(t, http.MethodPost, "/api/models/org/flow/commit/main", nil, payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("commit %s = %d, want 400", name, resp.StatusCode)
		}
	}
	resp, _ = h.do(t, http.MethodPost, "/api/models/org/flow/commit/main?create_pr=1", nil, commit)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create_pr commit = %d, want 400", resp.StatusCode)
	}

	// --- repo info ---
	resp, body = h.do(t, http.MethodGet, "/api/models/org/flow", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("model info = %d", resp.StatusCode)
	}
	var info hfapi.ModelInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if info.SHA != commitResp.CommitOID || len(info.Siblings) != 3 {
		t.Fatalf("info = sha %s siblings %d", info.SHA, len(info.Siblings))
	}
	if info.Siblings[0].RFilename != "README.md" { // sorted
		t.Fatalf("siblings[0] = %+v", info.Siblings[0])
	}
	// Revision-pinned info.
	resp, _ = h.do(t, http.MethodGet, "/api/models/org/flow/revision/"+commitResp.CommitOID, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revision info = %d", resp.StatusCode)
	}

	// --- tree ---
	resp, body = h.do(t, http.MethodGet, "/api/models/org/flow/tree/main", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tree = %d", resp.StatusCode)
	}
	var entries []hfapi.TreeEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatal(err)
	}
	// Non-recursive: README.md, model.safetensors, nested/ dir.
	if len(entries) != 3 {
		t.Fatalf("tree entries = %+v", entries)
	}
	var sawDir bool
	for _, e := range entries {
		if e.Type == hfapi.TreeEntryTypeDirectory && e.Path == "nested" && e.OID != "" {
			sawDir = true
		}
	}
	if !sawDir {
		t.Fatalf("no synthesized directory entry: %+v", entries)
	}
	// Recursive flattens.
	resp, body = h.do(t, http.MethodGet, "/api/models/org/flow/tree/main?recursive=true", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[2].Path != "nested/config.json" {
		t.Fatalf("recursive tree = %+v", entries)
	}
	// Subpath listing.
	resp, body = h.do(t, http.MethodGet, "/api/models/org/flow/tree/main/nested", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "nested/config.json" {
		t.Fatalf("subtree = %+v", entries)
	}
	// Unknown subpath is EntryNotFound.
	resp, _ = h.do(t, http.MethodGet, "/api/models/org/flow/tree/main/void", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("void subtree = %d", resp.StatusCode)
	}
	// Pagination: limit 1 pages through with a Link cursor.
	resp, body = h.do(t, http.MethodGet, "/api/models/org/flow/tree/main?recursive=true&limit=1", nil, "")
	if err := json.Unmarshal(body, &entries); err != nil || len(entries) != 1 {
		t.Fatalf("paged tree = %s (%v)", body, err)
	}
	link := resp.Header.Get("Link")
	if link == "" {
		t.Fatal("no Link header on paged tree")
	}
	next := strings.Trim(strings.Split(link, ";")[0], "<>")
	resp, body = h.do(t, http.MethodGet, strings.TrimPrefix(next, h.http.URL), nil, "")
	if err := json.Unmarshal(body, &entries); err != nil || len(entries) != 1 {
		t.Fatalf("page 2 = %s", body)
	}

	// --- resolve ---
	resp, _ = h.do(t, http.MethodHead, "/org/flow/resolve/main/model.safetensors", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve HEAD = %d", resp.StatusCode)
	}
	if got := resp.Header.Get(hfapi.HeaderRepoCommit); got != commitResp.CommitOID {
		t.Errorf("X-Repo-Commit = %q", got)
	}
	if got := resp.Header.Get("ETag"); got != `"`+oid+`"` {
		t.Errorf("LFS ETag = %q, want quoted sha256", got)
	}
	if got := resp.Header.Get(hfapi.HeaderLinkedSize); got != fmt.Sprint(len(weights)) {
		t.Errorf("linked size = %q", got)
	}
	resp, body = h.do(t, http.MethodGet, "/org/flow/resolve/main/model.safetensors", nil, "")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, weights) {
		t.Fatalf("resolve GET = %d, %d bytes", resp.StatusCode, len(body))
	}
	// Ranged read.
	resp, body = h.do(t, http.MethodGet, "/org/flow/resolve/main/model.safetensors",
		map[string]string{"Range": "bytes=0-99"}, "")
	if resp.StatusCode != http.StatusPartialContent || len(body) != 100 || !bytes.Equal(body, weights[:100]) {
		t.Fatalf("ranged resolve = %d, %d bytes", resp.StatusCode, len(body))
	}
	// Missing file and repo carry HF error codes.
	resp, _ = h.do(t, http.MethodGet, "/org/flow/resolve/main/nope.txt", nil, "")
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get(hfapi.HeaderErrorCode) != hfapi.ErrorCodeEntryNotFound {
		t.Fatalf("missing file = %d code %q", resp.StatusCode, resp.Header.Get(hfapi.HeaderErrorCode))
	}
	resp, _ = h.do(t, http.MethodGet, "/api/models/org/ghost", nil, "")
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get(hfapi.HeaderErrorCode) != hfapi.ErrorCodeRepoNotFound {
		t.Fatalf("missing repo = %d code %q", resp.StatusCode, resp.Header.Get(hfapi.HeaderErrorCode))
	}

	// --- deletions ---
	del := ndjson(t,
		`{"key":"header","value":{"summary":"clean"}}`,
		`{"key":"deletedFile","value":{"path":"README.md"}}`,
		`{"key":"deletedFolder","value":{"path":"nested"}}`,
	)
	resp, _ = h.do(t, http.MethodPost, "/api/models/org/flow/commit/main", nil, del)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete commit = %d", resp.StatusCode)
	}
	resp, body = h.do(t, http.MethodGet, "/api/models/org/flow", nil, "")
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if len(info.Siblings) != 1 || info.Siblings[0].RFilename != "model.safetensors" {
		t.Fatalf("post-delete siblings = %+v", info.Siblings)
	}

	// --- repo delete ---
	resp, _ = h.do(t, http.MethodDelete, "/api/repos/delete", nil, `{"name":"org/flow","type":"model"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repo delete = %d", resp.StatusCode)
	}
	resp, _ = h.do(t, http.MethodGet, "/api/models/org/flow", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted repo info = %d", resp.StatusCode)
	}
	resp, _ = h.do(t, http.MethodDelete, "/api/repos/delete", nil, `{"name":"###","type":"model"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad delete = %d", resp.StatusCode)
	}
}

// TestTreePaginationEdges pins the limit/cursor handling: bogus values
// fall back to defaults, exact-fit pages carry no Link, and file entries
// keep their git OIDs.
func TestTreePaginationEdges(t *testing.T) {
	t.Parallel()
	h := newServerHarness(t, nil, Options{})
	if resp, _ := h.do(t, http.MethodPost, "/api/repos/create", nil, `{"name":"org/pages","type":"model"}`); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	fileA := []byte("aaa")
	commit := ndjson(t,
		`{"key":"header","value":{"summary":"s"}}`,
		`{"key":"file","value":{"path":"a.txt","content":"`+base64.StdEncoding.EncodeToString(fileA)+`"}}`,
		`{"key":"file","value":{"path":"b.txt","content":"`+base64.StdEncoding.EncodeToString([]byte("bbb"))+`"}}`,
		`{"key":"file","value":{"path":"c.txt","content":"`+base64.StdEncoding.EncodeToString([]byte("ccc"))+`"}}`,
		`{"key":"file","value":{"path":"empty.txt","content":""}}`,
	)
	if resp, body := h.do(t, http.MethodPost, "/api/models/org/pages/commit/main", nil, commit); resp.StatusCode != http.StatusOK {
		t.Fatalf("commit = %d: %s", resp.StatusCode, body)
	}

	page := func(query string) ([]hfapi.TreeEntry, string) {
		t.Helper()
		resp, body := h.do(t, http.MethodGet, "/api/models/org/pages/tree/main"+query, nil, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("tree%s = %d", query, resp.StatusCode)
		}
		var entries []hfapi.TreeEntry
		if err := json.Unmarshal(body, &entries); err != nil {
			t.Fatal(err)
		}
		return entries, resp.Header.Get("Link")
	}

	// Bogus limits fall back to defaults; oversized ones clamp.
	for _, q := range []string{"", "?limit=0", "?limit=-5", "?limit=2000", "?limit=abc"} {
		if entries, link := page(q); len(entries) != 4 || link != "" {
			t.Errorf("tree%s = %d entries, link %q", q, len(entries), link)
		}
	}
	// An exact-fit page has no next Link.
	if entries, link := page("?limit=4"); len(entries) != 4 || link != "" {
		t.Errorf("exact-fit page = %d entries, link %q", len(entries), link)
	}
	if entries, link := page("?limit=3"); len(entries) != 3 || link == "" {
		t.Errorf("short page = %d entries, link %q", len(entries), link)
	}
	// Garbage cursors are ignored, negative ones too.
	if entries, _ := page("?cursor=%%%"); len(entries) != 4 {
		t.Error("garbage cursor changed the page")
	}
	if entries, _ := page("?cursor=" + base64.URLEncoding.EncodeToString([]byte("-1"))); len(entries) != 4 {
		t.Error("negative cursor changed the page")
	}
	// A cursor beyond the entries yields an empty final page.
	if entries, link := page("?cursor=" + base64.URLEncoding.EncodeToString([]byte("999"))); len(entries) != 0 || link != "" {
		t.Errorf("past-the-end cursor = %d entries, link %q", len(entries), link)
	}

	// File entries carry the git blob OID, not the storage digest.
	entries, _ := page("")
	if entries[0].Path != "a.txt" || entries[0].OID != fakehub.GitBlobOID(fileA) {
		t.Errorf("entry oid = %+v, want git oid %s", entries[0], fakehub.GitBlobOID(fileA))
	}

	// Sibling sizes: zero-byte files have no size field, real ones do.
	resp, body := h.do(t, http.MethodGet, "/api/models/org/pages", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	var info hfapi.ModelInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	sizes := map[string]*int64{}
	for _, s := range info.Siblings {
		sizes[s.RFilename] = s.Size
	}
	if sizes["empty.txt"] != nil {
		t.Errorf("empty file size = %v, want omitted", *sizes["empty.txt"])
	}
	if sizes["a.txt"] == nil || *sizes["a.txt"] != 3 {
		t.Errorf("a.txt size = %v, want 3", sizes["a.txt"])
	}

	// Content types on resolve: .json is JSON, everything else is bytes.
	jcommit := ndjson(t,
		`{"key":"header","value":{"summary":"j"}}`,
		`{"key":"file","value":{"path":"m.json","content":"`+base64.StdEncoding.EncodeToString([]byte(`{}`))+`"}}`,
	)
	if resp, _ := h.do(t, http.MethodPost, "/api/models/org/pages/commit/main", nil, jcommit); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	resp, _ = h.do(t, http.MethodGet, "/org/pages/resolve/main/m.json", nil, "")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("json content type = %q", ct)
	}
	resp, _ = h.do(t, http.MethodGet, "/org/pages/resolve/main/a.txt", nil, "")
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("txt content type = %q", ct)
	}
}

// TestCommitPayloadEdges: optional NDJSON fields default sanely and the
// inline size limit is inclusive.
func TestCommitPayloadEdges(t *testing.T) {
	t.Parallel()
	h := newServerHarness(t, nil, Options{})
	if resp, _ := h.do(t, http.MethodPost, "/api/repos/create", nil, `{"name":"org/edges","type":"model"}`); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}

	// A file line without an encoding field defaults to base64; an lfsFile
	// without an algo defaults to sha256.
	weights := bytes.Repeat([]byte{4, 2}, 1024)
	oid := fakehub.SHA256Hex(weights)
	req, _ := http.NewRequest(http.MethodPut, h.http.URL+"/shpiel-lfs/models/org/edges/"+oid, bytes.NewReader(weights))
	upResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	upResp.Body.Close()
	// Note: no ?size= param — unknown-size uploads are digest-verified too.
	if upResp.StatusCode != http.StatusOK {
		t.Fatalf("no-size lfs upload = %d", upResp.StatusCode)
	}
	commit := ndjson(t,
		`{"key":"header","value":{"summary":"defaults"}}`,
		`{"key":"file","value":{"path":"plain.txt","content":"`+base64.StdEncoding.EncodeToString([]byte("x"))+`"}}`,
		fmt.Sprintf(`{"key":"lfsFile","value":{"path":"w.bin","oid":"%s","size":%d}}`, oid, len(weights)),
	)
	if resp, body := h.do(t, http.MethodPost, "/api/models/org/edges/commit/main", nil, commit); resp.StatusCode != http.StatusOK {
		t.Fatalf("defaulted commit = %d: %s", resp.StatusCode, body)
	}

	// An inline file of exactly the limit passes; the NDJSON scanner buffer
	// must absorb its base64 form.
	max := make([]byte, maxInlineFileSize)
	for i := range max {
		max[i] = byte(i)
	}
	commit = ndjson(t,
		`{"key":"header","value":{"summary":"big"}}`,
		`{"key":"file","value":{"path":"max.bin","content":"`+base64.StdEncoding.EncodeToString(max)+`"}}`,
	)
	if resp, body := h.do(t, http.MethodPost, "/api/models/org/edges/commit/main", nil, commit); resp.StatusCode != http.StatusOK {
		t.Fatalf("max-size inline commit = %d: %s", resp.StatusCode, body)
	}

	// Preupload steering is inclusive at the limit too.
	pre := fmt.Sprintf(`{"files":[{"path":"exact.txt","size":%d},{"path":"over.txt","size":%d}]}`, maxInlineFileSize, maxInlineFileSize+1)
	resp, body := h.do(t, http.MethodPost, "/api/models/org/edges/preupload/main", nil, pre)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	var preResp hfapi.PreuploadResponse
	if err := json.Unmarshal(body, &preResp); err != nil {
		t.Fatal(err)
	}
	for _, f := range preResp.Files {
		want := hfapi.UploadModeRegular
		if f.Path == "over.txt" {
			want = hfapi.UploadModeLFS
		}
		if f.UploadMode != want {
			t.Errorf("%s mode = %q, want %q", f.Path, f.UploadMode, want)
		}
	}

	// Single-segment repos work through the LFS upload path too.
	if resp, _ := h.do(t, http.MethodPost, "/api/repos/create", nil, `{"name":"bare","type":"model"}`); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPut, h.http.URL+"/shpiel-lfs/models/bare/"+oid+fmt.Sprintf("?size=%d", len(weights)), bytes.NewReader(weights))
	upResp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		t.Fatalf("bare-repo lfs upload = %d", upResp.StatusCode)
	}
}

func TestValidateYAML(t *testing.T) {
	t.Parallel()
	h := newServerHarness(t, nil, Options{})
	type note struct {
		Message string `json:"message"`
	}
	var out struct {
		Warnings []note `json:"warnings"`
		Errors   []note `json:"errors"`
	}
	check := func(contentType, body string, wantStatus, wantWarn, wantErr int) {
		t.Helper()
		resp, data := h.do(t, http.MethodPost, "/api/validate-yaml", map[string]string{"Content-Type": contentType}, body)
		if resp.StatusCode != wantStatus {
			t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, wantStatus, data)
		}
		out.Warnings, out.Errors = nil, nil
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("non-JSON validate response: %s", data)
		}
		if len(out.Warnings) != wantWarn || len(out.Errors) != wantErr {
			t.Fatalf("warnings/errors = %d/%d, want %d/%d (%s)", len(out.Warnings), len(out.Errors), wantWarn, wantErr, data)
		}
	}

	goodCard := "---\nlicense: apache-2.0\n---\n# hi\n"
	check("application/json", `{"repoType":"model","content":`+mustJSON(t, goodCard)+`}`, 200, 0, 0)
	check("application/json", `{"content":"no metadata here"}`, 200, 1, 0)
	check("application/json", `{"content":"---\n: [\n---\n"}`, 400, 0, 1)
	check("application/json", `{"content":"---\n- just\n- a list\n---\n"}`, 400, 0, 1)
	check("application/x-www-form-urlencoded", "content="+strings.ReplaceAll(goodCard, "\n", "%0A"), 200, 0, 0)

	resp, _ := h.do(t, http.MethodPost, "/api/validate-yaml", map[string]string{"Content-Type": "application/json"}, `{"content":`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json = %d", resp.StatusCode)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOperationalEndpoints(t *testing.T) {
	t.Parallel()
	h := newServerHarness(t, nil, Options{})

	resp, body := h.do(t, http.MethodGet, "/healthz", nil, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Fatalf("healthz = %d %s", resp.StatusCode, body)
	}
	resp, body = h.do(t, http.MethodGet, "/readyz", nil, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ready") {
		t.Fatalf("readyz = %d %s", resp.StatusCode, body)
	}
	resp, body = h.do(t, http.MethodGet, "/", nil, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "shpiel") {
		t.Fatalf("root = %d %s", resp.StatusCode, body)
	}

	// A read-only backend root turns readiness off but not liveness.
	if err := os.Chmod(h.root, 0o500); err != nil { //nolint:gosec // dir needs the x bit
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.root, 0o700) }) //nolint:gosec // restoring a traversable test dir
	resp, _ = h.do(t, http.MethodGet, "/readyz", nil, "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz on broken backend = %d, want 503", resp.StatusCode)
	}
	resp, _ = h.do(t, http.MethodGet, "/healthz", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz on broken backend = %d, want 200", resp.StatusCode)
	}
}

func TestDispatchRouting(t *testing.T) {
	t.Parallel()
	h := newServerHarness(t, nil, Options{})

	// Dataset routes parse but are not served yet.
	resp, body := h.do(t, http.MethodGet, "/api/datasets/org/name", nil, "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "not supported") {
		t.Fatalf("dataset info = %d %s", resp.StatusCode, body)
	}
	// Unparseable paths are plain 404s.
	resp, _ = h.do(t, http.MethodGet, "/api/spaces/org/name", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("spaces = %d", resp.StatusCode)
	}
	// Known route, wrong method.
	resp, _ = h.do(t, http.MethodDelete, "/api/models/org/name", nil, "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE model info = %d, want 405", resp.StatusCode)
	}
	// Xet token endpoint without xet enabled: actionable 404.
	resp, body = h.do(t, http.MethodGet, "/api/models/org/name/xet-read-token/main", nil, "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "HF_HUB_DISABLE_XET") {
		t.Fatalf("xet token = %d %s", resp.StatusCode, body)
	}
}
