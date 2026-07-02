package relay

import (
	"fmt"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/loewenthal-corp/shpiel/internal/backend"
	"github.com/loewenthal-corp/shpiel/internal/config"
	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// Route pairs a repo-id glob with resolved backend instances.
type Route struct {
	Match    string
	Primary  backend.Backend
	Replicas []backend.Backend
}

// Router matches repo ids against ordered glob routes (first match wins).
type Router struct {
	routes []Route
}

// NewRouter resolves route config against instantiated backends.
func NewRouter(routes []config.Route, backends map[string]backend.Backend) (*Router, error) {
	r := &Router{}
	for i, rc := range routes {
		if !doublestar.ValidatePattern(rc.Match) {
			return nil, fmt.Errorf("routes[%d]: invalid glob %q", i, rc.Match)
		}
		primary, ok := backends[rc.Primary]
		if !ok {
			return nil, fmt.Errorf("routes[%d]: unknown backend %q", i, rc.Primary)
		}
		route := Route{Match: rc.Match, Primary: primary}
		for _, name := range rc.Replicas {
			rep, ok := backends[name]
			if !ok {
				return nil, fmt.Errorf("routes[%d]: unknown replica backend %q", i, name)
			}
			route.Replicas = append(route.Replicas, rep)
		}
		r.routes = append(r.routes, route)
	}
	return r, nil
}

// For returns the first route matching the repo id, or nil when no route
// matches. Globs follow doublestar semantics with "/" separating owner and
// name (so "exigence/*" matches every repo in that namespace); a bare "*"
// is special-cased as match-everything so the obvious catch-all works.
func (r *Router) For(repo hfapi.RepoID) *Route {
	id := repo.String()
	for i := range r.routes {
		if r.routes[i].Match == "*" {
			return &r.routes[i]
		}
		if ok, _ := doublestar.Match(r.routes[i].Match, id); ok {
			return &r.routes[i]
		}
	}
	return nil
}
