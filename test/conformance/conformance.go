// Package conformance is an executable specification of the Hugging Face
// Hub read API as Shpiel must serve it. Run points at any HF-compatible
// base URL, so the same suite validates Shpiel in-process, a deployed
// Shpiel, or (for calibrating the suite itself) huggingface.co.
//
// Every assertion here is a behavior some real client depends on:
// hf_hub_download reads X-Repo-Commit and ETag from HEAD; vLLM and
// safetensors lazy loading need byte ranges; snapshot_download walks
// siblings; huggingface_hub raises typed errors keyed on X-Error-Code.
package conformance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// FixtureFile is one file in the conformance repo.
type FixtureFile struct {
	Content []byte
	LFS     bool
}

// Fixture describes the repo the suite asserts against.
type Fixture struct {
	// Repo is the repo id, e.g. "conformance/basic".
	Repo string
	// CommitSHA of the revision "main" points at.
	CommitSHA string
	// Files maps path -> content. Must include at least one regular file,
	// one LFS file, and one file nested in a directory.
	Files map[string]FixtureFile
}

// BasicFixture returns the standard fixture used by Shpiel's own tests.
func BasicFixture() Fixture {
	weights := make([]byte, 8192)
	for i := range weights {
		weights[i] = byte(i % 251)
	}
	return Fixture{
		Repo: "conformance/basic",
		Files: map[string]FixtureFile{
			"config.json":          {Content: []byte(`{"model_type":"conformance","hidden_size":8}`)},
			"model.safetensors":    {Content: weights, LFS: true},
			"tokenizer/vocab.json": {Content: []byte(`{"a":1,"b":2}`)},
			"tokenizer/merges.txt": {Content: []byte("a b\nb c\n")},
		},
	}
}

// Run asserts the HF read-API contract for fx served at baseURL.
func Run(t *testing.T, baseURL string, fx Fixture) {
	t.Helper()
	c := &checker{
		base: strings.TrimRight(baseURL, "/"),
		fx:   fx,
		// hf clients HEAD with redirects disabled and read metadata off
		// the first response; the suite does the same so either direct
		// serving or CDN-redirect serving passes.
		noRedirect: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		follow:     &http.Client{},
	}

	t.Run("RepoInfo", c.repoInfo)
	t.Run("RepoInfoAtRevision", c.repoInfoAtRevision)
	t.Run("RepoInfoAtCommitSHA", c.repoInfoAtCommitSHA)
	t.Run("ResolveHeadRegular", c.resolveHeadRegular)
	t.Run("ResolveHeadLFS", c.resolveHeadLFS)
	t.Run("ResolveHeadDoesNotDownload", c.resolveGetAfterHead)
	t.Run("ResolveGet", c.resolveGet)
	t.Run("ResolveGetNested", c.resolveGetNested)
	t.Run("RangeRequests", c.rangeRequests)
	t.Run("ConditionalRequests", c.conditionalRequests)
	t.Run("Tree", c.tree)
	t.Run("TreeRecursive", c.treeRecursive)
	t.Run("TreeSubdir", c.treeSubdir)
	t.Run("ErrorRepoNotFound", c.errorRepoNotFound)
	t.Run("ErrorRevisionNotFound", c.errorRevisionNotFound)
	t.Run("ErrorEntryNotFound", c.errorEntryNotFound)
	t.Run("ValidateYAML", c.validateYAML)
}

type checker struct {
	base       string
	fx         Fixture
	noRedirect *http.Client
	follow     *http.Client
}

func (c *checker) url(format string, args ...any) string {
	return c.base + fmt.Sprintf(format, args...)
}

// result is a fully-consumed HTTP exchange; keeping raw *http.Response out
// of assertions makes leak-free body handling the helper's job alone.
type result struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (c *checker) get(t *testing.T, url string) result {
	t.Helper()
	return c.do(t, c.follow, http.MethodGet, url, nil)
}

func (c *checker) head(t *testing.T, url string) result {
	t.Helper()
	return c.do(t, c.noRedirect, http.MethodHead, url, http.Header{"Accept-Encoding": []string{"identity"}})
}

func (c *checker) do(t *testing.T, client *http.Client, method, url string, hdr http.Header) result {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body of %s: %v", url, err)
	}
	return result{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
}

// pickFile returns a fixture path selected by predicate.
func (c *checker) pickFile(t *testing.T, lfs, nested bool) (string, FixtureFile) {
	t.Helper()
	for p, f := range c.fx.Files {
		if f.LFS == lfs && strings.Contains(p, "/") == nested {
			return p, f
		}
	}
	t.Fatalf("fixture has no file with lfs=%v nested=%v", lfs, nested)
	return "", FixtureFile{}
}

func (c *checker) decodeModelInfo(t *testing.T, body []byte) hfapi.ModelInfo {
	t.Helper()
	var info hfapi.ModelInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("model info is not valid JSON: %v\n%s", err, body)
	}
	return info
}

// --- repo info ---

func (c *checker) repoInfo(t *testing.T) {
	res := c.get(t, c.url("/api/models/%s", c.fx.Repo))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", res.StatusCode, res.Body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	info := c.decodeModelInfo(t, res.Body)
	if info.ID != c.fx.Repo {
		t.Errorf("id = %q, want %q", info.ID, c.fx.Repo)
	}
	if info.SHA != c.fx.CommitSHA {
		t.Errorf("sha = %q, want %q", info.SHA, c.fx.CommitSHA)
	}
	if len(info.Siblings) != len(c.fx.Files) {
		t.Fatalf("siblings = %d, want %d", len(info.Siblings), len(c.fx.Files))
	}
	for _, sib := range info.Siblings {
		f, ok := c.fx.Files[sib.RFilename]
		if !ok {
			t.Errorf("unexpected sibling %q", sib.RFilename)
			continue
		}
		// Sizes are required for snapshot planning; LFS files must carry
		// lfs metadata with the sha256 spelling siblings use.
		if sib.Size == nil || *sib.Size != int64(len(f.Content)) {
			t.Errorf("sibling %s size = %v, want %d", sib.RFilename, sib.Size, len(f.Content))
		}
		if f.LFS {
			if sib.LFS == nil || sib.LFS.SHA256 != fakehub.SHA256Hex(f.Content) {
				t.Errorf("sibling %s lfs = %+v, want sha256 %s", sib.RFilename, sib.LFS, fakehub.SHA256Hex(f.Content))
			}
		} else if sib.LFS != nil {
			t.Errorf("sibling %s unexpectedly LFS", sib.RFilename)
		}
	}
}

func (c *checker) repoInfoAtRevision(t *testing.T) {
	res := c.get(t, c.url("/api/models/%s/revision/main", c.fx.Repo))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body: %s", res.StatusCode, res.Body)
	}
	if info := c.decodeModelInfo(t, res.Body); info.SHA != c.fx.CommitSHA {
		t.Errorf("sha = %q, want %q", info.SHA, c.fx.CommitSHA)
	}
}

func (c *checker) repoInfoAtCommitSHA(t *testing.T) {
	res := c.get(t, c.url("/api/models/%s/revision/%s", c.fx.Repo, c.fx.CommitSHA))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body: %s", res.StatusCode, res.Body)
	}
	if info := c.decodeModelInfo(t, res.Body); info.SHA != c.fx.CommitSHA {
		t.Errorf("sha = %q, want %q", info.SHA, c.fx.CommitSHA)
	}
}

// --- resolve metadata (the hf_hub_download HEAD contract) ---

func (c *checker) resolveHeadRegular(t *testing.T) {
	path, f := c.pickFile(t, false, false)
	resp := c.head(t, c.url("/%s/resolve/main/%s", c.fx.Repo, path))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(hfapi.HeaderRepoCommit); got != c.fx.CommitSHA {
		t.Errorf("X-Repo-Commit = %q, want %q", got, c.fx.CommitSHA)
	}
	wantETag := `"` + fakehub.GitBlobOID(f.Content) + `"`
	if got := resp.Header.Get("ETag"); got != wantETag {
		t.Errorf("ETag = %s, want %s (quoted git blob oid)", got, wantETag)
	}
	if got := resp.Header.Get("Content-Length"); got != fmt.Sprint(len(f.Content)) {
		t.Errorf("Content-Length = %q, want %d", got, len(f.Content))
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
}

func (c *checker) resolveHeadLFS(t *testing.T) {
	path, f := c.pickFile(t, true, false)
	resp := c.head(t, c.url("/%s/resolve/main/%s", c.fx.Repo, path))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 200 or 302", resp.StatusCode)
	}
	sha := fakehub.SHA256Hex(f.Content)
	if got := strings.Trim(resp.Header.Get(hfapi.HeaderLinkedETag), `"`); got != sha {
		t.Errorf("X-Linked-Etag = %q, want %q", got, sha)
	}
	if got := resp.Header.Get(hfapi.HeaderLinkedSize); got != fmt.Sprint(len(f.Content)) {
		t.Errorf("X-Linked-Size = %q, want %d", got, len(f.Content))
	}
	if got := resp.Header.Get(hfapi.HeaderRepoCommit); got != c.fx.CommitSHA {
		t.Errorf("X-Repo-Commit = %q, want %q", got, c.fx.CommitSHA)
	}
}

// resolveGetAfterHead orders a HEAD before any GET of a distinct file to
// pin the metadata-only property: a HEAD must succeed and carry complete
// metadata even for content the server has never materialized.
func (c *checker) resolveGetAfterHead(t *testing.T) {
	path, f := c.pickFile(t, false, true)
	resp := c.head(t, c.url("/%s/resolve/main/%s", c.fx.Repo, path))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != fmt.Sprint(len(f.Content)) {
		t.Errorf("Content-Length = %q, want %d", got, len(f.Content))
	}
}

// --- resolve content ---

func (c *checker) resolveGet(t *testing.T) {
	for _, lfs := range []bool{false, true} {
		path, f := c.pickFile(t, lfs, false)
		res := c.get(t, c.url("/%s/resolve/main/%s", c.fx.Repo, path))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d; body: %s", path, res.StatusCode, res.Body)
		}
		if string(res.Body) != string(f.Content) {
			t.Errorf("GET %s returned wrong bytes (%d vs %d)", path, len(res.Body), len(f.Content))
		}
	}
}

func (c *checker) resolveGetNested(t *testing.T) {
	path, f := c.pickFile(t, false, true)
	res := c.get(t, c.url("/%s/resolve/main/%s", c.fx.Repo, path))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if string(res.Body) != string(f.Content) {
		t.Errorf("nested file bytes mismatch")
	}
}

// rangeRequests: hf_hub_download resume and safetensors lazy loading both
// issue byte ranges; a server without them silently corrupts resumes.
func (c *checker) rangeRequests(t *testing.T) {
	path, f := c.pickFile(t, true, false)
	url := c.url("/%s/resolve/main/%s", c.fx.Repo, path)

	// Warm the content (a pull-through server may fetch lazily).
	if res := c.get(t, url); res.StatusCode != http.StatusOK {
		t.Fatalf("warmup GET = %d", res.StatusCode)
	}

	res := c.do(t, c.follow, http.MethodGet, url, http.Header{"Range": []string{"bytes=10-19"}})
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged GET status = %d, want 206", res.StatusCode)
	}
	if want := f.Content[10:20]; string(res.Body) != string(want) {
		t.Errorf("range bytes = %v, want %v", res.Body, want)
	}
	wantCR := fmt.Sprintf("bytes 10-19/%d", len(f.Content))
	if got := res.Header.Get("Content-Range"); got != wantCR {
		t.Errorf("Content-Range = %q, want %q", got, wantCR)
	}

	// Open-ended range (resume-from-offset).
	res = c.do(t, c.follow, http.MethodGet, url,
		http.Header{"Range": []string{fmt.Sprintf("bytes=%d-", len(f.Content)-5)}})
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("tail range status = %d, want 206", res.StatusCode)
	}
	if want := f.Content[len(f.Content)-5:]; string(res.Body) != string(want) {
		t.Errorf("tail range bytes mismatch")
	}
}

func (c *checker) conditionalRequests(t *testing.T) {
	path, _ := c.pickFile(t, false, false)
	url := c.url("/%s/resolve/main/%s", c.fx.Repo, path)
	etag := c.head(t, url).Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on HEAD")
	}
	res := c.do(t, c.follow, http.MethodGet, url, http.Header{"If-None-Match": []string{etag}})
	if res.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match status = %d, want 304", res.StatusCode)
	}
}

// --- tree ---

func (c *checker) treeGet(t *testing.T, url string) []hfapi.TreeEntry {
	t.Helper()
	res := c.get(t, url)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %s", url, res.StatusCode, res.Body)
	}
	var entries []hfapi.TreeEntry
	if err := json.Unmarshal(res.Body, &entries); err != nil {
		t.Fatalf("tree response is not a JSON array: %v\n%s", err, res.Body)
	}
	return entries
}

func (c *checker) tree(t *testing.T) {
	entries := c.treeGet(t, c.url("/api/models/%s/tree/main", c.fx.Repo))
	byPath := map[string]hfapi.TreeEntry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	// Top level: loose files plus collapsed directories.
	var wantFiles, wantDirs int
	seenDirs := map[string]bool{}
	for p := range c.fx.Files {
		if dir, _, nested := strings.Cut(p, "/"); nested {
			if !seenDirs[dir] {
				seenDirs[dir] = true
				wantDirs++
				if e, ok := byPath[dir]; !ok || e.Type != hfapi.TreeEntryTypeDirectory {
					t.Errorf("missing directory entry %q (got %+v)", dir, e)
				}
			}
			continue
		}
		wantFiles++
		e, ok := byPath[p]
		if !ok || e.Type != hfapi.TreeEntryTypeFile {
			t.Errorf("missing file entry %q", p)
			continue
		}
		if e.Size != int64(len(c.fx.Files[p].Content)) {
			t.Errorf("tree %s size = %d, want %d", p, e.Size, len(c.fx.Files[p].Content))
		}
		if e.OID == "" {
			t.Errorf("tree %s has empty oid", p)
		}
	}
	if len(entries) != wantFiles+wantDirs {
		t.Errorf("tree entries = %d, want %d", len(entries), wantFiles+wantDirs)
	}
}

func (c *checker) treeRecursive(t *testing.T) {
	entries := c.treeGet(t, c.url("/api/models/%s/tree/main?recursive=true", c.fx.Repo))
	files := 0
	for _, e := range entries {
		if e.Type == hfapi.TreeEntryTypeFile {
			files++
			f, ok := c.fx.Files[e.Path]
			if !ok {
				t.Errorf("unexpected recursive entry %q", e.Path)
				continue
			}
			if f.LFS && (e.LFS == nil || e.LFS.OID != fakehub.SHA256Hex(f.Content)) {
				t.Errorf("recursive %s lfs = %+v, want oid %s", e.Path, e.LFS, fakehub.SHA256Hex(f.Content))
			}
		}
	}
	if files != len(c.fx.Files) {
		t.Errorf("recursive files = %d, want %d", files, len(c.fx.Files))
	}
}

func (c *checker) treeSubdir(t *testing.T) {
	path, _ := c.pickFile(t, false, true)
	dir, _, _ := strings.Cut(path, "/")
	entries := c.treeGet(t, c.url("/api/models/%s/tree/main/%s", c.fx.Repo, dir))
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, dir+"/") {
			t.Errorf("subdir listing leaked %q", e.Path)
		}
	}
	if len(entries) == 0 {
		t.Error("subdir listing is empty")
	}
}

// --- errors ---

func (c *checker) assertError(t *testing.T, url string, wantStatus int, wantCode string) {
	t.Helper()
	res := c.get(t, url)
	if res.StatusCode != wantStatus {
		t.Errorf("GET %s status = %d, want %d", url, res.StatusCode, wantStatus)
	}
	if got := res.Header.Get(hfapi.HeaderErrorCode); got != wantCode {
		t.Errorf("GET %s X-Error-Code = %q, want %q", url, got, wantCode)
	}
	var e hfapi.ErrorResponse
	if err := json.Unmarshal(res.Body, &e); err != nil || e.Error == "" {
		t.Errorf("GET %s error body = %q, want JSON with error field", url, res.Body)
	}
}

func (c *checker) errorRepoNotFound(t *testing.T) {
	c.assertError(t, c.url("/api/models/no-such-org/no-such-repo"), http.StatusNotFound, hfapi.ErrorCodeRepoNotFound)
	c.assertError(t, c.url("/no-such-org/no-such-repo/resolve/main/config.json"), http.StatusNotFound, hfapi.ErrorCodeRepoNotFound)
}

func (c *checker) errorRevisionNotFound(t *testing.T) {
	c.assertError(t, c.url("/api/models/%s/revision/no-such-branch", c.fx.Repo), http.StatusNotFound, hfapi.ErrorCodeRevisionNotFound)
}

func (c *checker) errorEntryNotFound(t *testing.T) {
	c.assertError(t, c.url("/%s/resolve/main/no-such-file.bin", c.fx.Repo), http.StatusNotFound, hfapi.ErrorCodeEntryNotFound)
}

// validateYAML asserts the card pre-validation contract of
// HfApi._validate_yaml (huggingface_hub 1.x, called by upload_folder when
// a README.md is present; RepoCard.push_to_hub on 0.x): POST
// /api/validate-yaml returns a JSON body with "warnings" and "errors"
// lists of {"message"} — the client parses it unconditionally, even on
// the 400 path, so a non-JSON body breaks uploads outright. 200 means
// committable, 400 means rejected. 1.x sends JSON; releases before v0.24
// form-encoded the same fields, so both dialects must hold.
func (c *checker) validateYAML(t *testing.T) {
	validCard := "---\nlicense: apache-2.0\ntags:\n  - conformance\n---\n\n# Model card\n"
	brokenCard := "---\nlicense: [unclosed\n---\n"

	type note struct {
		Message string `json:"message"`
	}
	type validation struct {
		Warnings []note `json:"warnings"`
		Errors   []note `json:"errors"`
	}
	post := func(contentType string, body string) (int, validation) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, c.url("/api/validate-yaml"), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		resp, err := c.follow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		var v validation
		if err := json.Unmarshal(payload, &v); err != nil {
			t.Fatalf("validate-yaml response is not JSON (clients response.json() it even on 400): %v\n%s", err, payload)
		}
		return resp.StatusCode, v
	}
	asJSON := func(card string) string {
		payload, _ := json.Marshal(map[string]string{"repoType": "model", "content": card})
		return string(payload)
	}
	asForm := func(card string) string {
		return url.Values{"repoType": {"model"}, "content": {card}}.Encode()
	}

	if status, v := post("application/json", asJSON(validCard)); status != http.StatusOK || len(v.Errors) != 0 {
		t.Errorf("valid card (json) = %d %+v, want 200 with no errors", status, v)
	}
	if status, v := post("application/x-www-form-urlencoded", asForm(validCard)); status != http.StatusOK || len(v.Errors) != 0 {
		t.Errorf("valid card (form) = %d %+v, want 200 with no errors", status, v)
	}
	// A card with no metadata block is committable; the Hub only warns.
	if status, v := post("application/json", asJSON("# Just a readme\n")); status != http.StatusOK || len(v.Errors) != 0 {
		t.Errorf("card without metadata = %d %+v, want 200 with no errors", status, v)
	}

	for name, card := range map[string]string{
		"broken yaml": brokenCard,
		"non-mapping": "---\n- just\n- a\n- list\n---\n",
	} {
		status, v := post("application/json", asJSON(card))
		if status != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", name, status)
		}
		if len(v.Errors) == 0 || v.Errors[0].Message == "" {
			t.Errorf("%s: 400 without errors[].message; clients join those into the ValueError text", name)
		}
	}
}
