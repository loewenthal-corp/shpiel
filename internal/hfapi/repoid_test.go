package hfapi

import "testing"

func TestAPIPrefix(t *testing.T) {
	t.Parallel()
	if got := RepoKindModel.APIPrefix(); got != "models" {
		t.Errorf("model prefix = %q", got)
	}
	if got := RepoKindDataset.APIPrefix(); got != "datasets" {
		t.Errorf("dataset prefix = %q", got)
	}
}

func TestParseRepoID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		owner string
		name  string
		ok    bool
	}{
		{"org/name", "org", "name", true},
		{"gpt2", "", "gpt2", true},
		{"Org-1/Model.Name_v2", "Org-1", "Model.Name_v2", true},
		{"a/b/c", "", "", false},
		{"", "", "", false},
		{"../etc", "", "", false},
		{"org/", "", "", false},
		{".hidden/x", "", "", false}, // owner must start alphanumeric
	}
	for _, tc := range cases {
		id, err := ParseRepoID(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("ParseRepoID(%q) err = %v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && (id.Owner != tc.owner || id.Name != tc.name || id.String() != tc.in) {
			t.Errorf("ParseRepoID(%q) = %+v (String %q)", tc.in, id, id.String())
		}
	}
	if !(RepoID{}).IsZero() || (RepoID{Name: "x"}).IsZero() {
		t.Error("IsZero wrong")
	}
	// Quirk pinned on purpose: a leading slash yields an empty owner, which
	// collapses to the bare-name form.
	if id, err := ParseRepoID("/name"); err != nil || id.String() != "name" {
		t.Errorf(`ParseRepoID("/name") = %+v, %v`, id, err)
	}
}

func TestIsCommitSHA(t *testing.T) {
	t.Parallel()
	if !IsCommitSHA("0123456789abcdef0123456789abcdef01234567") {
		t.Error("valid sha rejected")
	}
	for _, bad := range []string{"", "main", "0123456789abcdef0123456789abcdef0123456", "0123456789ABCDEF0123456789abcdef01234567"} {
		if IsCommitSHA(bad) {
			t.Errorf("IsCommitSHA(%q) = true", bad)
		}
	}
}
