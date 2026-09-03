package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"time"
)

// RequestIDHeader is the HTTP header used for request correlation.
const RequestIDHeader = "X-Request-ID"

// maxRequestIDLen caps the accepted inbound header value length to keep it
// from being used as an unbounded log/echo sink.
const maxRequestIDLen = 128

type requestIDContextKey struct{}

// NewRequestID generates a fresh request ID: 128 bits of crypto/rand entropy
// encoded as 32 lowercase hex characters. If the entropy source fails it
// falls back to time/pid-derived bytes so callers always get a usable
// correlation ID.
func NewRequestID() string {
	buf := make([]byte, 16) // 128 bits
	if _, err := rand.Read(buf); err != nil {
		nano := time.Now().UnixNano()
		pid := os.Getpid()
		for i := 0; i < 8; i++ {
			shift := uint(i * 8)
			buf[i] = byte(nano >> shift)
			buf[8+i] = byte(nano>>shift) ^ byte(pid>>uint((i%4)*8))
		}
	}
	return hex.EncodeToString(buf)
}

// SanitizeRequestID validates an inbound request ID. It accepts non-empty
// values of at most 128 characters composed of ASCII letters, digits and the
// '-', '_', '.' separators. Anything else (including control characters,
// whitespace and over-long values) is rejected and replaced upstream with a
// freshly generated ID.
func SanitizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxRequestIDLen {
		return ""
	}
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '-' || r == '_' || r == '.':
		default:
			return ""
		}
	}
	return raw
}

// RequestID is middleware that establishes request correlation:
//
//   - an inbound X-Request-ID header is accepted when it passes
//     SanitizeRequestID, otherwise a new 128-bit hex ID is generated;
//   - the ID is stored in the request context (retrieve it with
//     RequestIDFromContext) and echoed back on the response;
//   - downstream middleware (e.g. WriteError, Recovery, MetricsMiddleware)
//     reads the same ID so logs, error payloads and metrics share one
//     correlation ID per request.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := SanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = NewRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDContextKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the correlation ID established by the
// RequestID middleware, or "" when absent.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(requestIDContextKey{}).(string); ok {
		return id
	}
	return ""
}
