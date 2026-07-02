package hfapi

import (
	"fmt"
	"regexp"
	"strings"
)

// RepoKind distinguishes the repo type namespaces of the Hub API.
type RepoKind string

// Repo kinds. Models ship in v1; datasets are v1.x per the spec.
const (
	RepoKindModel   RepoKind = "model"
	RepoKindDataset RepoKind = "dataset"
)

// APIPrefix returns the path segment used under /api/ ("models", "datasets").
func (k RepoKind) APIPrefix() string {
	return string(k) + "s"
}

// RepoID identifies a repository: an optional owner namespace plus a name.
// Canonical string form is "owner/name", or just "name" for legacy
// unnamespaced repos (e.g. "gpt2").
type RepoID struct {
	Owner string
	Name  string
}

var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ParseRepoID parses "owner/name" or "name". It rejects path-traversal
// shapes and empty segments.
func ParseRepoID(s string) (RepoID, error) {
	var id RepoID
	switch parts := strings.Split(s, "/"); len(parts) {
	case 1:
		id = RepoID{Name: parts[0]}
	case 2:
		id = RepoID{Owner: parts[0], Name: parts[1]}
	default:
		return RepoID{}, fmt.Errorf("invalid repo id %q: expected owner/name", s)
	}
	if id.Owner != "" && !repoNamePattern.MatchString(id.Owner) {
		return RepoID{}, fmt.Errorf("invalid repo owner %q", id.Owner)
	}
	if !repoNamePattern.MatchString(id.Name) {
		return RepoID{}, fmt.Errorf("invalid repo name %q", id.Name)
	}
	return id, nil
}

// String returns the canonical "owner/name" (or bare "name") form.
func (r RepoID) String() string {
	if r.Owner == "" {
		return r.Name
	}
	return r.Owner + "/" + r.Name
}

// IsZero reports whether the RepoID is empty.
func (r RepoID) IsZero() bool { return r.Name == "" }

var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsCommitSHA reports whether rev looks like a full git commit SHA.
func IsCommitSHA(rev string) bool { return commitSHAPattern.MatchString(rev) }
