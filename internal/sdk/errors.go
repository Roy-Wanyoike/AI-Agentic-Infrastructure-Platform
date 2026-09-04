package sdk

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ValidationError is one item of a 422 {"errors":[…]} validation body
// (workflow DSL validation is the producer; see cmd/api/workflows.go).
type ValidationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	NodeID  string `json:"node_id,omitempty"`
}

// APIError is returned for every non-2xx response. It unifies the four
// body styles the API emits:
//
//  1. {"error":{"code":"…","message":"…"}} — the structured envelope used by
//     the workflows/knowledge/usage/tools/… handlers;
//  2. {"errors":[{code,message,node_id},…]} — the 422 validation array;
//  3. the SCIM 2.0 error document (application/scim+json, RFC 7644 §3.12:
//     {"schemas":[…Error],"status":"…","detail":"…"}) emitted by the
//     /scim/v2/* endpoints;
//  4. a plain-text body from the legacy http.Error handlers (agents, runs).
//
// Code and Message are best-effort: plain-text bodies land in Message with an
// empty Code; unparseable bodies keep the HTTP status line in Message.
type APIError struct {
	StatusCode       int               `json:"status_code"`
	Status           string            `json:"status"`
	Code             string            `json:"code,omitempty"`
	Message          string            `json:"message"`
	ValidationErrors []ValidationError `json:"validation_errors,omitempty"`
	RawBody          string            `json:"-"`
}

// Error implements the error interface. 422 validation failures are rendered
// item by item so users see every violated rule.
func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s", e.headline())
	switch {
	case len(e.ValidationErrors) > 0:
		b.WriteString(":")
		for _, ve := range e.ValidationErrors {
			b.WriteString("\n  - ")
			if ve.Code != "" {
				fmt.Fprintf(&b, "[%s] ", ve.Code)
			}
			b.WriteString(ve.Message)
			if ve.NodeID != "" {
				fmt.Fprintf(&b, " (node: %s)", ve.NodeID)
			}
		}
	default:
		if e.Message != "" {
			fmt.Fprintf(&b, ": %s", e.Message)
		}
	}
	return b.String()
}

// headline renders the status half of the message, including the machine
// error code when the server supplied one.
func (e *APIError) headline() string {
	code := e.Code
	if code == "" {
		return strings.TrimSpace(e.Status)
	}
	return fmt.Sprintf("%s (%s)", strings.TrimSpace(e.Status), code)
}

// IsStatus reports whether the error carries the given HTTP status code.
func (e *APIError) IsStatus(code int) bool { return e != nil && e.StatusCode == code }

// newAPIError parses an error response body into an APIError.
func newAPIError(resp *http.Response, raw []byte) *APIError {
	e := &APIError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		RawBody:    string(raw),
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		e.Message = resp.Status
		return e
	}
	// Structured envelope first: {"error":{"code","message"}}.
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Errors []ValidationError `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		if env.Error != nil {
			e.Code = env.Error.Code
			e.Message = env.Error.Message
			return e
		}
		if len(env.Errors) > 0 {
			e.StatusCode = firstNonZero(resp.StatusCode, 422)
			e.Message = "validation failed"
			e.ValidationErrors = env.Errors
			return e
		}
	}
	// SCIM 2.0 error document (the /scim/v2/* endpoints; RFC 7644 §3.12):
	// the machine-readable status duplicates the HTTP status, so only the
	// human detail is carried over.
	var scimErr struct {
		Schemas []string `json:"schemas"`
		Status  string   `json:"status"`
		Detail  string   `json:"detail"`
	}
	if err := json.Unmarshal(raw, &scimErr); err == nil &&
		len(scimErr.Schemas) == 1 && strings.HasSuffix(scimErr.Schemas[0], ":Error") {
		if strings.TrimSpace(scimErr.Detail) != "" {
			e.Message = strings.TrimSpace(scimErr.Detail)
			return e
		}
	}
	// Legacy plain-text bodies (http.Error) and anything unparseable.
	e.Message = body
	return e
}

func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}
