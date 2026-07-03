package relay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/backend/fsbackend"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/upstream"
)

// TestPullThroughSizePrecedence: the stored blob size prefers the resolve
// response's size over the (possibly missing) listing size, and falls back
// to the listing when the response streams chunked without a length.
// Real hubs exhibit both shapes; getting the size wrong fails blob
// verification.
func TestPullThroughSizePrecedence(t *testing.T) {
	t.Parallel()
	const commit = "cccccccccccccccccccccccccccccccccccccccc"
	sizedContent := bytes.Repeat([]byte{7}, 2048)   // sized GET, size missing from listing
	chunkedContent := bytes.Repeat([]byte{9}, 1024) // chunked GET, size present in listing
	sum1 := sha256.Sum256(sizedContent)
	sum2 := sha256.Sum256(chunkedContent)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models/org/odd/revision/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sha": commit,
			"siblings": []map[string]any{
				// No size anywhere in the listing for this one.
				{"rfilename": "listing-unsized.bin", "lfs": map[string]any{"sha256": hex.EncodeToString(sum1[:])}},
				// Size present in the listing for the chunked one.
				{"rfilename": "get-chunked.bin", "size": len(chunkedContent),
					"lfs": map[string]any{"sha256": hex.EncodeToString(sum2[:]), "size": len(chunkedContent)}},
			},
		})
	})
	serveFile := func(w http.ResponseWriter, r *http.Request, content []byte, sha string, chunked bool) {
		w.Header().Set(hfapi.HeaderRepoCommit, commit)
		w.Header().Set("ETag", `"`+sha+`"`)
		w.Header().Set(hfapi.HeaderLinkedETag, `"`+sha+`"`)
		if r.Method == http.MethodHead {
			w.Header().Set(hfapi.HeaderLinkedSize, fmt.Sprint(len(content)))
			w.WriteHeader(http.StatusOK)
			return
		}
		if chunked {
			// Flushing before the body forces chunked transfer: no
			// Content-Length reaches the client.
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_, _ = w.Write(content)
	}
	// Metadata resolves use the ref; blob pull-through pins the commit.
	mux.HandleFunc("/org/odd/resolve/{rev}/{file...}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("file") {
		case "listing-unsized.bin":
			serveFile(w, r, sizedContent, hex.EncodeToString(sum1[:]), false)
		case "get-chunked.bin":
			serveFile(w, r, chunkedContent, hex.EncodeToString(sum2[:]), true)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	bk, err := fsbackend.New("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter([]config.Route{{Match: "*", Primary: "test"}}, map[string]backend.Backend{"test": bk})
	if err != nil {
		t.Fatal(err)
	}
	rl := New(Options{Router: router, Upstream: upstream.New(srv.URL, ""), RefreshInterval: time.Hour})

	repo, _ := hfapi.ParseRepoID("org/odd")
	m, err := rl.ResolveManifest(t.Context(), hfapi.RepoKindModel, repo, "main", "")
	if err != nil {
		t.Fatalf("ResolveManifest: %v", err)
	}
	for name, want := range map[string][]byte{
		"listing-unsized.bin": sizedContent,
		"get-chunked.bin":     chunkedContent,
	} {
		content, err := rl.OpenFile(t.Context(), hfapi.RepoKindModel, repo, m, name, "")
		if err != nil {
			t.Fatalf("OpenFile(%s): %v", name, err)
		}
		got, err := io.ReadAll(content)
		content.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s = %d bytes, want %d", name, len(got), len(want))
		}
		if !strings.Contains(content.Source, "upstream") && content.Source != "cache" {
			t.Fatalf("%s source = %q", name, content.Source)
		}
	}
}
