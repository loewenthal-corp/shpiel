package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/loewenthal-corp/shpiel/internal/hfapi"
	"github.com/loewenthal-corp/shpiel/internal/relay"
	"github.com/loewenthal-corp/shpiel/internal/upstream"
)

// writeHFError emits an HF-shaped error: JSON {"error": ...} plus the
// X-Error-Code header that huggingface_hub keys its typed exceptions on.
func writeHFError(w http.ResponseWriter, status int, code, msg string) {
	if code != "" {
		w.Header().Set(hfapi.HeaderErrorCode, code)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(hfapi.ErrorResponse{Error: msg})
}

// writeRelayError maps relay/upstream errors onto HF error semantics.
func writeRelayError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, relay.ErrRepoNotFound), errors.Is(err, relay.ErrNoRoute):
		// Matching the Hub: unknown and unauthorized repos are
		// indistinguishable; the error-code header is what clients key on.
		writeHFError(w, http.StatusNotFound, hfapi.ErrorCodeRepoNotFound,
			"Repository not found. If the repo is private or gated, make sure you are authenticated.")
	case errors.Is(err, relay.ErrRevisionNotFound):
		writeHFError(w, http.StatusNotFound, hfapi.ErrorCodeRevisionNotFound, "Revision not found.")
	case errors.Is(err, relay.ErrEntryNotFound):
		writeHFError(w, http.StatusNotFound, hfapi.ErrorCodeEntryNotFound, "Entry not found.")
	case errors.Is(err, upstream.ErrUnauthorized):
		writeHFError(w, http.StatusUnauthorized, "", "Unauthorized by upstream.")
	default:
		writeHFError(w, http.StatusInternalServerError, "", "Internal error.")
	}
}
