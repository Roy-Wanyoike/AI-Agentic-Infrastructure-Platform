package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- RequestID -------------------------------------------------------------

func TestRequestIDGeneratesAndEchoes(t *testing.T) {
	var seen string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get("X-Request-ID")
	if got == "" {
		t.Fatal("expected X-Request-ID on response")
	}
	if len(got) != 32 {
		t.Fatalf("expected 128-bit hex ID (32 chars), got %d chars: %q", len(got), got)
	}
	for _, c := range got {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			t.Fatalf("expected lowercase hex ID, got %q", got)
		}
	}
	if seen != got {
		t.Fatalf("context helper returned %q, response header was %q", seen, got)
	}

	// A second request must get a distinct ID.
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/agents", nil))
	if rec2.Header().Get("X-Request-ID") == got {
		t.Fatal("expected distinct generated request IDs across requests")
	}
}

func TestRequestIDAcceptsIncomingHeader(t *testing.T) {
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := RequestIDFromContext(r.Context()); got != "abc-123_XYZ.9" {
			t.Fatalf("expected incoming ID in context, got %q", got)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "abc-123_XYZ.9")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got != "abc-123_XYZ.9" {
		t.Fatalf("expected incoming ID echoed back, got %q", got)
	}
}

func TestRequestIDRejectsInvalidIncomingHeader(t *testing.T) {
	cases := map[string]string{
		"control chars": "bad\r\nid",
		"spaces":        "has space",
		"too long":      strings.Repeat("a", maxRequestIDLen+1),
		"unicode":       "requêst",
	}
	for name, invalid := range cases {
		t.Run(name, func(t *testing.T) {
			handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Request-ID", invalid)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			got := rec.Header().Get("X-Request-ID")
			if got == invalid || got == "" {
				t.Fatalf("expected a freshly generated ID for invalid input %q, got %q", invalid, got)
			}
			if len(got) != 32 {
				t.Fatalf("expected generated 32-char hex ID, got %q", got)
			}
		})
	}
}

func TestRequestIDFromContextWithoutMiddleware(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty ID outside middleware, got %q", got)
	}
	if got := RequestIDFromContext(nil); got != "" {
		t.Fatalf("expected empty ID for nil context, got %q", got)
	}
}

// --- ErrorModel -------------------------------------------------------------

func TestWriteErrorBodyShape(t *testing.T) {
	// RequestID is outermost so the handler sees the correlation ID.
	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ErrNotFound(w, r, "agent not found")
		}),
		RequestID,
	)
	req := httptest.NewRequest(http.MethodGet, "/v1/agents/agent-42", nil)
	req.Header.Set("X-Request-ID", "corr-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected application/json content type, got %q", ct)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "corr-1" {
		t.Fatalf("expected X-Request-ID echoed, got %q", got)
	}

	var envelope struct {
		Error ErrorDetail `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("error body is not valid JSON: %v — body: %s", err, rec.Body.String())
	}
	if envelope.Error.Code != "not_found" || envelope.Error.Message != "agent not found" {
		t.Fatalf("unexpected error payload: %+v", envelope.Error)
	}
	if envelope.Error.RequestID != "corr-1" {
		t.Fatalf("expected request_id %q in body, got %q", "corr-1", envelope.Error.RequestID)
	}
}

func TestWriteErrorWithoutRequestID(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodPost, "/", nil), http.StatusBadRequest, ErrorCodeBadRequest, "invalid request body")
	var envelope ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("error body is not valid JSON: %v", err)
	}
	if envelope.Error.RequestID != "" {
		t.Fatalf("expected empty request_id without middleware, got %q", envelope.Error.RequestID)
	}
	// Schema requires the key to be present even when empty.
	if !strings.Contains(rec.Body.String(), `"request_id"`) {
		t.Fatalf("expected request_id key present in body, got %s", rec.Body.String())
	}
}

func TestErrorHelpersStatusAndCode(t *testing.T) {
	cases := []struct {
		name     string
		call     func(w http.ResponseWriter, r *http.Request, msg string)
		status   int
		wantCode string
	}{
		{"bad request", ErrBadRequest, http.StatusBadRequest, ErrorCodeBadRequest},
		{"unauthorized", ErrUnauthorized, http.StatusUnauthorized, ErrorCodeUnauthorized},
		{"forbidden", ErrForbidden, http.StatusForbidden, ErrorCodeForbidden},
		{"not found", ErrNotFound, http.StatusNotFound, ErrorCodeNotFound},
		{"method not allowed", ErrMethodNotAllowed, http.StatusMethodNotAllowed, ErrorCodeMethodNotAllowed},
		{"internal", ErrInternal, http.StatusInternalServerError, ErrorCodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.call(rec, httptest.NewRequest(http.MethodGet, "/", nil), "boom")
			if rec.Code != tc.status {
				t.Fatalf("expected status %d, got %d", tc.status, rec.Code)
			}
			var envelope ErrorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			if envelope.Error.Code != tc.wantCode {
				t.Fatalf("expected code %q, got %q", tc.wantCode, envelope.Error.Code)
			}
		})
	}
}

// --- Recovery ---------------------------------------------------------------

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestRecoveryConvertsPanicTo500(t *testing.T) {
	var logBuf bytes.Buffer
	// RequestID outermost (Chain: first arg = outermost) so Recovery's
	// deferred error write carries the correlation ID.
	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		}),
		RequestID,
		Recovery(newTestLogger(&logBuf)),
	)
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("X-Request-ID", "panic-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	var envelope ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("expected JSON error body, got %q (%v)", rec.Body.String(), err)
	}
	if envelope.Error.Code != ErrorCodeInternal {
		t.Fatalf("expected internal_error, got %q", envelope.Error.Code)
	}
	if envelope.Error.RequestID != "panic-1" {
		t.Fatalf("expected request_id panic-1, got %q", envelope.Error.RequestID)
	}
	if !strings.Contains(logBuf.String(), "panic recovered") {
		t.Fatalf("expected panic log record, log: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "panic-1") {
		t.Fatalf("expected request_id in log record, log: %s", logBuf.String())
	}
}

func TestRecoveryRepanicsErrAbortHandler(t *testing.T) {
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("expected http.ErrAbortHandler to be re-panicked, got %v", p)
		}
	}()
	handler := Recovery(newTestLogger(&bytes.Buffer{}))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic(http.ErrAbortHandler)
		}),
	)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRecoveryDoesNotDoubleWriteAfterCommit(t *testing.T) {
	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial"))
			panic("too late")
		}),
		Recovery(nil), // nil logger falls back to slog.Default()
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected committed 200 to survive, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != "partial" {
		t.Fatalf("expected body untouched, got %q", body)
	}
}

// --- CORS --------------------------------------------------------------------

func TestCORSPreflight(t *testing.T) {
	opts := DefaultCORSOptions()
	opts.AllowedOrigins = []string{"https://app.agentos.dev"}
	handler := CORS(opts)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("preflight must not reach the wrapped handler")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/agents", nil)
	req.Header.Set("Origin", "https://app.agentos.dev")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "Content-Type,Authorization,X-API-Key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 preflight response, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.agentos.dev" {
		t.Fatalf("expected exact origin echo, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, http.MethodPost) || !strings.Contains(got, http.MethodDelete) {
		t.Fatalf("expected POST and DELETE in allow-methods, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") || !strings.Contains(got, "X-API-Key") {
		t.Fatalf("expected Authorization and X-API-Key in allow-headers, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "86400" {
		t.Fatalf("expected max-age 86400, got %q", got)
	}
	vary := rec.Header().Values("Vary")
	joined := strings.Join(vary, ", ")
	if !strings.Contains(joined, "Origin") || !strings.Contains(joined, "Access-Control-Request-Method") {
		t.Fatalf("expected Vary headers on preflight, got %v", vary)
	}
}

func TestCORSWildcardDefault(t *testing.T) {
	handler := CORS(CORSOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://random.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected non-preflight to pass through, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected wildcard ACAO by default, got %q", got)
	}
}

func TestCORSRejectsUnknownOrigin(t *testing.T) {
	handler := CORS(CORSOptions{AllowedOrigins: []string{"https://app.agentos.dev"}})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected no ACAO for disallowed origin, got %q", got)
	}
}

func TestCORSCredentialsWithWildcardEchoesOrigin(t *testing.T) {
	handler := CORS(CORSOptions{AllowCredentials: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.agentos.dev")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.agentos.dev" {
		t.Fatalf("credentials require exact origin echo, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("expected credentials header, got %q", got)
	}
}

func TestCORSPlainOptionsPassesThrough(t *testing.T) {
	handler := CORS(DefaultCORSOptions())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	// OPTIONS without Access-Control-Request-Method is not a CORS preflight.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected plain OPTIONS to pass through, got %d", rec.Code)
	}
}

// --- Timeout -----------------------------------------------------------------

func TestTimeoutReturns503OnSlowHandler(t *testing.T) {
	handler := Timeout(20 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on timeout, got %d", rec.Code)
	}
}

func TestTimeoutPassesFastHandlerThrough(t *testing.T) {
	handler := Timeout(1 * time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected pass-through, got %d", rec.Code)
	}
}

func TestTimeoutDisabledWithNonPositiveDuration(t *testing.T) {
	handler := Timeout(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected disabled timeout to pass through, got %d", rec.Code)
	}
}

// --- Chain --------------------------------------------------------------------

func TestChainAppliesMiddlewaresOutermostFirst(t *testing.T) {
	var order []string
	mk := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "handler")
		}),
		mk("first"),
		mk("second"),
	)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Join(order, ",") != "first,second,handler" {
		t.Fatalf("unexpected execution order: %v", order)
	}
}
