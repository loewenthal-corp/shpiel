package hfapi

import (
	"net/url"
	"strings"
)

// RouteKind names an HF API endpoint shape.
type RouteKind string

// The endpoint shapes of the read API.
const (
	RouteRepoInfo RouteKind = "repo_info"
	RouteTree     RouteKind = "tree"
	RouteResolve  RouteKind = "resolve"
)

// Route is a parsed HF API path.
type Route struct {
	Kind     RouteKind
	RepoKind RepoKind
	Repo     RepoID
	// Revision is the requested ref/commit; empty means the default branch.
	Revision string
	// Path is the file path for resolve routes and the optional subtree
	// prefix for tree routes.
	Path string
}

// ParseRoute parses an escaped URL path (r.URL.EscapedPath()) against the
// Hub's URL grammar:
//
//	/api/{models|datasets}/{id}                     repo info @ default
//	/api/{models|datasets}/{id}/revision/{rev}      repo info @ rev
//	/api/{models|datasets}/{id}/tree/{rev}[/{path}] tree listing
//	/{id}/resolve/{rev}/{path}                      file resolve (models)
//	/datasets/{id}/resolve/{rev}/{path}             file resolve (datasets)
//
// where {id} is one or two segments ("gpt2" or "org/name"). Escaping
// matters: a revision like "refs%2Fpr%2F1" is one segment whose decoded
// value contains slashes, so parsing must happen on the escaped form and
// unescape per segment — never on the decoded path.
//
// The grammar is greedy on keywords, mirroring the Hub: a bare repo named
// literally "resolve" or "revision" cannot be disambiguated and loses.
func ParseRoute(escapedPath string) (Route, bool) {
	segs, ok := splitAndUnescape(escapedPath)
	if !ok || len(segs) == 0 {
		return Route{}, false
	}

	if segs[0] == "api" {
		return parseAPIRoute(segs[1:])
	}

	kind := RepoKindModel
	if segs[0] == "datasets" {
		kind = RepoKindDataset
		segs = segs[1:]
	}
	return parseResolveRoute(kind, segs)
}

func parseAPIRoute(segs []string) (Route, bool) {
	if len(segs) == 0 {
		return Route{}, false
	}
	var kind RepoKind
	switch segs[0] {
	case "models":
		kind = RepoKindModel
	case "datasets":
		kind = RepoKindDataset
	default:
		return Route{}, false
	}
	segs = segs[1:]

	// Locate the subcommand keyword after a 1- or 2-segment repo id;
	// the shorter repo id wins when both parses are possible.
	for _, idLen := range []int{1, 2} {
		if len(segs) < idLen {
			return Route{}, false
		}
		if len(segs) == idLen {
			repo, err := ParseRepoID(strings.Join(segs[:idLen], "/"))
			if err != nil {
				return Route{}, false
			}
			return Route{Kind: RouteRepoInfo, RepoKind: kind, Repo: repo}, true
		}
		switch segs[idLen] {
		case "revision":
			if len(segs) != idLen+2 {
				return Route{}, false
			}
			repo, err := ParseRepoID(strings.Join(segs[:idLen], "/"))
			if err != nil {
				return Route{}, false
			}
			return Route{Kind: RouteRepoInfo, RepoKind: kind, Repo: repo, Revision: segs[idLen+1]}, true
		case "tree":
			if len(segs) < idLen+2 {
				return Route{}, false
			}
			repo, err := ParseRepoID(strings.Join(segs[:idLen], "/"))
			if err != nil {
				return Route{}, false
			}
			return Route{
				Kind:     RouteTree,
				RepoKind: kind,
				Repo:     repo,
				Revision: segs[idLen+1],
				Path:     strings.Join(segs[idLen+2:], "/"),
			}, true
		}
	}
	return Route{}, false
}

func parseResolveRoute(kind RepoKind, segs []string) (Route, bool) {
	for _, idLen := range []int{1, 2} {
		if len(segs) < idLen+3 {
			return Route{}, false
		}
		if segs[idLen] == "resolve" {
			repo, err := ParseRepoID(strings.Join(segs[:idLen], "/"))
			if err != nil {
				return Route{}, false
			}
			return Route{
				Kind:     RouteResolve,
				RepoKind: kind,
				Repo:     repo,
				Revision: segs[idLen+1],
				Path:     strings.Join(segs[idLen+2:], "/"),
			}, true
		}
	}
	return Route{}, false
}

// splitAndUnescape splits an escaped path into decoded segments.
func splitAndUnescape(escapedPath string) ([]string, bool) {
	trimmed := strings.Trim(escapedPath, "/")
	if trimmed == "" {
		return nil, false
	}
	raw := strings.Split(trimmed, "/")
	segs := make([]string, len(raw))
	for i, s := range raw {
		if s == "" {
			return nil, false // double slash
		}
		dec, err := url.PathUnescape(s)
		if err != nil {
			return nil, false
		}
		segs[i] = dec
	}
	return segs, true
}
