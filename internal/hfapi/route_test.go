package hfapi

import "testing"

func TestParseRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want Route
		ok   bool
	}{
		// Repo info.
		{"/api/models/gpt2", Route{Kind: RouteRepoInfo, RepoKind: RepoKindModel, Repo: RepoID{Name: "gpt2"}}, true},
		{"/api/models/org/name", Route{Kind: RouteRepoInfo, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}}, true},
		{"/api/models/org/name/revision/main", Route{Kind: RouteRepoInfo, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "main"}, true},
		{"/api/models/gpt2/revision/main", Route{Kind: RouteRepoInfo, RepoKind: RepoKindModel, Repo: RepoID{Name: "gpt2"}, Revision: "main"}, true},
		// Escaped revision stays one segment and decodes.
		{"/api/models/org/name/revision/refs%2Fpr%2F1", Route{Kind: RouteRepoInfo, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "refs/pr/1"}, true},

		// Tree.
		{"/api/models/org/name/tree/main", Route{Kind: RouteTree, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "main"}, true},
		{"/api/models/org/name/tree/main/sub/dir", Route{Kind: RouteTree, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "main", Path: "sub/dir"}, true},
		{"/api/models/gpt2/tree/main", Route{Kind: RouteTree, RepoKind: RepoKindModel, Repo: RepoID{Name: "gpt2"}, Revision: "main"}, true},

		// Resolve.
		{"/org/name/resolve/main/config.json", Route{Kind: RouteResolve, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "main", Path: "config.json"}, true},
		{"/gpt2/resolve/main/config.json", Route{Kind: RouteResolve, RepoKind: RepoKindModel, Repo: RepoID{Name: "gpt2"}, Revision: "main", Path: "config.json"}, true},
		{"/org/name/resolve/main/vae/weights.safetensors", Route{Kind: RouteResolve, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "main", Path: "vae/weights.safetensors"}, true},
		{"/org/name/resolve/refs%2Fpr%2F1/f.txt", Route{Kind: RouteResolve, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "refs/pr/1", Path: "f.txt"}, true},

		// Datasets.
		{"/api/datasets/org/name", Route{Kind: RouteRepoInfo, RepoKind: RepoKindDataset, Repo: RepoID{Owner: "org", Name: "name"}}, true},
		{"/datasets/org/name/resolve/main/data.parquet", Route{Kind: RouteResolve, RepoKind: RepoKindDataset, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "main", Path: "data.parquet"}, true},

		// Write path.
		{"/api/models/org/name/preupload/main", Route{Kind: RoutePreupload, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "main"}, true},
		{"/api/models/gpt2/preupload/main", Route{Kind: RoutePreupload, RepoKind: RepoKindModel, Repo: RepoID{Name: "gpt2"}, Revision: "main"}, true},
		{"/api/models/org/name/commit/main", Route{Kind: RouteCommit, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "main"}, true},
		{"/api/models/org/name/commit/refs%2Fpr%2F1", Route{Kind: RouteCommit, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}, Revision: "refs/pr/1"}, true},
		{"/org/name.git/info/lfs/objects/batch", Route{Kind: RouteLFSBatch, RepoKind: RepoKindModel, Repo: RepoID{Owner: "org", Name: "name"}}, true},
		{"/gpt2.git/info/lfs/objects/batch", Route{Kind: RouteLFSBatch, RepoKind: RepoKindModel, Repo: RepoID{Name: "gpt2"}}, true},
		{"/datasets/org/name.git/info/lfs/objects/batch", Route{Kind: RouteLFSBatch, RepoKind: RepoKindDataset, Repo: RepoID{Owner: "org", Name: "name"}}, true},
		{"/org/name/info/lfs/objects/batch", Route{}, false}, // no .git suffix
		{"/org/name.git/info/lfs/objects", Route{}, false},   // truncated
		{"/api/models/org/name/preupload", Route{}, false},   // missing revision
		{"/api/models/org/name/commit/main/extra", Route{}, false},
		{"/api/models/org/name/tree", Route{}, false}, // tree without revision
		{"/api/models/gpt2/tree", Route{}, false},

		// Non-routes.
		{"/", Route{}, false},
		{"/api", Route{}, false},
		{"/api/spaces/org/name", Route{}, false},
		{"/api/models", Route{}, false},
		{"/org/name", Route{}, false},
		{"/org/name/resolve/main", Route{}, false},          // no file path
		{"/org/name/blob/main/config.json", Route{}, false}, // web UI path, not API
		{"/api/models/org/name/revision", Route{}, false},
		{"/api/models/a/b/c/d", Route{}, false},
		{"/api/models/org/name/revision/main/extra", Route{}, false},
		{"/api/models/../etc/passwd", Route{}, false},
		{"/api/models/org//name", Route{}, false},
	}
	for _, tc := range cases {
		got, ok := ParseRoute(tc.path)
		if ok != tc.ok {
			t.Errorf("ParseRoute(%q) ok = %v, want %v", tc.path, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRoute(%q) = %+v, want %+v", tc.path, got, tc.want)
		}
	}
}
