// Package httpx holds HTTP building blocks shared by every service: the
// standard error envelope, middleware and health endpoints.
package httpx

import (
	"encoding/json"
	"net/http"
)

// Error codes used across services. Business errors are added next to the
// sentinel errors they map from.
const (
	CodeNotFound         = "NOT_FOUND"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodeInternal         = "INTERNAL_ERROR"
)

// ErrorBody is the error field of the standard error response:
//
//	{ "error": { "code": "...", "message": "...", "details": { ... } } }
type ErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type errorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The status line is already sent, so an encode error (in practice a
	// client that went away) cannot be reported to the client anyway.
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the standard error envelope.
func WriteError(w http.ResponseWriter, status int, body ErrorBody) {
	WriteJSON(w, status, errorEnvelope{Error: body})
}

// NotFound is the router fallback for unknown routes.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, http.StatusNotFound, ErrorBody{Code: CodeNotFound, Message: "Không tìm thấy tài nguyên"})
}

// MethodNotAllowed is the router fallback for known routes hit with the wrong
// method.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	WriteError(w, http.StatusMethodNotAllowed, ErrorBody{Code: CodeMethodNotAllowed, Message: "Phương thức không được hỗ trợ"})
}
