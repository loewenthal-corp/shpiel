package backend

import (
	"strings"
	"testing"

	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

func TestParseDigest(t *testing.T) {
	t.Parallel()
	good := []string{
		"sha256:" + strings.Repeat("ab", 32),
		"sha1:" + strings.Repeat("0f", 20),
		"sha256:abcdef", // short hex is tolerated (6+ chars)
	}
	for _, s := range good {
		d, err := ParseDigest(s)
		if err != nil || d.String() != s {
			t.Errorf("ParseDigest(%q) = %q, %v", s, d, err)
		}
	}
	bad := []string{"", "sha256", "sha256:", "md5:abcdef", "sha256:XYZ", "sha256:abc", "sha256:" + strings.Repeat("a", 129)}
	for _, s := range bad {
		if _, err := ParseDigest(s); err == nil {
			t.Errorf("ParseDigest(%q) accepted", s)
		}
	}
}

func TestDigestParts(t *testing.T) {
	t.Parallel()
	d := SHA256Digest("ABCDEF012345")
	if d.Algo() != "sha256" || d.Hex() != "abcdef012345" { // lowercased
		t.Fatalf("digest = %q (algo %q, hex %q)", d, d.Algo(), d.Hex())
	}
	if SHA1Digest("aa").Algo() != "sha1" {
		t.Fatal("sha1 algo wrong")
	}
	if NewDigest("sha256", "aa").String() != "sha256:aa" {
		t.Fatal("NewDigest form wrong")
	}
	// Malformed digests degrade predictably.
	if Digest("noprefix").Algo() != "" || Digest("noprefix").Hex() != "noprefix" {
		t.Fatal("prefix-less digest parts wrong")
	}
	if !Digest("").IsZero() || Digest("sha256:aa").IsZero() {
		t.Fatal("IsZero wrong")
	}
}

func TestManifestFile(t *testing.T) {
	t.Parallel()
	m := &Manifest{Files: []FileEntry{
		{Path: "a.txt", Size: 1},
		{Path: "dir/b.txt", Size: 2},
	}}
	if f := m.File("dir/b.txt"); f == nil || f.Size != 2 {
		t.Fatalf("File(dir/b.txt) = %+v", f)
	}
	if f := m.File("missing"); f != nil {
		t.Fatalf("File(missing) = %+v", f)
	}
	// The returned pointer aliases the manifest, so callers can mutate.
	m.File("a.txt").Size = 10
	if m.Files[0].Size != 10 {
		t.Fatal("File returned a copy")
	}
}

// TestFileEntryETag pins the ETag precedence: LFS sha256, then git OID,
// then the raw storage digest.
func TestFileEntryETag(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("ab", 32)
	e := FileEntry{
		Digest: SHA256Digest(strings.Repeat("ee", 32)),
		OID:    "gitoid123",
		LFS:    &hfapi.LFSInfo{SHA256: sha},
	}
	if e.ETag() != sha {
		t.Errorf("lfs etag = %q, want content sha", e.ETag())
	}
	e.LFS = &hfapi.LFSInfo{} // LFS present but sha empty: fall through
	if e.ETag() != "gitoid123" {
		t.Errorf("oid etag = %q", e.ETag())
	}
	e.LFS = nil
	if e.ETag() != "gitoid123" {
		t.Errorf("plain oid etag = %q", e.ETag())
	}
	e.OID = ""
	if e.ETag() != strings.Repeat("ee", 32) {
		t.Errorf("fallback etag = %q, want storage digest hex", e.ETag())
	}
}
