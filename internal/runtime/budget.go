// budget.go — issue #107: per-run token budget enforcement for the agent
// runtime.
//
// The runtime already records provider-reported usage on every model step
// (run.Tokens). TokenBudget adds ENFORCEMENT on top of that accounting:
// cumulative prompt/completion/total tokens are tracked across ALL steps of
// a run and compared against the configured limits.
//
// Behavior contract:
//
//   - limits of 0 mean "unlimited" (the default: no budget configured -> no
//     tracking, zero overhead, byte-identical legacy behavior);
//   - when cumulative usage crosses AlertThreshold (default 0.8) of any
//     configured limit, a warning is recorded ONCE as a dedicated "budget"
//     annotation step on the run timeline;
//   - when cumulative usage EXCEEDS any limit the loop hard-stops: the
//     budget annotation step (status failed) is recorded, the
//     run.token_budget_exceeded event is published through the event seam
//     (when wired and the run carries a tenant scope) and the loop returns a
//     typed *TokenBudgetExceededError;
//   - run status mapping is unchanged: the typed error flows through the
//     existing failure path, so the run is marked FAILED exactly like every
//     other loop error (ErrMaxStepsExceeded et al). No new run status is
//     introduced — the machine reason rides on run.Error (the message starts
//     with "token_budget_exceeded") and on the failed budget step, keeping
//     the runs API contract stable. Enforcement is post-response (reported
//     usage): a single call can overshoot before the stop lands, which is
//     the standard trade-off for provider-reported accounting.
package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"agentos/internal/models"
)

// TokenBudgetConfig is the per-agent/per-runner budget configuration carried
// in the runtime_config document under the "token_budget" key. All limits
// default to 0 = unlimited; AlertThreshold defaults to DefaultAlertThreshold
// when unset.
type TokenBudgetConfig struct {
	// MaxTotalTokens caps the cumulative total tokens of one run (0 =
	// unlimited).
	MaxTotalTokens int `json:"max_total_tokens,omitempty"`
	// MaxPromptTokens caps the cumulative prompt tokens of one run (0 =
	// unlimited).
	MaxPromptTokens int `json:"max_prompt_tokens,omitempty"`
	// MaxCompletionTokens caps the cumulative completion tokens of one run
	// (0 = unlimited).
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
	// AlertThreshold is the fractional crossing (of any configured limit)
	// that records the one-time warning annotation; 0 applies
	// DefaultAlertThreshold, values above 1 are rejected by
	// ParseRuntimeConfig.
	AlertThreshold float64 `json:"alert_threshold,omitempty"`
}

// DefaultAlertThreshold is the default fractional usage crossing that
// triggers the budget warning annotation (issue #107 safe default).
const DefaultAlertThreshold = 0.8

// Budget dimension labels used in error messages, annotations and events.
const (
	budgetDimensionTotal      = "total"
	budgetDimensionPrompt     = "prompt"
	budgetDimensionCompletion = "completion"
)

// BudgetSnapshot is the point-in-time budget state recorded on budget
// annotation steps (it flows into the step record's output meta, so budget
// data is persisted with the run trace).
type BudgetSnapshot struct {
	Phase               string  `json:"phase"` // "warn" | "exceeded"
	PromptTokens        int     `json:"prompt_tokens"`
	CompletionTokens    int     `json:"completion_tokens"`
	TotalTokens         int     `json:"total_tokens"`
	MaxTotalTokens      int     `json:"max_total_tokens,omitempty"`
	MaxPromptTokens     int     `json:"max_prompt_tokens,omitempty"`
	MaxCompletionTokens int     `json:"max_completion_tokens,omitempty"`
	AlertThreshold      float64 `json:"alert_threshold,omitempty"`
	// Limit is the dimension that tripped ("total", "prompt" or
	// "completion"); empty on warn snapshots that crossed no threshold.
	Limit string `json:"limit,omitempty"`
}

// TokenBudgetExceededError is the typed outcome returned when a run exceeds
// its configured token budget. errors.Is(err, ErrTokenBudgetExceeded)
// classifies it.
type TokenBudgetExceededError struct {
	// Dimension is the tripped limit: "total", "prompt" or "completion".
	Dimension string
	// Used is the cumulative usage at the moment of the violation.
	Used int
	// Limit is the configured maximum that was exceeded.
	Limit int
}

// ErrTokenBudgetExceeded is the sentinel for errors.Is classification of
// token budget violations (issue #107).
var ErrTokenBudgetExceeded = fmt.Errorf("runtime: token budget exceeded")

// Error implements error with the machine-readable
// "token_budget_exceeded: ..." prefix that doubles as the run's outcome
// marker.
func (e *TokenBudgetExceededError) Error() string {
	return fmt.Sprintf("token_budget_exceeded: %s_tokens used=%d limit=%d", e.Dimension, e.Used, e.Limit)
}

// Is matches the ErrTokenBudgetExceeded sentinel.
func (e *TokenBudgetExceededError) Is(target error) bool { return target == ErrTokenBudgetExceeded }

// TokenBudget is the mutable per-run token budget tracker. It accumulates
// the provider-reported usage of every model call and classifies each record
// as ok / warn / exceeded. Single-run state: not safe for concurrent use
// (the runtime loop is single-goroutine per run). Construct with
// NewTokenBudget; the nil *TokenBudget is a valid unlimited-budget no-op
// (nil-safe DI, matching the Runner's other seams).
type TokenBudget struct {
	MaxTotalTokens      int
	MaxPromptTokens     int
	MaxCompletionTokens int
	AlertThreshold      float64

	usedPrompt     int
	usedCompletion int
	usedTotal      int
	exceeded       bool
	alerted        bool
	lastViolation  *TokenBudgetExceededError
}

// NewTokenBudget builds a tracker from the config: a config with all limits
// unset returns nil (no tracking, unlimited); AlertThreshold <= 0 applies
// DefaultAlertThreshold and values above 1 are clamped to 1.
func NewTokenBudget(cfg TokenBudgetConfig) *TokenBudget {
	if cfg.MaxTotalTokens <= 0 && cfg.MaxPromptTokens <= 0 && cfg.MaxCompletionTokens <= 0 {
		return nil
	}
	threshold := cfg.AlertThreshold
	if threshold <= 0 {
		threshold = DefaultAlertThreshold
	}
	if threshold > 1 {
		threshold = 1
	}
	return &TokenBudget{
		MaxTotalTokens:      cfg.MaxTotalTokens,
		MaxPromptTokens:     cfg.MaxPromptTokens,
		MaxCompletionTokens: cfg.MaxCompletionTokens,
		AlertThreshold:      threshold,
	}
}

// BudgetState is the result of one Record call.
type BudgetState struct {
	// Exceeded is non-nil when the run must hard-stop now.
	Exceeded *TokenBudgetExceededError
	// Alert is non-nil when this record crossed the alert threshold for the
	// first time (suppress while exceeded: the violation annotation carries
	// everything the warning would).
	Alert *BudgetSnapshot
	// Snapshot is the always-current budget state.
	Snapshot BudgetSnapshot
}

// Record accumulates one provider-reported usage and re-classifies the
// budget. Total accounting normalizes providers that report no total:
// total = max(reported total, prompt+completion). Dimension checks run in a
// fixed order (prompt, completion, total) so the tripped dimension is
// deterministic. A nil receiver records nothing.
func (b *TokenBudget) Record(u models.Usage) BudgetState {
	if b == nil {
		return BudgetState{}
	}
	b.usedPrompt += u.PromptTokens
	b.usedCompletion += u.CompletionTokens
	total := u.TotalTokens
	if u.PromptTokens+u.CompletionTokens > total {
		total = u.PromptTokens + u.CompletionTokens
	}
	b.usedTotal += total

	state := BudgetState{Snapshot: b.Snapshot()}
	if b.exceeded {
		// Sticky violation: later records keep reporting the FIRST tripped
		// dimension (the loop stops at the first violation anyway).
		state.Exceeded = b.lastViolation
		return state
	}
	return b.classify()
}

// classify applies the exceed/alert rules to the current counters.
func (b *TokenBudget) classify() BudgetState {
	state := BudgetState{Snapshot: b.Snapshot()}
	checks := []struct {
		dimension string
		used      int
		limit     int
	}{
		{budgetDimensionPrompt, b.usedPrompt, b.MaxPromptTokens},
		{budgetDimensionCompletion, b.usedCompletion, b.MaxCompletionTokens},
		{budgetDimensionTotal, b.usedTotal, b.MaxTotalTokens},
	}
	for _, c := range checks {
		if c.limit > 0 && c.used > c.limit {
			b.exceeded = true
			b.lastViolation = &TokenBudgetExceededError{Dimension: c.dimension, Used: c.used, Limit: c.limit}
			state.Exceeded = b.lastViolation
			state.Snapshot = b.Snapshot()
			state.Snapshot.Limit = c.dimension
			state.Snapshot.Phase = "exceeded"
			return state
		}
	}
	if !b.alerted {
		for _, c := range checks {
			if c.limit > 0 && float64(c.used) >= float64(c.limit)*b.AlertThreshold {
				b.alerted = true
				snap := b.Snapshot()
				snap.Limit = c.dimension
				snap.Phase = "warn"
				state.Alert = &snap
				break
			}
		}
	}
	return state
}

// Exceeded reports whether the budget has been violated (sticky).
func (b *TokenBudget) Exceeded() bool { return b != nil && b.exceeded }

// Snapshot returns the current cumulative usage and limits.
func (b *TokenBudget) Snapshot() BudgetSnapshot {
	if b == nil {
		return BudgetSnapshot{Phase: "ok"}
	}
	return BudgetSnapshot{
		Phase:               "ok",
		PromptTokens:        b.usedPrompt,
		CompletionTokens:    b.usedCompletion,
		TotalTokens:         b.usedTotal,
		MaxTotalTokens:      b.MaxTotalTokens,
		MaxPromptTokens:     b.MaxPromptTokens,
		MaxCompletionTokens: b.MaxCompletionTokens,
		AlertThreshold:      b.AlertThreshold,
	}
}

// --- per-agent runtime configuration document -------------------------------

// ConversationHistoryConfig is the conversation-history half of the
// runtime_config document (issue #107). Strategy must be one of the pinned
// TruncationStrategy values; MaxTokens 0 applies DefaultHistoryMaxTokens and
// a negative value disables history management.
type ConversationHistoryConfig struct {
	Strategy  TruncationStrategy `json:"strategy,omitempty"`
	MaxTokens int                `json:"max_tokens,omitempty"`
}

// RuntimeConfig is the per-agent runtime configuration document (issue #107).
// Agents do not persist a runtime_config blob yet — the Runner resolves it
// through WithAgentConfigSource (any store may back it) and/or the
// runner-wide WithRuntimeConfig; both fall back to safe defaults:
// conversation_history {strategy: sliding_window, max_tokens: 8000},
// token_budget unlimited.
type RuntimeConfig struct {
	ConversationHistory ConversationHistoryConfig `json:"conversation_history,omitempty"`
	TokenBudget         TokenBudgetConfig         `json:"token_budget,omitempty"`
}

// DefaultRuntimeConfig returns the safe-default configuration (issue #107):
// sliding_window history with DefaultHistoryMaxTokens and an unlimited token
// budget with the default alert threshold.
func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		ConversationHistory: ConversationHistoryConfig{
			Strategy:  DefaultHistoryStrategy,
			MaxTokens: DefaultHistoryMaxTokens,
		},
		TokenBudget: TokenBudgetConfig{AlertThreshold: DefaultAlertThreshold},
	}
}

// WithDefaults fills unset fields with the safe defaults (zero-valued
// fields keep the documented fallbacks; a negative history MaxTokens is an
// explicit disable and is preserved).
func (c RuntimeConfig) WithDefaults() RuntimeConfig {
	if c.ConversationHistory.Strategy == "" {
		c.ConversationHistory.Strategy = DefaultHistoryStrategy
	}
	if c.ConversationHistory.MaxTokens == 0 {
		c.ConversationHistory.MaxTokens = DefaultHistoryMaxTokens
	}
	if c.TokenBudget.AlertThreshold <= 0 {
		c.TokenBudget.AlertThreshold = DefaultAlertThreshold
	}
	return c
}

// ParseRuntimeConfig decodes a runtime_config JSON document
// ({"conversation_history": {...}, "token_budget": {...}}). An empty or
// blank document parses to the zero config (defaults apply later). It
// returns an error for malformed JSON, an unknown conversation strategy, a
// negative token-budget limit or an alert threshold outside (0, 1] — callers
// that must never break a run on bad config (the Runner) treat the error as
// "use the defaults". Unset fields stay zero: merge in the caller before
// applying WithDefaults.
func ParseRuntimeConfig(raw []byte) (RuntimeConfig, error) {
	var cfg RuntimeConfig
	if len(strings.TrimSpace(string(raw))) == 0 {
		return RuntimeConfig{}, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return RuntimeConfig{}, fmt.Errorf("runtime: invalid runtime_config: %w", err)
	}
	if s := cfg.ConversationHistory.Strategy; s != "" && !validStrategy(s) {
		return RuntimeConfig{}, fmt.Errorf("runtime: invalid runtime_config: unknown conversation_history strategy %q (want sliding_window, summarize_older or drop_oldest)", s)
	}
	tb := cfg.TokenBudget
	if tb.MaxTotalTokens < 0 || tb.MaxPromptTokens < 0 || tb.MaxCompletionTokens < 0 {
		return RuntimeConfig{}, fmt.Errorf("runtime: invalid runtime_config: token_budget limits must be >= 0 (0 = unlimited)")
	}
	if tb.AlertThreshold < 0 || tb.AlertThreshold > 1 {
		return RuntimeConfig{}, fmt.Errorf("runtime: invalid runtime_config: token_budget alert_threshold must be within (0, 1]")
	}
	return cfg, nil
}

// mergeRuntimeConfig overlays the per-agent override on the base config:
// only fields the override SETS (non-zero) win; everything else keeps the
// base value. Parsed-but-unset fields must therefore stay zero in
// ParseRuntimeConfig (they do).
func mergeRuntimeConfig(base, override RuntimeConfig) RuntimeConfig {
	if override.ConversationHistory.Strategy != "" {
		base.ConversationHistory.Strategy = override.ConversationHistory.Strategy
	}
	if override.ConversationHistory.MaxTokens != 0 {
		base.ConversationHistory.MaxTokens = override.ConversationHistory.MaxTokens
	}
	if override.TokenBudget.MaxTotalTokens != 0 {
		base.TokenBudget.MaxTotalTokens = override.TokenBudget.MaxTotalTokens
	}
	if override.TokenBudget.MaxPromptTokens != 0 {
		base.TokenBudget.MaxPromptTokens = override.TokenBudget.MaxPromptTokens
	}
	if override.TokenBudget.MaxCompletionTokens != 0 {
		base.TokenBudget.MaxCompletionTokens = override.TokenBudget.MaxCompletionTokens
	}
	if override.TokenBudget.AlertThreshold != 0 {
		base.TokenBudget.AlertThreshold = override.TokenBudget.AlertThreshold
	}
	return base
}

// Environment variables the worker (and any embedder) may set to configure
// the runner-wide runtime defaults (issue #107 wiring). Unset or invalid
// values keep the safe defaults.
const (
	EnvHistoryStrategy        = "AGENTOS_RUNTIME_HISTORY_STRATEGY"
	EnvHistoryMaxTokens       = "AGENTOS_RUNTIME_HISTORY_MAX_TOKENS"
	EnvBudgetMaxTotalTokens   = "AGENTOS_RUNTIME_BUDGET_MAX_TOTAL_TOKENS"
	EnvBudgetMaxPromptTokens  = "AGENTOS_RUNTIME_BUDGET_MAX_PROMPT_TOKENS"
	EnvBudgetMaxCompletionTok = "AGENTOS_RUNTIME_BUDGET_MAX_COMPLETION_TOKENS"
	EnvBudgetAlertThreshold   = "AGENTOS_RUNTIME_BUDGET_ALERT_THRESHOLD"
)

// RuntimeConfigFromEnv builds the runner-wide default configuration from the
// AGENTOS_RUNTIME_* environment variables, starting from
// DefaultRuntimeConfig. Invalid values (unparseable numbers, unknown
// strategy) are ignored in favor of the defaults — env config must never
// prevent the worker from starting.
func RuntimeConfigFromEnv() RuntimeConfig {
	cfg := DefaultRuntimeConfig()
	if v := strings.TrimSpace(os.Getenv(EnvHistoryStrategy)); v != "" && validStrategy(TruncationStrategy(v)) {
		cfg.ConversationHistory.Strategy = TruncationStrategy(v)
	}
	if v, ok := envInt(EnvHistoryMaxTokens); ok {
		cfg.ConversationHistory.MaxTokens = v // negative = explicit disable
	}
	if v, ok := envInt(EnvBudgetMaxTotalTokens); ok && v >= 0 {
		cfg.TokenBudget.MaxTotalTokens = v
	}
	if v, ok := envInt(EnvBudgetMaxPromptTokens); ok && v >= 0 {
		cfg.TokenBudget.MaxPromptTokens = v
	}
	if v, ok := envInt(EnvBudgetMaxCompletionTok); ok && v >= 0 {
		cfg.TokenBudget.MaxCompletionTokens = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvBudgetAlertThreshold)); v != "" {
		if f, err := parseNonNegativeFloat(v); err == nil && f > 0 && f <= 1 {
			cfg.TokenBudget.AlertThreshold = f
		}
	}
	return cfg
}

// envInt parses an integer environment variable; ok is false when unset or
// invalid.
func envInt(key string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	n, err := parseIntStrict(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseIntStrict parses a base-10 integer without allocations beyond the
// error path.
func parseIntStrict(s string) (int, error) {
	n := 0
	neg := false
	for i, r := range s {
		switch {
		case r == '-' && i == 0:
			neg = true
		case r < '0' || r > '9':
			return 0, fmt.Errorf("invalid integer %q", s)
		default:
			n = n*10 + int(r-'0')
			if n > 1<<62 {
				return 0, fmt.Errorf("integer overflow %q", s)
			}
		}
	}
	if neg {
		n = -n
	}
	return n, nil
}

// parseNonNegativeFloat parses a float that must be >= 0.
func parseNonNegativeFloat(s string) (float64, error) {
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0, err
	}
	if f < 0 {
		return 0, fmt.Errorf("negative float %q", s)
	}
	return f, nil
}
