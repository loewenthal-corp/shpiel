package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/hfapi"
)

// tokenValidator implements passthrough auth: a caller's Bearer token is
// valid if upstream's whoami-v2 accepts it. Verdicts are cached with a TTL
// so a fleet pushing files doesn't hammer upstream.
type tokenValidator struct {
	ttl time.Duration
	mu  sync.Mutex
	// entries maps token -> verdict; both accepts and rejects are cached
	// (a bad token retried in a tight loop should not reach upstream).
	entries map[string]tokenVerdict
}

type tokenVerdict struct {
	ok      bool
	name    string // identity from upstream whoami, for audit records
	expires time.Time
}

func newTokenValidator(ttl time.Duration) *tokenValidator {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &tokenValidator{ttl: ttl, entries: map[string]tokenVerdict{}}
}

func (v *tokenValidator) get(token string) (verdict tokenVerdict, found bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	verdict, found = v.entries[token]
	if found && time.Now().After(verdict.expires) {
		delete(v.entries, token)
		return tokenVerdict{}, false
	}
	return verdict, found
}

func (v *tokenValidator) put(token string, ok bool, name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Cap the cache so a spray of unique bad tokens cannot grow it
	// unboundedly; evicting everything is fine, entries repopulate.
	if len(v.entries) > 10_000 {
		v.entries = map[string]tokenVerdict{}
	}
	v.entries[token] = tokenVerdict{ok: ok, name: name, expires: time.Now().Add(v.ttl)}
}

// authorizeWrite gates write endpoints and returns the authenticated actor
// for audit records. mode "none" admits everything anonymously; mode
// "passthrough" requires a token upstream vouches for and yields the
// upstream identity. On upstream outage it fails closed for writes (reads
// are unaffected).
func (s *Server) authorizeWrite(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.cfg.Auth.Mode != "passthrough" {
		return "anonymous", true
	}
	token := bearerToken(r)
	if token == "" {
		writeHFError(w, http.StatusUnauthorized, "", "Invalid credentials in Authorization header")
		return "", false
	}
	ok, name, err := s.validateToken(r.Context(), token)
	if err != nil {
		writeHFError(w, http.StatusBadGateway, "", "Token validation against upstream failed.")
		return "", false
	}
	if !ok {
		writeHFError(w, http.StatusUnauthorized, "", "Invalid user token.")
		return "", false
	}
	return name, true
}

func (s *Server) validateToken(ctx context.Context, token string) (bool, string, error) {
	if verdict, found := s.tokens.get(token); found {
		return verdict.ok, verdict.name, nil
	}
	if s.upstream == nil {
		return false, "", nil
	}
	status, body, err := s.upstream.WhoAmI(ctx, token)
	if err != nil {
		return false, "", err
	}
	ok := status == http.StatusOK
	name := ""
	if ok {
		var who struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &who); err == nil {
			name = who.Name
		}
	}
	s.tokens.put(token, ok, name)
	return ok, name, nil
}

// repoKindFromType maps the "type" field of repo create/delete payloads.
func repoKindFromType(t string) (hfapi.RepoKind, bool) {
	switch t {
	case "", "model":
		return hfapi.RepoKindModel, true
	case "dataset":
		return hfapi.RepoKindDataset, true
	default:
		return "", false
	}
}
