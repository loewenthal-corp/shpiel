package conformance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/loewenthal-corp/shpiel/internal/fakehub"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// WriteClient drives the HF write protocol the way huggingface_hub does:
// create repo, preupload negotiation, LFS batch + PUT, NDJSON commit. It
// exists so the conformance suite can prove the write-what-you-read loop
// against any HF-compatible endpoint.
type WriteClient struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// NewWriteClient builds a client for an endpoint base URL.
func NewWriteClient(base, token string) *WriteClient {
	return &WriteClient{Base: strings.TrimRight(base, "/"), Token: token, HTTP: &http.Client{}}
}

func (c *WriteClient) do(method, url, contentType string, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

// CreateRepo creates a model repo; conflict (409) is returned as
// ErrRepoConflict so callers can treat it as exist_ok.
func (c *WriteClient) CreateRepo(repo string) error {
	payload, _ := json.Marshal(hfapi.CreateRepoRequest{Name: repo, Type: "model"})
	status, _, body, err := c.do(http.MethodPost, c.Base+"/api/repos/create", "application/json", payload)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		return ErrRepoConflict
	}
	if status != http.StatusOK {
		return fmt.Errorf("create repo: status %d: %s", status, body)
	}
	return nil
}

// ErrRepoConflict mirrors the Hub's 409 on re-creating a repo.
var ErrRepoConflict = fmt.Errorf("repo already exists")

// Preupload asks the server how each file should ship.
func (c *WriteClient) Preupload(repo, revision string, files map[string][]byte) (map[string]string, error) {
	req := hfapi.PreuploadRequest{}
	for path, content := range files {
		sample := content
		if len(sample) > 512 {
			sample = sample[:512]
		}
		req.Files = append(req.Files, hfapi.PreuploadFile{
			Path:   path,
			Size:   int64(len(content)),
			Sample: base64.StdEncoding.EncodeToString(sample),
		})
	}
	payload, _ := json.Marshal(req)
	status, _, body, err := c.do(http.MethodPost,
		fmt.Sprintf("%s/api/models/%s/preupload/%s", c.Base, repo, revision), "application/json", payload)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("preupload: status %d: %s", status, body)
	}
	var resp hfapi.PreuploadResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("preupload: %v", err)
	}
	modes := map[string]string{}
	for _, f := range resp.Files {
		modes[f.Path] = f.UploadMode
	}
	return modes, nil
}

// UploadLFS runs the batch negotiation and uploads whatever the server
// asks for, returning how many objects actually transferred (dedup skips).
func (c *WriteClient) UploadLFS(repo string, blobs map[string][]byte) (uploaded int, err error) {
	req := hfapi.LFSBatchRequest{Operation: "upload", Transfers: []string{"basic", "multipart"}, HashAlgo: "sha256"}
	byOID := map[string][]byte{}
	for _, content := range blobs {
		oid := fakehub.SHA256Hex(content)
		byOID[oid] = content
		req.Objects = append(req.Objects, hfapi.LFSBatchObject{OID: oid, Size: int64(len(content))})
	}
	payload, _ := json.Marshal(req)
	status, _, body, err := c.do(http.MethodPost,
		fmt.Sprintf("%s/%s.git/info/lfs/objects/batch", c.Base, repo), hfapi.LFSContentType, payload)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("lfs batch: status %d: %s", status, body)
	}
	var resp hfapi.LFSBatchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("lfs batch: %v", err)
	}
	for _, obj := range resp.Objects {
		if obj.Error != nil {
			return uploaded, fmt.Errorf("lfs object %s: %s", obj.OID, obj.Error.Message)
		}
		action, ok := obj.Actions["upload"]
		if !ok {
			continue // server already has it
		}
		putReq, err := http.NewRequest(http.MethodPut, action.Href, bytes.NewReader(byOID[obj.OID]))
		if err != nil {
			return uploaded, err
		}
		for k, v := range action.Header {
			putReq.Header.Set(k, v)
		}
		putResp, err := c.HTTP.Do(putReq)
		if err != nil {
			return uploaded, fmt.Errorf("lfs put %s: %v", obj.OID, err)
		}
		putBody, _ := io.ReadAll(putResp.Body)
		putResp.Body.Close()
		if putResp.StatusCode != http.StatusOK {
			return uploaded, fmt.Errorf("lfs put %s: status %d: %s", obj.OID, putResp.StatusCode, putBody)
		}
		uploaded++
	}
	return uploaded, nil
}

// Commit posts the NDJSON payload and returns the new commit sha.
func (c *WriteClient) Commit(repo, revision, summary string, inline map[string][]byte, lfs map[string][]byte, deleted []string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	writeLine := func(key string, value any) {
		_ = enc.Encode(hfapi.CommitLine{Key: key, Value: value})
	}
	writeLine("header", hfapi.CommitHeader{Summary: summary})
	for path, content := range inline {
		writeLine("file", hfapi.CommitFile{Path: path, Content: base64.StdEncoding.EncodeToString(content), Encoding: "base64"})
	}
	for path, content := range lfs {
		writeLine("lfsFile", hfapi.CommitLFSFile{
			Path: path, Algo: "sha256", OID: fakehub.SHA256Hex(content), Size: int64(len(content)),
		})
	}
	for _, path := range deleted {
		writeLine("deletedFile", hfapi.CommitDeleted{Path: path})
	}

	status, _, body, err := c.do(http.MethodPost,
		fmt.Sprintf("%s/api/models/%s/commit/%s", c.Base, repo, revision), "application/x-ndjson", buf.Bytes())
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("commit: status %d: %s", status, body)
	}
	var resp hfapi.CommitResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("commit: %v", err)
	}
	if resp.CommitOID == "" {
		return "", fmt.Errorf("commit: empty commitOid in %s", body)
	}
	return resp.CommitOID, nil
}

// PushFixture pushes fx through the full write protocol (create repo,
// preupload, LFS, commit) and returns the fixture with CommitSHA set to
// the server-minted commit.
func (c *WriteClient) PushFixture(fx Fixture) (Fixture, error) {
	if err := c.CreateRepo(fx.Repo); err != nil && err != ErrRepoConflict {
		return fx, err
	}
	files := map[string][]byte{}
	for path, f := range fx.Files {
		files[path] = f.Content
	}
	modes, err := c.Preupload(fx.Repo, "main", files)
	if err != nil {
		return fx, err
	}

	inline, lfs := map[string][]byte{}, map[string][]byte{}
	for path, f := range fx.Files {
		switch {
		case f.LFS && modes[path] != hfapi.UploadModeLFS:
			return fx, fmt.Errorf("preupload steered LFS fixture file %s to %q", path, modes[path])
		case f.LFS:
			lfs[path] = f.Content
		default:
			inline[path] = f.Content
		}
	}
	if _, err := c.UploadLFS(fx.Repo, lfs); err != nil {
		return fx, err
	}
	sha, err := c.Commit(fx.Repo, "main", "conformance push", inline, lfs, nil)
	if err != nil {
		return fx, err
	}
	fx.CommitSHA = sha
	return fx, nil
}
