package hfapi

import "testing"

// FuzzParseRepoID checks that ParseRepoID never panics on arbitrary input
// and that every accepted id round-trips through String() back to an
// equal, re-parseable RepoID. ParseRepoID sits on the untrusted request
// path (owner/name comes straight off the wire), so hardening it against
// malformed input is worthwhile.
func FuzzParseRepoID(f *testing.F) {
	for _, seed := range []string{
		"",
		"gpt2",
		"openai/whisper",
		"a/b/c",
		"/leading",
		"trailing/",
		"..",
		"../etc/passwd",
		"owner/na me",
		"Org_1/model.v2-final",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		id, err := ParseRepoID(s)
		if err != nil {
			return
		}

		// Name is mandatory on any successful parse.
		if id.Name == "" {
			t.Fatalf("ParseRepoID(%q) returned nil error with empty name", s)
		}

		// The canonical string must parse back to an identical id.
		got, err := ParseRepoID(id.String())
		if err != nil {
			t.Fatalf("re-parsing canonical form %q of %q failed: %v", id.String(), s, err)
		}
		if got != id {
			t.Fatalf("round-trip mismatch for %q: %#v -> %q -> %#v", s, id, id.String(), got)
		}
	})
}
