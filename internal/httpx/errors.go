package httpx

import (
	"encoding/json"
	"net/http"
)

// Stable error codes used by the structured JSON error model. These match the
// contract in api/openapi.yaml (components.schemas.Error) and
// docs/api-contract.md.
const (
	ErrorCodeBadRequest       = "bad_request"
	ErrorCodeUnauthorized     = "unauthorized"
	ErrorCodeForbidden        = "forbidden"
	ErrorCodeNotFound         = "not_found"
	ErrorCodeInternal         = "internal_error"
	ErrorCodeMethodNotAllowed = "method_not_allowed"
)

// ErrorDetail is the inner object of the error envelope.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// ErrorEnvelope is the wire shape for every JSON error emitted by AgentOS:
//
//	{"error":{"code":"not_found","message":"agent not found","request_id":"..."}}
type ErrorEnvelope struct {
	Error ErrorDetail `json:"error"`
}

// WriteError emits the structured JSON error model on w. The request_id field
// is populated from the RequestID middleware when available. It is safe to
// call with a nil request (request_id will be empty). Callers must not have
// written a response yet; WriteError is a single, terminal write.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	var requestID string
	if r != nil {
		requestID = RequestIDFromContext(r.Context())
	}
	body := ErrorEnvelope{
		Error: ErrorDetail{
			Code:      code,
			Message:   message,
			RequestID: requestID,
		},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// The response is terminal; a marshal failure of this fixed-shape struct
	// cannot realistically occur, and there is no meaningful recovery.
	_ = json.NewEncoder(w).Encode(body)
}

// ErrBadRequest writes a 400 bad_request error.
func ErrBadRequest(w http.ResponseWriter, r *http.Request, message string) {
	WriteError(w, r, http.StatusBadRequest, ErrorCodeBadRequest, message)
}

// ErrUnauthorized writes a 401 unauthorized error.
func ErrUnauthorized(w http.ResponseWriter, r *http.Request, message string) {
	WriteError(w, r, http.StatusUnauthorized, ErrorCodeUnauthorized, message)
}

// ErrForbidden writes a 403 forbidden error.
func ErrForbidden(w http.ResponseWriter, r *http.Request, message string) {
	WriteError(w, r, http.StatusForbidden, ErrorCodeForbidden, message)
}

// ErrNotFound writes a 404 not_found error.
func ErrNotFound(w http.ResponseWriter, r *http.Request, message string) {
	WriteError(w, r, http.StatusNotFound, ErrorCodeNotFound, message)
}

// ErrMethodNotAllowed writes a 405 method_not_allowed error.
func ErrMethodNotAllowed(w http.ResponseWriter, r *http.Request, message string) {
	WriteError(w, r, http.StatusMethodNotAllowed, ErrorCodeMethodNotAllowed, message)
}

// ErrInternal writes a 500 internal_error error.
func ErrInternal(w http.ResponseWriter, r *http.Request, message string) {
	WriteError(w, r, http.StatusInternalServerError, ErrorCodeInternal, message)
}
