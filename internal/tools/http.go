package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// HTTPRequestTool is the "http_request" tool exposed to agents. It performs a
// single outbound HTTP request and returns {status, body, headers}.
//
// Security model (SSRF protection, enabled unless the tool is built with
// NewHTTPRequestToolAllowPrivate for tests/trusted environments):
//   - only http/https URLs are accepted;
//   - loopback, unspecified (0.0.0.0/8), private (10/8, 172.16/12, 192.168/16),
//     link-local (169.254/16 including the cloud metadata endpoint
//     169.254.169.254), CGNAT (100.64/10), multicast and IPv6 ULA/link-local
//     destinations are refused;
//   - hostnames are resolved up-front and every resolved IP is checked;
//   - a dial-level Control hook re-checks the concrete peer IP of every
//     connection, which also covers redirect targets and DNS rebinding
//     between the pre-check and the actual dial.
type HTTPRequestTool struct {
	client          *http.Client
	allowPrivate    bool
	maxResponseBody int
	maxRequestBody  int
}

const (
	// HTTPToolName is the registry name of the HTTP request tool.
	HTTPToolName = "http_request"

	defaultHTTPToolTimeout = 10 * time.Second
	maxHTTPToolTimeout     = 60 * time.Second
	// DefaultHTTPMaxResponseBody caps how much of the response body is
	// returned to the model (to keep observations and the DB small).
	DefaultHTTPMaxResponseBody = 64 << 10
	httpBodyTruncationMarker   = "\n...[truncated]"
	maxHTTPRequestBody         = 1 << 20
	maxHTTPRedirects           = 3
)

var (
	// ErrInvalidToolInput is returned when the model supplied malformed tool
	// arguments.
	ErrInvalidToolInput = errors.New("tools: invalid tool input")
	// ErrBlockedHost is returned when SSRF protection refuses a destination.
	ErrBlockedHost = errors.New("tools: host blocked by ssrf protection")
)

// NewHTTPRequestTool builds the tool with SSRF protection enabled.
func NewHTTPRequestTool() *HTTPRequestTool {
	return newHTTPRequestTool(false)
}

// NewHTTPRequestToolAllowPrivate builds the tool with SSRF protection
// disabled. Intended for tests and explicitly trusted environments only.
func NewHTTPRequestToolAllowPrivate() *HTTPRequestTool {
	return newHTTPRequestTool(true)
}

func newHTTPRequestTool(allowPrivate bool) *HTTPRequestTool {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: ssrfDialControl(allowPrivate),
	}
	return &HTTPRequestTool{
		client: &http.Client{
			Transport: &http.Transport{
				// Proxy is intentionally disabled: an environment proxy
				// would silently defeat the SSRF dial-level checks.
				Proxy:       nil,
				DialContext: dialer.DialContext,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxHTTPRedirects {
					return fmt.Errorf("too many redirects (limit %d)", maxHTTPRedirects)
				}
				return nil
			},
		},
		allowPrivate:    allowPrivate,
		maxResponseBody: DefaultHTTPMaxResponseBody,
		maxRequestBody:  maxHTTPRequestBody,
	}
}

// Name implements Tool.
func (t *HTTPRequestTool) Name() string { return HTTPToolName }

// Execute implements Tool using a background context.
func (t *HTTPRequestTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware so the runtime can enforce its
// per-call tool timeout directly.
func (t *HTTPRequestTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t == nil || t.client == nil {
		return nil, errors.New("tools: http_request tool is not initialized")
	}

	rawURL, err := stringArg(input["url"])
	if err != nil {
		return nil, fmt.Errorf("%w: url: %v", ErrInvalidToolInput, err)
	}
	method, err := httpMethodArg(input["method"])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToolInput, err)
	}

	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot parse url %q: %v", ErrInvalidToolInput, rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: url scheme %q is not allowed (http/https only)", ErrInvalidToolInput, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: url host is required", ErrInvalidToolInput)
	}
	if err := t.checkDestination(ctx, parsed); err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if body, err := stringArg(input["body"]); err == nil && body != "" {
		if len(body) > t.maxRequestBody {
			return nil, fmt.Errorf("%w: body exceeds %d bytes", ErrInvalidToolInput, t.maxRequestBody)
		}
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build request: %v", ErrInvalidToolInput, err)
	}
	for key, value := range headerArgs(input["headers"]) {
		req.Header.Set(key, value)
	}
	if bodyReader != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	timeout := timeoutArg(input["timeout_ms"], defaultHTTPToolTimeout, maxHTTPToolTimeout)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Bind the call timeout to the request itself: client.Do honors the
	// request context, so the deadline actually cancels an in-flight call.
	req = req.WithContext(callCtx)

	start := time.Now()
	resp, err := t.client.Do(req)
	if err != nil {
		if callCtx.Err() != nil {
			return nil, fmt.Errorf("%s %s failed after %s: %w", method, parsed.Host, time.Since(start).Round(time.Millisecond), callCtx.Err())
		}
		if errors.Is(err, ErrBlockedHost) {
			return nil, err
		}
		return nil, fmt.Errorf("%s %s failed: %w", method, parsed.Host, err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, int64(t.maxResponseBody)+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading response body failed: %w", method, parsed.Host, err)
	}
	body := string(raw)
	if len(raw) > t.maxResponseBody {
		body = string(raw[:t.maxResponseBody]) + httpBodyTruncationMarker
	}

	headers := make(map[string][]string, len(resp.Header))
	for key, values := range resp.Header {
		headers[key] = append([]string(nil), values...)
	}

	return map[string]any{
		"status":  resp.StatusCode,
		"body":    body,
		"headers": headers,
	}, nil
}

// checkDestination performs the up-front SSRF validation: scheme/host rules
// plus, when protection is enabled, resolution of every IP the hostname maps
// to. The dial-level Control hook is the authoritative second gate.
func (t *HTTPRequestTool) checkDestination(ctx context.Context, u *url.URL) error {
	if t.allowPrivate {
		return nil
	}
	host := u.Hostname()
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return fmt.Errorf("%w: %q resolves to a forbidden local name", ErrBlockedHost, host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isForbiddenIP(ip) {
			return fmt.Errorf("%w: %s is a forbidden address", ErrBlockedHost, host)
		}
		return nil
	}
	resolver := net.DefaultResolver
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve host %q: %v", ErrBlockedHost, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: host %q resolved to no addresses", ErrBlockedHost, host)
	}
	for _, addr := range ips {
		if isForbiddenIP(addr.IP) {
			return fmt.Errorf("%w: host %q resolves to forbidden address %s", ErrBlockedHost, host, addr.IP)
		}
	}
	return nil
}

// ssrfDialControl returns a net.Dialer Control hook that validates the
// concrete peer IP right before the TCP connect. It runs for every dial the
// client makes (initial request, redirects), closing the DNS-rebinding and
// redirect-to-private holes.
func ssrfDialControl(allowPrivate bool) func(network, address string, conn syscall.RawConn) error {
	return func(network, address string, conn syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: invalid dial address %q", ErrBlockedHost, address)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("%w: non-IP dial address %q", ErrBlockedHost, host)
		}
		if isForbiddenIP(ip) {
			return fmt.Errorf("%w: refusing connection to %s", ErrBlockedHost, host)
		}
		return nil
	}
}

// isForbiddenIP reports whether ip must not be dialed by agent tools.
func isForbiddenIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 { // remainder of 0.0.0.0/8 ("this network")
			return true
		}
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 { // CGNAT 100.64/10
			return true
		}
	}
	return false
}

func stringArg(value any) (string, error) {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return "", errors.New("is required")
		}
		return v, nil
	case nil:
		return "", errors.New("is required")
	default:
		return "", fmt.Errorf("must be a string, got %T", value)
	}
}

var allowedHTTPMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodHead: true,
	http.MethodOptions: true,
}

func httpMethodArg(value any) (string, error) {
	method, err := stringArg(value)
	if err != nil {
		return http.MethodGet, nil // method is optional; default GET
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if !allowedHTTPMethods[method] {
		return "", fmt.Errorf("method %q is not allowed", method)
	}
	return method, nil
}

func headerArgs(value any) map[string]string {
	headers := make(map[string]string)
	raw, ok := value.(map[string]any)
	if !ok {
		return headers
	}
	for key, v := range raw {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		switch typed := v.(type) {
		case string:
			headers[key] = typed
		case nil:
			// skip
		default:
			headers[key] = fmt.Sprintf("%v", typed)
		}
	}
	return headers
}

// timeoutArg reads "timeout_ms" style numbers (JSON decoding yields float64)
// and clamps the result into [1ms, maxValue].
func timeoutArg(value any, def, maxValue time.Duration) time.Duration {
	var millis int64
	switch v := value.(type) {
	case float64:
		millis = int64(v)
	case int64:
		millis = v
	case int:
		millis = int64(v)
	case int32:
		millis = int64(v)
	case string:
		parsed := strings.TrimSpace(v)
		if parsed == "" {
			return def
		}
		neg := false
		if parsed[0] == '-' {
			neg = true
			parsed = parsed[1:]
		}
		for _, ch := range parsed {
			if ch < '0' || ch > '9' {
				return def
			}
			millis = millis*10 + int64(ch-'0')
		}
		if neg {
			millis = -millis
		}
	case nil:
		return def
	default:
		return def
	}
	if millis <= 0 {
		return def
	}
	d := time.Duration(millis) * time.Millisecond
	if d > maxValue {
		return maxValue
	}
	if d < time.Millisecond {
		return time.Millisecond
	}
	return d
}
