package hfapi

import (
	"net/url"
	"strings"
)

// RouteKind names an HF API endpoint shape.
type RouteKind string

// The endpoint shapes of the read and write APIs.
const (
	RouteRepoInfo  RouteKind = "repo_info"
	RouteTree      RouteKind = "tree"
	RouteResolve   RouteKind = "resolve"
	RoutePreupload RouteKind = "preupload"
	RouteCommit    RouteKind = "commit"
	RouteLFSBatch  RouteKind = "lfs_batch"
	// RouteXetToken covers /xet-read-token/{rev} and /xet-write-token/{rev},
	// recognized so the not-supported answer is deliberate and actionable
	// (huggingface_hub >= 1.x requests them by default when hf_xet is
	// installed).
	RouteXetToken RouteKind = "xet_token"
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
//	/api/{models|datasets}/{id}/preupload/{rev}     upload negotiation
//	/api/{models|datasets}/{id}/commit/{rev}        NDJSON commit
//	/{id}/resolve/{rev}/{path}                      file resolve (models)
//	/datasets/{id}/resolve/{rev}/{path}             file resolve (datasets)
//	/{id}.git/info/lfs/objects/batch                git-lfs batch API
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
		case "revision", "preupload", "commit", "xet-read-token", "xet-write-token":
			if len(segs) != idLen+2 {
				return Route{}, false
			}
			repo, err := ParseRepoID(strings.Join(segs[:idLen], "/"))
			if err != nil {
				return Route{}, false
			}
			routeKind := map[string]RouteKind{
				"revision":        RouteRepoInfo,
				"preupload":       RoutePreupload,
				"commit":          RouteCommit,
				"xet-read-token":  RouteXetToken,
				"xet-write-token": RouteXetToken,
			}[segs[idLen]]
			return Route{Kind: routeKind, RepoKind: kind, Repo: repo, Revision: segs[idLen+1]}, true
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
	if route, ok := parseLFSBatchRoute(kind, segs); ok {
		return route, true
	}
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

// parseLFSBatchRoute matches /{id}.git/info/lfs/objects/batch, where the
// ".git" suffix rides on the repo name's last segment.
func parseLFSBatchRoute(kind RepoKind, segs []string) (Route, bool) {
	for _, idLen := range []int{1, 2} {
		if len(segs) != idLen+4 {
			continue
		}
		if segs[idLen] != "info" || segs[idLen+1] != "lfs" || segs[idLen+2] != "objects" || segs[idLen+3] != "batch" {
			continue
		}
		last := segs[idLen-1]
		if !strings.HasSuffix(last, ".git") {
			continue
		}
		id := strings.Join(append(append([]string{}, segs[:idLen-1]...), strings.TrimSuffix(last, ".git")), "/")
		repo, err := ParseRepoID(id)
		if err != nil {
			return Route{}, false
		}
		return Route{Kind: RouteLFSBatch, RepoKind: kind, Repo: repo}, true
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
