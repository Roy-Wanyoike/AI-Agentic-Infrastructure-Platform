package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPRequestToolSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Header", "agentos")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	tool := NewHTTPRequestToolAllowPrivate()
	result, err := tool.Execute(map[string]any{"url": server.URL})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got, ok := result["status"].(int); !ok || got != http.StatusOK {
		t.Fatalf("expected status 200, got %#v", result["status"])
	}
	if body, _ := result["body"].(string); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("unexpected body %q", body)
	}
	headers, _ := result["headers"].(map[string][]string)
	if headers == nil || len(headers["X-Test-Header"]) != 1 || headers["X-Test-Header"][0] != "agentos" {
		t.Fatalf("expected response headers captured, got %#v", result["headers"])
	}
}

func TestHTTPRequestToolDefaultGetAndPostBody(t *testing.T) {
	var seenMethod, seenBody, seenContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		seenBody = string(buf[:n])
		seenContentType = r.Header.Get("Content-Type")
		fmt.Fprint(w, seenBody)
	}))
	defer server.Close()

	tool := NewHTTPRequestToolAllowPrivate()
	result, err := tool.Execute(map[string]any{
		"url":    server.URL,
		"method": "post",
		"body":   `{"hello":"world"}`,
		"headers": map[string]any{
			"Content-Type": "application/json",
		},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if seenMethod != http.MethodPost {
		t.Fatalf("expected POST, got %s", seenMethod)
	}
	if seenBody != `{"hello":"world"}` {
		t.Fatalf("expected body forwarded, got %q", seenBody)
	}
	if seenContentType != "application/json" {
		t.Fatalf("expected content-type header forwarded, got %q", seenContentType)
	}
	if body, _ := result["body"].(string); body != `{"hello":"world"}` {
		t.Fatalf("expected echoed body, got %q", body)
	}
}

func TestHTTPRequestToolInvalidInput(t *testing.T) {
	tool := NewHTTPRequestToolAllowPrivate()
	cases := []struct {
		name   string
		input  map[string]any
		wantIs error
	}{
		{"missing url", map[string]any{}, ErrInvalidToolInput},
		{"empty url", map[string]any{"url": "   "}, ErrInvalidToolInput},
		{"file scheme", map[string]any{"url": "file:///etc/passwd"}, ErrInvalidToolInput},
		{"ftp scheme", map[string]any{"url": "ftp://example.com/file"}, ErrInvalidToolInput},
		{"missing host", map[string]any{"url": "http:///only-path"}, ErrInvalidToolInput},
		{"bad method", map[string]any{"url": "http://example.com", "method": "TRACE"}, ErrInvalidToolInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Execute(tc.input)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("expected error %v, got %v", tc.wantIs, err)
			}
		})
	}
}

func TestHTTPRequestToolBlocksPrivateAndMetadataTargets(t *testing.T) {
	tool := NewHTTPRequestTool()
	cases := []struct {
		name string
		url  string
	}{
		{"loopback ipv4", "http://127.0.0.1:8080/admin"},
		{"loopback name", "http://localhost:8080/admin"},
		{"subdomain localhost", "http://api.localhost/admin"},
		{"unspecified", "http://0.0.0.0/admin"},
		{"metadata endpoint", "http://169.254.169.254/latest/meta-data/"},
		{"link local", "http://169.254.10.20/"},
		{"rfc1918 10/8", "http://10.1.2.3/"},
		{"rfc1918 172.16/12", "http://172.16.0.9/"},
		{"rfc1918 192.168/16", "http://192.168.1.1/router"},
		{"cgnat", "http://100.100.1.1/"},
		{"ipv6 loopback", "http://[::1]:9000/"},
		{"ipv6 link local", "http://[fe80::1]/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Execute(map[string]any{"url": tc.url})
			if err == nil {
				t.Fatalf("expected SSRF rejection for %s", tc.url)
			}
			if !errors.Is(err, ErrBlockedHost) {
				t.Fatalf("expected ErrBlockedHost for %s, got %v", tc.url, err)
			}
		})
	}
}

func TestHTTPRequestToolAllowsPrivateWhenConfigured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "private-ok")
	}))
	defer server.Close()

	tool := NewHTTPRequestToolAllowPrivate()
	result, err := tool.Execute(map[string]any{"url": server.URL})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if body, _ := result["body"].(string); body != "private-ok" {
		t.Fatalf("expected private access to succeed, got %q", body)
	}
}

func TestHTTPRequestToolTimeout(t *testing.T) {
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
	}))
	defer server.Close()
	defer close(released)

	tool := NewHTTPRequestToolAllowPrivate()
	start := time.Now()
	_, err := tool.Execute(map[string]any{"url": server.URL, "timeout_ms": 50})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout not enforced promptly: %s", elapsed)
	}
}

func TestHTTPRequestToolRespectsCallerContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should never be read")
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tool := NewHTTPRequestToolAllowPrivate()
	_, err := tool.ExecuteContext(ctx, map[string]any{"url": server.URL})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestHTTPRequestToolNon2xxStatusIsAResultNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	tool := NewHTTPRequestToolAllowPrivate()
	result, err := tool.Execute(map[string]any{"url": server.URL})
	if err != nil {
		t.Fatalf("4xx responses are observations, not tool errors: %v", err)
	}
	if got, _ := result["status"].(int); got != http.StatusNotFound {
		t.Fatalf("expected status 404, got %v", result["status"])
	}
}

func TestHTTPRequestToolTruncatesLargeBodies(t *testing.T) {
	payload := strings.Repeat("a", DefaultHTTPMaxResponseBody*2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, payload)
	}))
	defer server.Close()

	tool := NewHTTPRequestToolAllowPrivate()
	result, err := tool.Execute(map[string]any{"url": server.URL})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	body, _ := result["body"].(string)
	wantLen := DefaultHTTPMaxResponseBody + len(httpBodyTruncationMarker)
	if len(body) != wantLen {
		t.Fatalf("expected truncated body of %d bytes, got %d", wantLen, len(body))
	}
	if !strings.HasSuffix(body, httpBodyTruncationMarker) {
		t.Fatal("expected truncation marker suffix")
	}
}

func TestHTTPRequestToolRejectsOversizedRequestBody(t *testing.T) {
	tool := NewHTTPRequestToolAllowPrivate()
	big := strings.Repeat("x", maxHTTPRequestBody+1)
	_, err := tool.Execute(map[string]any{"url": "http://127.0.0.1:1/", "method": "POST", "body": big})
	if !errors.Is(err, ErrInvalidToolInput) {
		t.Fatalf("expected ErrInvalidToolInput for oversized body, got %v", err)
	}
}

func TestTimeoutArgParsing(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  time.Duration
	}{
		{"nil default", nil, 10 * time.Second},
		{"float64", float64(250), 250 * time.Millisecond},
		{"int", 100, 100 * time.Millisecond},
		{"int64", int64(1000), time.Second},
		{"string", "1500", 1500 * time.Millisecond},
		{"zero default", 0, 10 * time.Second},
		{"negative default", float64(-5), 10 * time.Second},
		{"over max clamped", float64(600000), maxHTTPToolTimeout},
		{"garbage default", []int{1}, 10 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := timeoutArg(tc.value, 10*time.Second, maxHTTPToolTimeout); got != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
		})
	}
}
