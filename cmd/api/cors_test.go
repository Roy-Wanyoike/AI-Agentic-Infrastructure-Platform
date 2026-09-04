package main

// Tests for the issue #55 CORS allowlist logic (cors.go): the parse matrix
// (pure), the match matrix (pure) and the header-application matrix both
// through the pure function and through the real corsMiddleware handler.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseCORSOrigins(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"unset equivalent (empty string)", "", nil},
		{"whitespace only", "   ", nil},
		{"commas only", ",,,", nil},
		{"single origin", "https://app.example.com", []string{"https://app.example.com"}},
		{"single origin padded", "  https://app.example.com  ", []string{"https://app.example.com"}},
		{"multi origin", "https://a.test,https://b.test", []string{"https://a.test", "https://b.test"}},
		{"multi origin with spaces", " https://a.test , https://b.test , https://c.test ",
			[]string{"https://a.test", "https://b.test", "https://c.test"}},
		// Sloppy values only ever SHRINK the allowlist: blanks are dropped.
		{"trailing comma", "https://a.test,", []string{"https://a.test"}},
		{"interior blanks", "https://a.test,, ,https://b.test", []string{"https://a.test", "https://b.test"}},
		{"blank equivalent of commas+spaces", " , ,, ", nil},
		// Entries are never transformed (no trailing-slash stripping, no
		// lowercasing): exact-match contract.
		{"trailing slash kept", "https://a.test/", []string{"https://a.test/"}},
		{"case kept", "HTTPS://A.TEST", []string{"HTTPS://A.TEST"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseCORSOrigins(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseCORSOrigins(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseCORSOrigins(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCORSOriginAllowed(t *testing.T) {
	allowed := []string{"https://app.example.com", "https://staging.example.com"}
	cases := []struct {
		name   string
		origin string
		want   bool
	}{
		{"exact match", "https://app.example.com", true},
		{"second entry", "https://staging.example.com", true},
		{"no origin header (same-origin request)", "", false},
		{"unknown origin", "https://evil.example.net", false},
		{"case mismatch fails closed", "https://APP.example.com", false},
		{"trailing slash mismatch fails closed", "https://app.example.com/", false},
		{"suffix mismatch", "https://app.example.com.evil.test", false},
		{"prefix mismatch", "https://app.example.com:8443", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CORSOriginAllowed(allowed, tc.origin); got != tc.want {
				t.Fatalf("CORSOriginAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}
	// Empty allowlist allows nothing (callers fall back to wildcard mode, but
	// the pure matcher itself must never open up).
	for _, origin := range []string{"", "https://app.example.com", "*"} {
		if CORSOriginAllowed(nil, origin) {
			t.Fatalf("empty allowlist must allow nothing, allowed %q", origin)
		}
	}
}

func TestApplyCORSHeaders(t *testing.T) {
	allowed := []string{"https://app.example.com"}

	t.Run("allowed origin gets the full credentials-safe grant set", func(t *testing.T) {
		h := http.Header{}
		ApplyCORSHeaders(h, allowed, "https://app.example.com")
		if got := h.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
			t.Fatalf("ACAO = %q, want the exact echoed origin", got)
		}
		if got := h.Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Fatalf("Allow-Credentials = %q, want true (safe: echo is never *)", got)
		}
		if got := h.Get("Access-Control-Allow-Methods"); got != corsAllowMethods {
			t.Fatalf("Allow-Methods = %q, want %q", got, corsAllowMethods)
		}
		if got := h.Get("Access-Control-Allow-Headers"); got != corsAllowHeaders {
			t.Fatalf("Allow-Headers = %q, want %q", got, corsAllowHeaders)
		}
		if got := h.Values("Vary"); len(got) == 0 {
			t.Fatalf("Vary: Origin must always be set in allowlist mode")
		}
	})

	t.Run("non-matching origin gets NO ACAO header", func(t *testing.T) {
		h := http.Header{}
		ApplyCORSHeaders(h, allowed, "https://evil.example.net")
		if got := h.Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("ACAO must be absent for non-allowlisted origins, got %q", got)
		}
		if got := h.Get("Access-Control-Allow-Credentials"); got != "" {
			t.Fatalf("Allow-Credentials must be absent for non-allowlisted origins, got %q", got)
		}
		if got := h.Values("Vary"); len(got) == 0 {
			t.Fatalf("Vary: Origin must be set even for non-matching origins (cache correctness)")
		}
	})

	t.Run("empty origin (same-origin / curl) gets no grants", func(t *testing.T) {
		h := http.Header{}
		ApplyCORSHeaders(h, allowed, "")
		if got := h.Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("ACAO must be absent without an Origin header, got %q", got)
		}
	})
}

// TestCorsMiddlewareMatrix drives the REAL corsMiddleware (the wiring
// main.go routes() uses) through both modes.
func TestCorsMiddlewareMatrix(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("upstream"))
	})

	t.Run("unset env keeps the exact wildcard dev behavior", func(t *testing.T) {
		t.Setenv(CORSOriginsEnvVar, "")
		h := corsMiddleware(next)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://anything.test")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("wildcard mode ACAO = %q, want * (dev behavior unchanged)", got)
		}
		if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Fatalf("wildcard mode must stay credentials-safe (no Allow-Credentials), got %q", got)
		}
	})

	t.Run("allowlist mode echoes only allowed origins", func(t *testing.T) {
		t.Setenv(CORSOriginsEnvVar, "https://app.example.com, https://staging.example.com")
		h := corsMiddleware(next)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://app.example.com")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
			t.Fatalf("ACAO = %q, want exact echo of the allowlisted origin", got)
		}
		if rr.Body.String() != "upstream" {
			t.Fatalf("non-preflight request must reach the upstream handler, body=%q", rr.Body.String())
		}

		req = httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://evil.example.net")
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("non-matching origin must get no ACAO header, got %q", got)
		}
		if rr.Body.String() != "upstream" {
			t.Fatalf("CORS is a browser concern: the response still renders, body=%q", rr.Body.String())
		}
	})

	t.Run("preflight OPTIONS short-circuits with 204 in both modes", func(t *testing.T) {
		t.Setenv(CORSOriginsEnvVar, "")
		rr := httptest.NewRecorder()
		corsMiddleware(next).ServeHTTP(rr, httptest.NewRequest(http.MethodOptions, "/", nil))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("wildcard preflight = %d, want 204", rr.Code)
		}

		t.Setenv(CORSOriginsEnvVar, "https://app.example.com")
		req := httptest.NewRequest(http.MethodOptions, "/", nil)
		req.Header.Set("Origin", "https://app.example.com")
		rr = httptest.NewRecorder()
		corsMiddleware(next).ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("allowlist preflight = %d, want 204", rr.Code)
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
			t.Fatalf("allowlist preflight ACAO = %q, want the echoed origin", got)
		}

		req = httptest.NewRequest(http.MethodOptions, "/", nil)
		req.Header.Set("Origin", "https://evil.example.net")
		rr = httptest.NewRecorder()
		corsMiddleware(next).ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("disallowed preflight = %d, want 204 (browser still rejects on missing ACAO)", rr.Code)
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("disallowed preflight must not carry ACAO, got %q", got)
		}
	})
}
