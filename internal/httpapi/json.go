package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/apierr"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

// maxRequestBody caps how much a client can push into a single request.
const maxRequestBody = 1 << 20 // 1 MiB

// errEmptyBody signals a request with no payload at all.
var errEmptyBody = errors.New("httpapi: empty request body")

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// decodeJSON reads a JSON body into dst. It returns errEmptyBody when the
// request carries no payload, which optional-body endpoints may ignore.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	err := json.NewDecoder(r.Body).Decode(dst)
	if errors.Is(err, io.EOF) {
		return errEmptyBody
	}
	return err
}

// queryLimit reads a ?limit= parameter. Anything unparseable falls back to the
// repository default rather than failing the request.
func queryLimit(r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return limit
}

// writeWAError maps an engine failure onto the right status code.
func writeWAError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, wa.ErrNotPaired):
		apierr.Write(w, http.StatusConflict, apierr.CodeConflict, "the tenant has no paired WhatsApp session")
		return true
	case errors.Is(err, wa.ErrUnknownTenant), errors.Is(err, store.ErrNotFound):
		apierr.NotFound(w, "session not found for this tenant")
		return true
	case errors.Is(err, wa.ErrInvalidJID):
		apierr.InvalidRequest(w, "the destination is not a valid WhatsApp JID")
		return true
	default:
		return false
	}
}
