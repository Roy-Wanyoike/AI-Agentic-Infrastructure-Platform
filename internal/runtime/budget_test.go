package runtime

import (
	"errors"
	"testing"

	"agentos/internal/models"
)

// --- tracker construction ----------------------------------------------------

func TestNewTokenBudgetUnlimitedIsNil(t *testing.T) {
	if got := NewTokenBudget(TokenBudgetConfig{}); got != nil {
		t.Fatalf("all limits unset must disable tracking (nil), got %+v", got)
	}
}

func TestNewTokenBudgetDefaultsAndClamps(t *testing.T) {
	b := NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 100})
	if b.AlertThreshold != DefaultAlertThreshold {
		t.Fatalf("unset threshold should default to %v, got %v", DefaultAlertThreshold, b.AlertThreshold)
	}
	b2 := NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 100, AlertThreshold: 2})
	if b2.AlertThreshold != 1 {
		t.Fatalf("threshold above 1 must clamp to 1, got %v", b2.AlertThreshold)
	}
	b3 := NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 100, AlertThreshold: 0.5})
	if b3.AlertThreshold != 0.5 {
		t.Fatalf("explicit threshold must be honored, got %v", b3.AlertThreshold)
	}
}

// --- cumulative accounting -----------------------------------------------------

func TestTokenBudgetCumulativeAccounting(t *testing.T) {
	b := NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 1000, MaxPromptTokens: 700})
	s1 := b.Record(models.Usage{PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300})
	if s1.Exceeded != nil || s1.Alert != nil {
		t.Fatalf("first record must be clean: %+v", s1)
	}
	if s1.Snapshot.TotalTokens != 300 || s1.Snapshot.PromptTokens != 200 {
		t.Fatalf("snapshot must accumulate: %+v", s1.Snapshot)
	}
	s2 := b.Record(models.Usage{PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300})
	if s2.Exceeded != nil || s2.Alert != nil {
		t.Fatalf("second record at 600/1000 must still be clean: %+v", s2)
	}
	// 0.8 threshold of 1000 total: 600+200=800 crosses -> warn.
	s3 := b.Record(models.Usage{PromptTokens: 100, CompletionTokens: 100, TotalTokens: 200})
	if s3.Exceeded != nil {
		t.Fatalf("no limit exceeded yet: %+v", s3)
	}
	if s3.Alert == nil || s3.Alert.Phase != "warn" {
		t.Fatalf("800/1000 must trip the alert: %+v", s3)
	}
	if s3.Snapshot.TotalTokens != 800 {
		t.Fatalf("cumulative total must be 800, got %d", s3.Snapshot.TotalTokens)
	}
	// Warn is one-time.
	s4 := b.Record(models.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20})
	if s4.Alert != nil {
		t.Fatalf("alert must fire exactly once: %+v", s4)
	}
	if b.Exceeded() {
		t.Fatal("budget must not be exceeded at 820/1000")
	}
	// The prompt limit (700) trips before the total limit (1000).
	s5 := b.Record(models.Usage{PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300})
	if s5.Exceeded == nil || s5.Exceeded.Dimension != budgetDimensionPrompt || s5.Exceeded.Used != 710 {
		t.Fatalf("prompt limit must trip at 710: %+v", s5.Exceeded)
	}
}

func TestTokenBudgetHardStopTypedError(t *testing.T) {
	b := NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 100})
	s1 := b.Record(models.Usage{PromptTokens: 60, CompletionTokens: 30, TotalTokens: 90})
	if s1.Exceeded != nil {
		t.Fatalf("90/100 must not exceed: %+v", s1)
	}
	s2 := b.Record(models.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30})
	if s2.Exceeded == nil {
		t.Fatal("120/100 must hard-stop")
	}
	if !errors.Is(s2.Exceeded, ErrTokenBudgetExceeded) {
		t.Fatalf("typed error must match ErrTokenBudgetExceeded, got %v", s2.Exceeded)
	}
	if s2.Exceeded.Dimension != budgetDimensionTotal || s2.Exceeded.Used != 120 || s2.Exceeded.Limit != 100 {
		t.Fatalf("violation details wrong: %+v", s2.Exceeded)
	}
	if s2.Snapshot.Phase != "exceeded" || s2.Snapshot.Limit != budgetDimensionTotal {
		t.Fatalf("snapshot must carry the exceeded phase/limit: %+v", s2.Snapshot)
	}
	if !b.Exceeded() {
		t.Fatal("Exceeded must be sticky")
	}
	// Subsequent records stay exceeded (the loop stops immediately anyway).
	s3 := b.Record(models.Usage{PromptTokens: 1})
	if s3.Exceeded == nil {
		t.Fatal("budget must remain exceeded")
	}
}

func TestTokenBudgetDimensionOrderDeterministic(t *testing.T) {
	// Prompt, completion and total all violated at once: the fixed order
	// (prompt -> completion -> total) decides the reported dimension.
	b := NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 10, MaxPromptTokens: 10, MaxCompletionTokens: 10})
	s := b.Record(models.Usage{PromptTokens: 50, CompletionTokens: 50, TotalTokens: 100})
	if s.Exceeded == nil || s.Exceeded.Dimension != budgetDimensionPrompt {
		t.Fatalf("prompt must be reported first: %+v", s.Exceeded)
	}

	b = NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 10, MaxCompletionTokens: 10})
	s = b.Record(models.Usage{PromptTokens: 50, CompletionTokens: 50})
	if s.Exceeded == nil || s.Exceeded.Dimension != budgetDimensionCompletion {
		t.Fatalf("completion must precede total: %+v", s.Exceeded)
	}
}

func TestTokenBudgetNormalizesMissingTotal(t *testing.T) {
	// Provider reports prompt+completion but no total: accounting must not
	// under-count.
	b := NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 100})
	s := b.Record(models.Usage{PromptTokens: 60, CompletionTokens: 60})
	if s.Snapshot.TotalTokens != 120 {
		t.Fatalf("total must normalize to prompt+completion, got %d", s.Snapshot.TotalTokens)
	}
	if s.Exceeded == nil {
		t.Fatal("120/100 must exceed after normalization")
	}
}

func TestTokenBudgetNilSafe(t *testing.T) {
	var b *TokenBudget
	s := b.Record(models.Usage{PromptTokens: 100})
	if s.Exceeded != nil || s.Alert != nil {
		t.Fatalf("nil tracker must no-op: %+v", s)
	}
	if b.Exceeded() {
		t.Fatal("nil tracker is never exceeded")
	}
	if b.Snapshot().Phase != "ok" {
		t.Fatal("nil snapshot should be phase ok")
	}
}

func TestTokenBudgetAlertSuppressedWhenExceeded(t *testing.T) {
	// One record jumps straight past the limit: the violation annotation
	// carries everything, so no separate warn is emitted.
	b := NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 100})
	s := b.Record(models.Usage{PromptTokens: 200, CompletionTokens: 0, TotalTokens: 200})
	if s.Exceeded == nil || s.Alert != nil {
		t.Fatalf("exceeded record must suppress the alert: %+v", s)
	}
}

func TestTokenBudgetExactLimitBoundary(t *testing.T) {
	// Edge: used == limit is NOT a violation (strict >), but it crosses the
	// 0.8 alert; the NEXT token tips the run over.
	b := NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 100})
	s := b.Record(models.Usage{PromptTokens: 60, CompletionTokens: 40, TotalTokens: 100})
	if s.Exceeded != nil {
		t.Fatalf("used == limit must not exceed: %+v", s.Exceeded)
	}
	if s.Alert == nil || s.Alert.Phase != "warn" {
		t.Fatalf("used == limit must have crossed the alert: %+v", s.Alert)
	}
	s2 := b.Record(models.Usage{TotalTokens: 1})
	if s2.Exceeded == nil || s2.Exceeded.Used != 101 || s2.Exceeded.Limit != 100 {
		t.Fatalf("101/100 must exceed: %+v", s2.Exceeded)
	}
	// Exact-threshold alert on a dimension that never violates: 80/100 total.
	b2 := NewTokenBudget(TokenBudgetConfig{MaxTotalTokens: 100})
	s3 := b2.Record(models.Usage{PromptTokens: 80, CompletionTokens: 0, TotalTokens: 80})
	if s3.Exceeded != nil || s3.Alert == nil || s3.Alert.TotalTokens != 80 {
		t.Fatalf("exactly 0.8*limit must fire the alert once: %+v", s3)
	}
}

// --- runtime config document ---------------------------------------------------

func TestParseRuntimeConfigEmptyIsZero(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("   ")} {
		cfg, err := ParseRuntimeConfig(raw)
		if err != nil {
			t.Fatalf("empty config must parse clean: %v", err)
		}
		if cfg != (RuntimeConfig{}) {
			t.Fatalf("empty config must be the zero value, got %+v", cfg)
		}
	}
}

func TestParseRuntimeConfigValidDocument(t *testing.T) {
	raw := []byte(`{
                "conversation_history": {"strategy": "summarize_older", "max_tokens": 2000},
                "token_budget": {"max_total_tokens": 50000, "max_prompt_tokens": 40000, "max_completion_tokens": 10000, "alert_threshold": 0.9}
        }`)
	cfg, err := ParseRuntimeConfig(raw)
	if err != nil {
		t.Fatalf("valid document must parse: %v", err)
	}
	if cfg.ConversationHistory.Strategy != StrategySummarizeOlder || cfg.ConversationHistory.MaxTokens != 2000 {
		t.Fatalf("conversation_history not parsed: %+v", cfg.ConversationHistory)
	}
	if cfg.TokenBudget.MaxTotalTokens != 50000 || cfg.TokenBudget.MaxPromptTokens != 40000 ||
		cfg.TokenBudget.MaxCompletionTokens != 10000 || cfg.TokenBudget.AlertThreshold != 0.9 {
		t.Fatalf("token_budget not parsed: %+v", cfg.TokenBudget)
	}
}

func TestParseRuntimeConfigValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"malformed json", `{"conversation_history":`},
		{"unknown strategy", `{"conversation_history": {"strategy": "magic"}}`},
		{"negative limit", `{"token_budget": {"max_total_tokens": -5}}`},
		{"threshold too high", `{"token_budget": {"alert_threshold": 1.5}}`},
		{"threshold negative", `{"token_budget": {"alert_threshold": -0.1}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseRuntimeConfig([]byte(c.raw)); err == nil {
				t.Fatalf("%s must be rejected", c.name)
			}
		})
	}
}

func TestRuntimeConfigWithDefaults(t *testing.T) {
	cfg := RuntimeConfig{}.WithDefaults()
	if cfg.ConversationHistory.Strategy != DefaultHistoryStrategy ||
		cfg.ConversationHistory.MaxTokens != DefaultHistoryMaxTokens {
		t.Fatalf("history defaults wrong: %+v", cfg.ConversationHistory)
	}
	if cfg.TokenBudget.AlertThreshold != DefaultAlertThreshold {
		t.Fatalf("alert threshold default wrong: %+v", cfg.TokenBudget)
	}
	// Explicit values survive; explicit disable (negative) survives.
	cfg2 := RuntimeConfig{
		ConversationHistory: ConversationHistoryConfig{MaxTokens: -1},
		TokenBudget:         TokenBudgetConfig{AlertThreshold: 0.5},
	}.WithDefaults()
	if cfg2.ConversationHistory.MaxTokens != -1 || cfg2.ConversationHistory.Strategy != DefaultHistoryStrategy {
		t.Fatalf("negative max_tokens is an explicit disable and must survive: %+v", cfg2.ConversationHistory)
	}
	if cfg2.TokenBudget.AlertThreshold != 0.5 {
		t.Fatalf("explicit threshold must survive: %+v", cfg2.TokenBudget)
	}
}

func TestMergeRuntimeConfigOverridesOnlySetFields(t *testing.T) {
	base := RuntimeConfig{
		ConversationHistory: ConversationHistoryConfig{Strategy: StrategyDropOldest, MaxTokens: 1234},
		TokenBudget:         TokenBudgetConfig{MaxTotalTokens: 1000, AlertThreshold: 0.9},
	}
	merged := mergeRuntimeConfig(base, RuntimeConfig{
		ConversationHistory: ConversationHistoryConfig{MaxTokens: 42},
		TokenBudget:         TokenBudgetConfig{MaxTotalTokens: 7},
	})
	if merged.ConversationHistory.Strategy != StrategyDropOldest {
		t.Fatal("unset override strategy must keep the base")
	}
	if merged.ConversationHistory.MaxTokens != 42 {
		t.Fatal("set override max_tokens must win")
	}
	if merged.TokenBudget.MaxTotalTokens != 7 {
		t.Fatal("set override budget limit must win")
	}
	if merged.TokenBudget.AlertThreshold != 0.9 {
		t.Fatal("unset override threshold must keep the base")
	}
}

func TestRuntimeConfigFromEnvDefaultsAndOverrides(t *testing.T) {
	// Defaults without any env set.
	cfg := RuntimeConfigFromEnv()
	want := DefaultRuntimeConfig()
	if cfg != want {
		t.Fatalf("expected defaults %+v, got %+v", want, cfg)
	}

	t.Setenv(EnvHistoryStrategy, "drop_oldest")
	t.Setenv(EnvHistoryMaxTokens, "555")
	t.Setenv(EnvBudgetMaxTotalTokens, "90000")
	t.Setenv(EnvBudgetMaxPromptTokens, "60000")
	t.Setenv(EnvBudgetMaxCompletionTok, "30000")
	t.Setenv(EnvBudgetAlertThreshold, "0.95")
	cfg = RuntimeConfigFromEnv().WithDefaults()
	if cfg.ConversationHistory.Strategy != StrategyDropOldest || cfg.ConversationHistory.MaxTokens != 555 {
		t.Fatalf("env history config not applied: %+v", cfg.ConversationHistory)
	}
	if cfg.TokenBudget.MaxTotalTokens != 90000 || cfg.TokenBudget.MaxPromptTokens != 60000 ||
		cfg.TokenBudget.MaxCompletionTokens != 30000 || cfg.TokenBudget.AlertThreshold != 0.95 {
		t.Fatalf("env budget config not applied: %+v", cfg.TokenBudget)
	}

	// Invalid values keep the defaults.
	t.Setenv(EnvHistoryStrategy, "bogus")
	t.Setenv(EnvHistoryMaxTokens, "not-a-number")
	t.Setenv(EnvBudgetMaxTotalTokens, "-3")
	t.Setenv(EnvBudgetAlertThreshold, "2.5")
	cfg = RuntimeConfigFromEnv()
	if cfg.ConversationHistory.Strategy != DefaultHistoryStrategy ||
		cfg.ConversationHistory.MaxTokens != DefaultHistoryMaxTokens ||
		cfg.TokenBudget.MaxTotalTokens != DefaultRuntimeConfig().TokenBudget.MaxTotalTokens ||
		cfg.TokenBudget.AlertThreshold != DefaultAlertThreshold {
		t.Fatalf("invalid env values must keep defaults, got %+v", cfg)
	}
}
