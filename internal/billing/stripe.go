package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Optional Stripe usage-record sync (issue #57).
//
// A THIN REST client — no stripe-go dependency (go.mod is frozen): usage
// meters are POSTed to Stripe's https://api.stripe.com/v1/usage_records as
// form-encoded requests. Semantics (all documented, all tested in
// stripe_test.go against an httptest fake):
//
//   - AUTH          Authorization: Bearer $STRIPE_API_KEY.
//   - IDEMPOTENCY   every POST carries an Idempotency-Key derived
//                   deterministically from (org, period, meter):
//                   "agentos-<orgID>-<fromUnix>-<toUnix>-<meter>". Re-syncing
//                   the SAME org+period+meter reuses the same key, so Stripe
//                   (and any transport retry) deduplicates — the increment
//                   action plus deterministic keys can never double count.
//   - PAYLOAD       application/x-www-form-urlencoded with
//                   subscription_item, quantity, timestamp, action=increment.
//                   The timestamp is the window end clamped to now (Stripe
//                   rejects future timestamps; clamping keeps in-flight
//                   periods deliverable). Meters with a zero/absent quantity
//                   are NOT posted — posting a zero would fabricate usage.
//   - RETRIES       5xx, 429 and network errors are retried with exponential
//                   backoff (base 100ms, doubling, capped at 2s); a 429
//                   additionally honors Stripe's Retry-After (capped at 30s).
//                   Other 4xx are permanent and fail immediately. Context
//                   cancellation is respected between attempts.
//   - NO-OP         when STRIPE_API_KEY is unset the constructor returns a
//                   NoopSyncer: zero network, one debug log per call.
//   - ASYNC         SyncUsage is NEVER called on the run path; the HTTP layer
//                   (cmd/api) fires it in a recover-guarded goroutine with
//                   its own timeout after a meters read. There is no manual
//                   POST trigger: v1 stays read-only and the sync is
//                   env-driven (so no audit entry is emitted — the sync is
//                   system behavior triggered by a read, not a user-initiated
//                   state change; outcomes are logged instead).
// ---------------------------------------------------------------------------

// Environment knobs.
const (
	// EnvAPIKey enables the syncer when set to a non-empty Stripe secret key.
	EnvAPIKey = "STRIPE_API_KEY"
	// EnvAPIBase overrides the Stripe API base URL (self-hosted fakes, tests).
	EnvAPIBase = "STRIPE_API_BASE"
	// EnvSubscriptionItem is the platform-level default subscription item the
	// usage records are posted against. Optional: when unset the syncer skips
	// with a debug log instead of guessing an SI id (never fabricate one).
	EnvSubscriptionItem = "STRIPE_SUBSCRIPTION_ITEM"
)

// Client defaults.
const (
	stripeDefaultBaseURL   = "https://api.stripe.com"
	stripeUsageRecordsPath = "/v1/usage_records"
	stripeRequestTimeout   = 10 * time.Second
	stripeMaxAttempts      = 4 // 1 attempt + 3 retries
	stripeBaseBackoff      = 100 * time.Millisecond
	stripeMaxBackoff       = 2 * time.Second
	stripeRetryAfterCap    = 30 * time.Second
	stripeErrorBodyLimit   = 4 << 10 // 4 KiB of a non-2xx body into the error
	stripeKeyMaxLen        = 255     // Stripe Idempotency-Key length limit
)

// StripeSyncer pushes one org's aggregated meters for one window to Stripe.
// Implementations must be safe for concurrent use (the HTTP layer may call
// it from a goroutine per meters read).
type StripeSyncer interface {
	// SyncUsage posts one usage record per meter with quantity > 0. It is
	// idempotent per (org, from, to, meter) — see the Idempotency-Key note.
	SyncUsage(ctx context.Context, orgID string, from, to time.Time, meters *Meters) error
}

// SyncerEnabled reports whether the syncer performs real network work; the
// HTTP layer skips its async goroutine entirely for disabled syncers. The
// capability is optional — unknown implementations are treated as enabled
// (conservative: never silently drop a sync).
func SyncerEnabled(s StripeSyncer) bool {
	if s == nil {
		return false
	}
	type enabler interface{ Enabled() bool }
	if e, ok := s.(enabler); ok {
		return e.Enabled()
	}
	return true
}

// NoopSyncer is the zero-network syncer used whenever STRIPE_API_KEY is
// unset: every call logs at debug and succeeds. It exists so callers never
// need a nil check and no code path can accidentally hit Stripe.
type NoopSyncer struct {
	Logr *slog.Logger
}

// NewNoopSyncer returns the zero-network syncer.
func NewNoopSyncer(logr *slog.Logger) *NoopSyncer {
	return &NoopSyncer{Logr: logr}
}

// Enabled is always false: the NoopSyncer never performs network work.
func (n *NoopSyncer) Enabled() bool { return false }

// SyncUsage logs at debug and returns nil (zero network, zero side effects).
func (n *NoopSyncer) SyncUsage(_ context.Context, orgID string, from, to time.Time, meters *Meters) error {
	if n != nil && n.Logr != nil {
		n.Logr.Debug("stripe usage sync skipped (no STRIPE_API_KEY)",
			"org_id", orgID, "from", from.Format(time.RFC3339), "to", to.Format(time.RFC3339))
	}
	return nil
}

// StripeClient is the thin REST client (net/http only). Construct with
// NewStripeClient or — for production — NewStripeSyncerFromEnv. The zero
// value is not usable; clients are immutable after construction except for
// the retry/backoff knobs tests may tune (same package).
type StripeClient struct {
	apiKey string
	// baseURL defaults to https://api.stripe.com (EnvAPIBase override).
	baseURL string
	// subscriptionItem is the Stripe subscription item usage is posted
	// against (EnvSubscriptionItem). Empty disables posting (documented
	// skip — an SI id is never guessed).
	subscriptionItem string
	logr             *slog.Logger
	http             *http.Client
	maxAttempts      int
	backoff          func(attempt int) time.Duration
	// nowFn allows tests to pin the timestamp clamp.
	nowFn func() time.Time
}

// NewStripeClient returns a client for the given key/base/SI triple. An
// empty base falls back to https://api.stripe.com; a nil logger falls back
// to slog.Default().
func NewStripeClient(apiKey, baseURL, subscriptionItem string, logr *slog.Logger) *StripeClient {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = stripeDefaultBaseURL
	}
	if logr == nil {
		logr = slog.Default()
	}
	c := &StripeClient{
		apiKey:           strings.TrimSpace(apiKey),
		baseURL:          strings.TrimRight(baseURL, "/"),
		subscriptionItem: strings.TrimSpace(subscriptionItem),
		logr:             logr,
		http:             &http.Client{Timeout: stripeRequestTimeout},
		maxAttempts:      stripeMaxAttempts,
	}
	c.backoff = c.defaultBackoff
	c.nowFn = func() time.Time { return time.Now().UTC() }
	return c
}

// NewStripeSyncerFromEnv is the production constructor: STRIPE_API_KEY set →
// a live StripeClient (base/SI from their optional env knobs); unset → the
// zero-network NoopSyncer. It never fails: a missing key is a legitimate
// "sync disabled" deployment, not an error.
func NewStripeSyncerFromEnv(logr *slog.Logger) StripeSyncer {
	if strings.TrimSpace(os.Getenv(EnvAPIKey)) == "" {
		return NewNoopSyncer(logr)
	}
	return NewStripeClient(os.Getenv(EnvAPIKey), os.Getenv(EnvAPIBase), os.Getenv(EnvSubscriptionItem), logr)
}

// Enabled is always true for a live client.
func (c *StripeClient) Enabled() bool { return true }

// defaultBackoff is exponential with a doubling base and a hard cap:
// 100ms, 200ms, 400ms, ... capped at stripeMaxBackoff.
func (c *StripeClient) defaultBackoff(attempt int) time.Duration {
	d := stripeBaseBackoff << attempt // attempt starts at 0
	if d <= 0 || d > stripeMaxBackoff {
		return stripeMaxBackoff
	}
	return d
}

// SyncUsage posts one usage record per meter with a positive quantity. The
// meter values come from MetersForPeriod (same process, same read) — the
// syncer never recomputes and never invents usage.
func (c *StripeClient) SyncUsage(ctx context.Context, orgID string, from, to time.Time, meters *Meters) error {
	if c == nil || c.apiKey == "" {
		return errors.New("billing: stripe sync requires an API key")
	}
	if strings.TrimSpace(orgID) == "" {
		return errors.New("billing: stripe sync requires an org id")
	}
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return fmt.Errorf("%w: stripe sync requires from < to", ErrInvalidPeriod)
	}
	if meters == nil {
		return errors.New("billing: stripe sync requires meters")
	}
	if c.subscriptionItem == "" {
		// Honest skip: posting against a guessed subscription item would
		// corrupt someone's Stripe billing. The env knob is the fix.
		c.logr.Debug("stripe usage sync skipped (no STRIPE_SUBSCRIPTION_ITEM configured)", "org_id", orgID)
		return nil
	}

	// Stripe rejects future usage timestamps; clamp the window end to now so
	// syncing the in-flight period is deliverable.
	ts := to
	if now := c.nowFn(); ts.After(now) {
		ts = now
	}

	for _, m := range []struct {
		name     string
		quantity int64
	}{
		{MeterRunsCount, meters.RunsCount},
		{MeterToolCallsCount, meters.ToolCallsCount},
	} {
		if m.quantity <= 0 {
			// Nothing happened for this meter in this window: posting a zero
			// record would fabricate usage; skip it.
			continue
		}
		if err := c.postUsageRecord(ctx, orgID, from, to, ts, m.name, m.quantity); err != nil {
			return err
		}
	}
	return nil
}

// postUsageRecord POSTs one meter with retry/backoff (5xx, 429, network) and
// fails fast on other 4xx.
func (c *StripeClient) postUsageRecord(ctx context.Context, orgID string, from, to, ts time.Time, meter string, quantity int64) error {
	form := url.Values{}
	form.Set("subscription_item", c.subscriptionItem)
	form.Set("quantity", strconv.FormatInt(quantity, 10))
	form.Set("timestamp", strconv.FormatInt(ts.Unix(), 10))
	form.Set("action", "increment") // deterministic Idempotency-Key makes increment replay-safe

	idem := idempotencyKeyFor(orgID, from, to, meter)

	var lastErr error
	retryAfter := time.Duration(0)
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		if attempt > 0 {
			wait := c.backoff(attempt - 1)
			if retryAfter > wait {
				wait = retryAfter // 429: honor Retry-After (already capped)
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
		}
		ra, err := c.postOnce(ctx, form, idem)
		if err == nil {
			c.logr.Debug("stripe usage record synced",
				"org_id", orgID, "meter", meter, "quantity", quantity,
				"idempotency_key", idem)
			return nil
		}
		lastErr = err
		retryAfter = 0
		var se *stripeStatusError
		if errors.As(err, &se) {
			if se.StatusCode < 500 && se.StatusCode != http.StatusTooManyRequests {
				return err // permanent client error: never retried
			}
			if se.StatusCode == http.StatusTooManyRequests && ra > 0 {
				retryAfter = time.Duration(ra) * time.Second
				if retryAfter > stripeRetryAfterCap {
					retryAfter = stripeRetryAfterCap
				}
			}
		}
	}
	return fmt.Errorf("billing: stripe usage sync failed after %d attempts: %w", c.maxAttempts, lastErr)
}

// postOnce performs one HTTP attempt. On a non-2xx response it returns the
// parsed Retry-After seconds (0 when absent) and a *stripeStatusError.
func (c *StripeClient) postOnce(ctx context.Context, form url.Values, idempotencyKey string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+stripeUsageRecordsPath, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", idempotencyKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err // network error: retryable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain (bounded) so the connection is reusable.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return 0, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, stripeErrorBodyLimit))
	ra := 0
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ra = n
		}
	}
	return ra, &stripeStatusError{StatusCode: resp.StatusCode, Body: string(body)}
}

// stripeStatusError is a non-2xx Stripe response.
type stripeStatusError struct {
	StatusCode int
	Body       string
}

func (e *stripeStatusError) Error() string {
	return fmt.Sprintf("billing: stripe returned status %d: %s", e.StatusCode, e.Body)
}

// idempotencyKeyFor derives the deterministic per (org, period, meter) key:
// "agentos-<orgID>-<fromUnix>-<toUnix>-<meter>". Keys longer than Stripe's
// 255-char limit (long tenant ids) fall back to a stable 40-hex digest of
// the same tuple — still deterministic, still per (org, period, meter).
func idempotencyKeyFor(orgID string, from, to time.Time, meter string) string {
	key := fmt.Sprintf("agentos-%s-%d-%d-%s", orgID, from.Unix(), to.Unix(), meter)
	if len(key) <= stripeKeyMaxLen {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "agentos-" + hex.EncodeToString(sum[:20])
}

// sleepCtx waits d or until the context is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
