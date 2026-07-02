package server

import (
	"context"
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

func (v *tokenValidator) put(token string, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Cap the cache so a spray of unique bad tokens cannot grow it
	// unboundedly; evicting everything is fine, entries repopulate.
	if len(v.entries) > 10_000 {
		v.entries = map[string]tokenVerdict{}
	}
	v.entries[token] = tokenVerdict{ok: ok, expires: time.Now().Add(v.ttl)}
}

// authorizeWrite gates write endpoints. mode "none" admits everything;
// mode "passthrough" requires a token upstream vouches for. On upstream
// outage it fails closed for writes (reads are unaffected).
func (s *Server) authorizeWrite(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Auth.Mode != "passthrough" {
		return true
	}
	token := bearerToken(r)
	if token == "" {
		writeHFError(w, http.StatusUnauthorized, "", "Invalid credentials in Authorization header")
		return false
	}
	ok, err := s.validateToken(r.Context(), token)
	if err != nil {
		writeHFError(w, http.StatusBadGateway, "", "Token validation against upstream failed.")
		return false
	}
	if !ok {
		writeHFError(w, http.StatusUnauthorized, "", "Invalid user token.")
		return false
	}
	return true
}

func (s *Server) validateToken(ctx context.Context, token string) (bool, error) {
	if verdict, found := s.tokens.get(token); found {
		return verdict.ok, nil
	}
	if s.upstream == nil {
		return false, nil
	}
	status, _, err := s.upstream.WhoAmI(ctx, token)
	if err != nil {
		return false, err
	}
	ok := status == http.StatusOK
	s.tokens.put(token, ok)
	return ok, nil
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
