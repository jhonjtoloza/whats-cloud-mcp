// Package apierr defines the single JSON error shape every HTTP surface of the
// gateway returns, so clients only have to parse one thing.
package apierr

import (
	"encoding/json"
	"net/http"
)

// Machine-readable error codes. Clients should branch on these, not on prose.
const (
	CodeUnauthorized   = "unauthorized"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeInvalidRequest = "invalid_request"
	CodeConflict       = "conflict"
	CodeInternal       = "internal_error"
	CodeUnavailable    = "unavailable"
	// CodeGone means the thing existed and no longer does. It is deliberately
	// distinct from CodeUnavailable, which invites a retry: WhatsApp expires
	// media server-side, and a caller told to retry a file that is gone for
	// good would keep asking forever.
	CodeGone = "gone"
	// CodeTooLarge means the request would move more bytes than the gateway
	// is configured to handle.
	CodeTooLarge = "too_large"
	// CodeUnsupportedType means the gateway does not handle that media type.
	CodeUnsupportedType = "unsupported_media_type"
)

// Body is the wire representation of an error response.
type Body struct {
	Error Detail `json:"error"`
}

// Detail carries the machine-readable code and a human-readable message.
type Detail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Write renders an error response. It never leaks internal details: callers are
// responsible for passing a message that is safe to show to a client.
func Write(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Body{Error: Detail{Code: code, Message: message}})
}

// Unauthorized writes a 401 with a WWW-Authenticate challenge.
func Unauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="whats-cloud-mcp"`)
	Write(w, http.StatusUnauthorized, CodeUnauthorized, message)
}

// Forbidden writes a 403.
func Forbidden(w http.ResponseWriter, message string) {
	Write(w, http.StatusForbidden, CodeForbidden, message)
}

// NotFound writes a 404.
func NotFound(w http.ResponseWriter, message string) {
	Write(w, http.StatusNotFound, CodeNotFound, message)
}

// InvalidRequest writes a 400.
func InvalidRequest(w http.ResponseWriter, message string) {
	Write(w, http.StatusBadRequest, CodeInvalidRequest, message)
}

// Internal writes a 500 with a fixed, information-free message.
func Internal(w http.ResponseWriter) {
	Write(w, http.StatusInternalServerError, CodeInternal, "internal error")
}
