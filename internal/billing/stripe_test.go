package billing

// Stripe usage-record client tests (issue #57): the thin net/http client is
// exercised against an httptest fake that asserts the contract pinned in
// stripe.go — POST /v1/usage_records, Bearer auth, deterministic
// Idempotency-Key per (org, period, meter), form payload shape, retry with
// backoff on 5xx/429/network, fail-fast on other 4xx, honest skips (no SI, no
// zero-quantity meters) and the zero-network NoopSyncer.

import (
        "context"
        "io"
        "log/slog"
        "net/http"
        "net/http/httptest"
        "net/url"
        "strings"
        "sync"
        "testing"
        "time"
)

// testLogger discards output.
func testLogger() *slog.Logger {
        return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordedRequest is one captured POST.
type recordedRequest struct {
        method         string
        path           string
        authorization  string
        contentType    string
        idempotencyKey string
        form           map[string]string
}

// captureServer records every request and answers with a caller-controlled
// sequence of (delay, status) responses.
type captureServer struct {
        srv *httptest.Server

        mu          sync.Mutex
        requests    []recordedRequest
        statuses    []int // consumed per request; last value repeats
        retryAfters []string
}

func newCaptureServer(t *testing.T, statuses ...int) *captureServer {
        t.Helper()
        c := &captureServer{statuses: statuses}
        mux := http.NewServeMux()
        mux.HandleFunc("/v1/usage_records", func(w http.ResponseWriter, r *http.Request) {
                body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
                req := recordedRequest{
                        method:         r.Method,
                        path:           r.URL.Path,
                        authorization:  r.Header.Get("Authorization"),
                        contentType:    r.Header.Get("Content-Type"),
                        idempotencyKey: r.Header.Get("Idempotency-Key"),
                        form:           parseForm(string(body)),
                }
                c.mu.Lock()
                i := len(c.requests)
                c.requests = append(c.requests, req)
                var status int
                switch {
                case i < len(c.statuses):
                        status = c.statuses[i]
                default:
                        status = c.statuses[len(c.statuses)-1]
                }
                var ra string
                if i < len(c.retryAfters) {
                        ra = c.retryAfters[i]
                }
                c.mu.Unlock()
                if ra != "" {
                        w.Header().Set("Retry-After", ra)
                }
                w.WriteHeader(status)
        })
        c.srv = httptest.NewServer(mux)
        t.Cleanup(c.srv.Close)
        return c
}

func (c *captureServer) count() int {
        c.mu.Lock()
        defer c.mu.Unlock()
        return len(c.requests)
}

func (c *captureServer) at(i int) recordedRequest {
        c.mu.Lock()
        defer c.mu.Unlock()
        return c.requests[i]
}

// parseForm parses an application/x-www-form-urlencoded body.
func parseForm(raw string) map[string]string {
        vals, err := url.ParseQuery(raw)
        if err != nil {
                return map[string]string{"__parse_error__": err.Error()}
        }
        out := map[string]string{}
        for k, v := range vals {
                out[k] = strings.Join(v, ",")
        }
        return out
}

func newTestClient(t *testing.T, statuses ...int) (*StripeClient, *captureServer) {
        t.Helper()
        server := newCaptureServer(t, statuses...)
        client := NewStripeClient("sk_test_agentos", server.srv.URL, "si_test_123", testLogger())
        client.backoff = func(int) time.Duration { return time.Millisecond }
        return client, server
}

var (
        syncFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
        syncTo   = time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
)

func TestStripeClientPostsUsageRecord(t *testing.T) {
        client, server := newTestClient(t, http.StatusOK)
        meters := &Meters{RunsCount: 7, ToolCallsCount: 0} // tool calls zero: NOT posted

        if err := client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, meters); err != nil {
                t.Fatalf("SyncUsage returned error: %v", err)
        }
        if got := server.count(); got != 1 {
                t.Fatalf("expected exactly 1 POST (zero-quantity meter skipped), got %d", got)
        }
        req := server.at(0)
        if req.method != http.MethodPost || req.path != stripeUsageRecordsPath {
                t.Fatalf("expected POST %s, got %s %s", stripeUsageRecordsPath, req.method, req.path)
        }
        if req.authorization != "Bearer sk_test_agentos" {
                t.Fatalf("authorization header: expected Bearer sk_test_agentos, got %q", req.authorization)
        }
        if req.contentType != "application/x-www-form-urlencoded" {
                t.Fatalf("content type: got %q", req.contentType)
        }
        // Idempotency-Key: deterministic per (org, period, meter).
        wantKey := "agentos-org-1-1767225600-1769817600-runs_count"
        if req.idempotencyKey != wantKey {
                t.Fatalf("idempotency key: expected %q, got %q", wantKey, req.idempotencyKey)
        }
        if req.form["subscription_item"] != "si_test_123" {
                t.Fatalf("subscription_item: got %q", req.form["subscription_item"])
        }
        if req.form["quantity"] != "7" {
                t.Fatalf("quantity: got %q", req.form["quantity"])
        }
        if req.form["timestamp"] != "1769817600" { // window end (not future -> unclamped)
                t.Fatalf("timestamp: got %q", req.form["timestamp"])
        }
        if req.form["action"] != "increment" {
                t.Fatalf("action: got %q", req.form["action"])
        }
}

func TestStripeClientOnePostPerMeter(t *testing.T) {
        client, server := newTestClient(t, http.StatusOK, http.StatusOK)
        meters := &Meters{RunsCount: 3, ToolCallsCount: 11}

        if err := client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, meters); err != nil {
                t.Fatalf("SyncUsage returned error: %v", err)
        }
        if got := server.count(); got != 2 {
                t.Fatalf("expected 2 POSTs (one per meter), got %d", got)
        }
        first, second := server.at(0), server.at(1)
        if first.form["quantity"] != "3" || second.form["quantity"] != "11" {
                t.Fatalf("unexpected per-meter quantities: %q then %q", first.form["quantity"], second.form["quantity"])
        }
        // Keys are per meter: same org+period prefix, distinct meter suffix.
        if first.idempotencyKey == second.idempotencyKey {
                t.Fatalf("per-meter keys must differ, both %q", first.idempotencyKey)
        }
        if !strings.HasSuffix(first.idempotencyKey, "-runs_count") || !strings.HasSuffix(second.idempotencyKey, "-tool_calls_count") {
                t.Fatalf("unexpected meter key suffixes: %q / %q", first.idempotencyKey, second.idempotencyKey)
        }
}

func TestStripeClientRetries500ThenSucceeds(t *testing.T) {
        client, server := newTestClient(t, http.StatusInternalServerError, http.StatusInternalServerError, http.StatusOK)
        if err := client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, &Meters{RunsCount: 2}); err != nil {
                t.Fatalf("SyncUsage should succeed after retries, got: %v", err)
        }
        if got := server.count(); got != 3 {
                t.Fatalf("expected 3 attempts (2x500 then 200), got %d", got)
        }
        // Every retry MUST reuse the same Idempotency-Key — that is what makes
        // the increment action replay-safe on Stripe's side.
        if server.at(0).idempotencyKey != server.at(1).idempotencyKey || server.at(1).idempotencyKey != server.at(2).idempotencyKey {
                t.Fatalf("retries must reuse the idempotency key: %q / %q / %q",
                        server.at(0).idempotencyKey, server.at(1).idempotencyKey, server.at(2).idempotencyKey)
        }
}

func TestStripeClient500ExhaustsRetries(t *testing.T) {
        client, server := newTestClient(t, http.StatusInternalServerError)
        client.maxAttempts = 3 // keep the test fast; pins the attempts contract

        err := client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, &Meters{RunsCount: 2})
        if err == nil {
                t.Fatal("expected error after exhausting retries")
        }
        if got := server.count(); got != 3 {
                t.Fatalf("expected exactly %d attempts, got %d", client.maxAttempts, got)
        }
        if !strings.Contains(err.Error(), "after 3 attempts") || !strings.Contains(err.Error(), "status 500") {
                t.Fatalf("error should name attempts and status, got: %v", err)
        }
}

func TestStripeClient429HonorsRetryAfter(t *testing.T) {
        client, server := newTestClient(t, http.StatusTooManyRequests, http.StatusOK)
        server.retryAfters = []string{"1", ""} // first attempt: Retry-After: 1

        start := time.Now()
        if err := client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, &Meters{RunsCount: 2}); err != nil {
                t.Fatalf("SyncUsage should succeed after 429 backoff, got: %v", err)
        }
        if got := server.count(); got != 2 {
                t.Fatalf("expected 2 attempts (429 then 200), got %d", got)
        }
        // The 1s Retry-After must be honored on top of (instead of) the tiny test
        // backoff schedule.
        if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
                t.Fatalf("429 must back off per Retry-After, elapsed only %v", elapsed)
        }
}

func TestStripeClient400FailsFast(t *testing.T) {
        client, server := newTestClient(t, http.StatusBadRequest)
        err := client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, &Meters{RunsCount: 2})
        if err == nil {
                t.Fatal("expected a permanent error for 400")
        }
        if got := server.count(); got != 1 {
                t.Fatalf("4xx (other than 429) must never be retried, got %d attempts", got)
        }
}

func TestStripeClientNetworkErrorsRetried(t *testing.T) {
        // A closed server: every attempt is a transport error (retryable).
        client := NewStripeClient("sk_test_agentos", "http://127.0.0.1:1", "si_test_123", testLogger())
        client.backoff = func(int) time.Duration { return time.Millisecond }
        client.maxAttempts = 3

        err := client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, &Meters{RunsCount: 2})
        if err == nil {
                t.Fatal("expected error after network failures")
        }
        if !strings.Contains(err.Error(), "after 3 attempts") {
                t.Fatalf("expected attempts in error, got: %v", err)
        }
}

func TestStripeClientDeterministicIdempotencyKeys(t *testing.T) {
        client, server := newTestClient(t, http.StatusOK, http.StatusOK, http.StatusOK, http.StatusOK)
        meters := &Meters{RunsCount: 1, ToolCallsCount: 1}

        // Same org+period twice: identical keys (Stripe dedupes, no double count).
        // One POST per meter with quantity > 0: 2 POSTs per SyncUsage.
        _ = client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, meters)
        _ = client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, meters)
        // Different period: different keys.
        _ = client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo.Add(24*time.Hour), meters)

        if got := server.count(); got != 6 {
                t.Fatalf("expected 6 POSTs so far (2 meters x 3 calls), got %d", got)
        }
        if server.at(0).idempotencyKey != server.at(2).idempotencyKey {
                t.Fatalf("same window must reuse the key: %q vs %q", server.at(0).idempotencyKey, server.at(2).idempotencyKey)
        }
        if server.at(0).idempotencyKey == server.at(4).idempotencyKey {
                t.Fatal("different windows must not share a key")
        }
        // Cross-org isolation of keys.
        _ = client.SyncUsage(context.Background(), "org-2", syncFrom, syncTo, meters)
        if got := server.count(); got != 8 {
                t.Fatalf("expected 8 POSTs after the 4th call, got %d", got)
        }
        if server.at(0).idempotencyKey == server.at(6).idempotencyKey {
                t.Fatal("different orgs must not share a key")
        }
}

func TestStripeClientTimestampClampedToNow(t *testing.T) {
        client, server := newTestClient(t, http.StatusOK)
        pinned := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
        client.nowFn = func() time.Time { return pinned }

        // An in-flight window whose end is in the future.
        if err := client.SyncUsage(context.Background(), "org-1", pinned.Add(-time.Hour), pinned.Add(48*time.Hour), &Meters{RunsCount: 2}); err != nil {
                t.Fatalf("SyncUsage returned error: %v", err)
        }
        if got := server.at(0).form["timestamp"]; got != "1773576000" { // pinned now
                t.Fatalf("future timestamps must clamp to now, got %q", got)
        }
}

func TestStripeClientWithoutSubscriptionItemSkips(t *testing.T) {
        // No STRIPE_SUBSCRIPTION_ITEM: honest skip — never guess an SI id.
        srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
                t.Fatal("no request may be sent without a subscription item")
        }))
        defer srv.Close()
        client := NewStripeClient("sk_test_agentos", srv.URL, "", testLogger())
        if err := client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, &Meters{RunsCount: 5}); err != nil {
                t.Fatalf("skip must be a clean no-op, got: %v", err)
        }
}

func TestStripeClientInputValidation(t *testing.T) {
        client := NewStripeClient("sk_test_agentos", "http://127.0.0.1:1", "si_test_123", testLogger())
        if err := client.SyncUsage(context.Background(), "", syncFrom, syncTo, &Meters{}); err == nil {
                t.Fatal("empty org id must fail")
        }
        if err := client.SyncUsage(context.Background(), "org-1", syncTo, syncFrom, &Meters{}); err == nil {
                t.Fatal("inverted window must fail")
        }
        if err := client.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, nil); err == nil {
                t.Fatal("nil meters must fail")
        }
        keyless := NewStripeClient("", "http://127.0.0.1:1", "si_test_123", testLogger())
        if err := keyless.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, &Meters{}); err == nil {
                t.Fatal("missing API key must fail")
        }
}

func TestIdempotencyKeyFallsBackToDigest(t *testing.T) {
        longOrg := strings.Repeat("a", 300)
        key := idempotencyKeyFor(longOrg, syncFrom, syncTo, MeterRunsCount)
        if len(key) > stripeKeyMaxLen {
                t.Fatalf("key exceeds Stripe's %d-char limit: %d", stripeKeyMaxLen, len(key))
        }
        if key != idempotencyKeyFor(longOrg, syncFrom, syncTo, MeterRunsCount) {
                t.Fatal("digest fallback must stay deterministic")
        }
        if key == idempotencyKeyFor(longOrg, syncFrom, syncTo, MeterToolCallsCount) {
                t.Fatal("digest fallback must stay per-meter")
        }
}

func TestNoopSyncerZeroNetwork(t *testing.T) {
        noop := NewNoopSyncer(testLogger())
        if noop.Enabled() {
                t.Fatal("NoopSyncer must report disabled")
        }
        if SyncerEnabled(noop) {
                t.Fatal("SyncerEnabled(NoopSyncer) must be false")
        }
        if err := noop.SyncUsage(context.Background(), "org-1", syncFrom, syncTo, &Meters{RunsCount: 5}); err != nil {
                t.Fatalf("NoopSyncer must always succeed, got: %v", err)
        }
        if SyncerEnabled(nil) {
                t.Fatal("SyncerEnabled(nil) must be false")
        }
        // Unknown implementations are treated as enabled (conservative).
        if !SyncerEnabled(unknownSyncer{}) {
                t.Fatal("unknown syncers must default to enabled")
        }
}

type unknownSyncer struct{}

func (unknownSyncer) SyncUsage(context.Context, string, time.Time, time.Time, *Meters) error {
        return nil
}

func TestNewStripeSyncerFromEnv(t *testing.T) {
        t.Setenv("STRIPE_API_KEY", "")
        if _, ok := NewStripeSyncerFromEnv(testLogger()).(*NoopSyncer); !ok {
                t.Fatal("unset STRIPE_API_KEY must yield the NoopSyncer")
        }

        // Key set: a live client pointed at the env base (the fake proves it).
        server := newCaptureServer(t, http.StatusOK)
        t.Setenv("STRIPE_API_KEY", "sk_test_env")
        t.Setenv("STRIPE_API_BASE", server.srv.URL)
        t.Setenv("STRIPE_SUBSCRIPTION_ITEM", "si_from_env")

        syncer := NewStripeSyncerFromEnv(testLogger())
        if noop, ok := syncer.(*NoopSyncer); ok {
                t.Fatalf("key set must yield a live client, got %T", noop)
        }
        if !SyncerEnabled(syncer) {
                t.Fatal("env-configured client must be enabled")
        }
        if err := syncer.SyncUsage(context.Background(), "org-env", syncFrom, syncTo, &Meters{RunsCount: 4}); err != nil {
                t.Fatalf("env syncer failed: %v", err)
        }
        if server.count() != 1 {
                t.Fatalf("expected 1 POST via env base URL, got %d", server.count())
        }
        req := server.at(0)
        if req.authorization != "Bearer sk_test_env" || req.form["subscription_item"] != "si_from_env" {
                t.Fatalf("env config not applied: auth=%q si=%q", req.authorization, req.form["subscription_item"])
        }
}
