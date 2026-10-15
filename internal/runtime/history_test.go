package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentos/internal/models"
)

// NOTE on arithmetic: EstimateMessageTokens counts the content AND the role
// (and tool name) with per-piece round-up, so a 40-char "user" message is
// 10+1=11 tokens. The tests below are sized against that estimator.

// --- estimator ------------------------------------------------------------

func TestEstimateTokensCharsPerFour(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2}, // 5 chars round up to 2 tokens
		{"abcdefgh", 2},
		{"abcdefghi", 3},
	}
	for _, c := range cases {
		if got := EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	// Determinism: same input, same output.
	if EstimateTokens("deterministic input") != EstimateTokens("deterministic input") {
		t.Error("estimator must be deterministic")
	}
}

func TestEstimateMessageTokensNeverUndercounts(t *testing.T) {
	m := models.Message{Role: "user", Content: "hello world", Name: "calc"}
	// 11 chars content -> 3, role 4 chars -> 1, name 4 chars -> 1.
	if got := EstimateMessageTokens(m); got != 5 {
		t.Fatalf("EstimateMessageTokens = %d, want 5", got)
	}
	if got := EstimateMessageTokens(models.Message{}); got != 0 {
		t.Fatalf("empty message should estimate 0, got %d", got)
	}
}

// --- constructor defaults --------------------------------------------------

func TestNewConversationHistoryDefaults(t *testing.T) {
	h := NewConversationHistory(0, "")
	if h.MaxTokens != DefaultHistoryMaxTokens {
		t.Fatalf("expected default max tokens %d, got %d", DefaultHistoryMaxTokens, h.MaxTokens)
	}
	if h.Strategy != StrategySlidingWindow {
		t.Fatalf("expected default strategy sliding_window, got %q", h.Strategy)
	}
	// Unknown strategies fall back to the default.
	h2 := NewConversationHistory(100, TruncationStrategy("nonsense"))
	if h2.Strategy != StrategySlidingWindow {
		t.Fatalf("unknown strategy should fall back to sliding_window, got %q", h2.Strategy)
	}
	// Explicit values are honored.
	h3 := NewConversationHistory(50, StrategySummarizeOlder)
	if h3.MaxTokens != 50 || h3.Strategy != StrategySummarizeOlder {
		t.Fatalf("explicit config not honored: %+v", h3)
	}
}

// --- sliding window ---------------------------------------------------------

func TestSlidingWindowKeepsSystemAndRecent(t *testing.T) {
	h := NewConversationHistory(40, StrategySlidingWindow)
	// system: 40 chars -> 12 tokens (content 10 + role 2); each turn: 40
	// chars -> 11 tokens. Budget for the window: 40-12 = 28 -> keeps the two
	// newest turns (22) and drops the two oldest.
	h.Messages = []models.Message{
		{Role: "system", Content: strings.Repeat("s", 40)},
		{Role: "user", Content: strings.Repeat("u1", 20)},
		{Role: "assistant", Content: strings.Repeat("a1", 20)},
		{Role: "user", Content: strings.Repeat("u2", 20)},
		{Role: "assistant", Content: strings.Repeat("a2", 20)},
		{Role: "user", Content: strings.Repeat("u3", 20)},
	}
	view := h.Prompt(context.Background())
	if len(view) == 0 {
		t.Fatal("view must never be empty for a non-empty history")
	}
	if view[0].Role != "system" || view[0].Content != h.Messages[0].Content {
		t.Fatalf("system message must be preserved verbatim, got %+v", view[0])
	}
	if view[len(view)-1].Content != h.Messages[len(h.Messages)-1].Content {
		t.Fatal("newest message must always be kept")
	}
	if got := estimateMessages(view); got > h.MaxTokens {
		t.Fatalf("view exceeds budget: %d > %d", got, h.MaxTokens)
	}
	// Something old was actually dropped (the window is shorter than the input).
	if len(view) >= len(h.Messages) {
		t.Fatalf("expected truncation, view has %d of %d messages", len(view), len(h.Messages))
	}
	// The full history is untouched.
	if len(h.Messages) != 6 {
		t.Fatalf("Prompt must not mutate Messages; got %d messages", len(h.Messages))
	}
}

func TestSlidingWindowSystemAloneOverBudgetStillKept(t *testing.T) {
	// A system message bigger than the whole budget is still preserved (the
	// agent's instructions must survive) and the newest message is still
	// returned (never an empty conversation).
	h := NewConversationHistory(5, StrategySlidingWindow)
	h.Messages = []models.Message{
		{Role: "system", Content: strings.Repeat("s", 400)},
		{Role: "user", Content: strings.Repeat("a", 40)},
		{Role: "user", Content: strings.Repeat("b", 40)},
	}
	view := h.Prompt(context.Background())
	if len(view) != 2 || view[0].Role != "system" || view[1].Content != h.Messages[2].Content {
		t.Fatalf("expected [system, newest], got %+v", view)
	}
}

func TestSlidingWindowZeroMaxTokensAppliesDefault(t *testing.T) {
	h := &ConversationHistory{Strategy: StrategySlidingWindow} // MaxTokens 0 -> default
	h.Messages = []models.Message{{Role: "user", Content: strings.Repeat("x", 100)}}
	view := h.Prompt(context.Background())
	if len(view) != 1 {
		t.Fatalf("small conversation under the default budget must be untouched, got %d", len(view))
	}
}

// --- drop oldest -------------------------------------------------------------

func TestDropOldestDropsSystemWithoutPreservation(t *testing.T) {
	h := NewConversationHistory(22, StrategyDropOldest)
	// system: 12 tokens; two user turns: 11 tokens each. Budget 22: the two
	// user turns fit exactly (22) and the SYSTEM message — the oldest, with
	// no preservation nuance in drop_oldest — is the one dropped.
	h.Messages = []models.Message{
		{Role: "system", Content: strings.Repeat("s", 40)},
		{Role: "user", Content: strings.Repeat("a", 40)},
		{Role: "user", Content: strings.Repeat("b", 40)},
	}
	view := h.Prompt(context.Background())
	if len(view) != 2 {
		t.Fatalf("expected [u1, u2] kept, got %d messages: %+v", len(view), view)
	}
	for _, m := range view {
		if m.Role == "system" {
			t.Fatal("drop_oldest must not special-case system messages")
		}
	}
	if view[len(view)-1].Content != h.Messages[2].Content {
		t.Fatal("newest message must survive drop_oldest")
	}
}

// --- summarize older ----------------------------------------------------------

func TestSummarizeOlderCompressesDroppedTurns(t *testing.T) {
	// u1/a1: 400 chars -> 101 tokens each; u2 (recent): 160 chars -> 41
	// tokens. Budget 141: the two old turns cannot fit after u2 (41+101 >
	// 141), so both are summarized; the digest (~45 tokens) fits in the
	// remaining room (100).
	h := NewConversationHistory(141, StrategySummarizeOlder)
	h.Messages = []models.Message{
		{Role: "user", Content: strings.Repeat("a", 400)},
		{Role: "assistant", Content: strings.Repeat("b", 400)},
		{Role: "user", Content: strings.Repeat("z", 160)},
	}
	view := h.Prompt(context.Background())
	if len(view) == 0 {
		t.Fatal("view must not be empty")
	}
	var summary *models.Message
	for i := range view {
		if strings.HasPrefix(view[i].Content, summaryPrefix) {
			summary = &view[i]
			break
		}
	}
	if summary == nil {
		t.Fatalf("expected a synthetic summary message in the view: %+v", view)
	}
	if summary.Role != summaryRole {
		t.Fatalf("summary role should be %q, got %q", summaryRole, summary.Role)
	}
	// The mechanical digest names the dropped roles.
	if !strings.Contains(summary.Content, "user:") || !strings.Contains(summary.Content, "assistant:") {
		t.Fatalf("summary should carry truncated role+content snippets: %q", summary.Content)
	}
	// The recent window survives after the summary.
	if view[len(view)-1].Content != h.Messages[2].Content {
		t.Fatal("recent messages must survive after the summary")
	}
	if got := estimateMessages(view); got > h.MaxTokens {
		t.Fatalf("summary view exceeds budget: %d > %d", got, h.MaxTokens)
	}
	// Deterministic: two prompts produce byte-identical views.
	first := h.Prompt(context.Background())
	second := h.Prompt(context.Background())
	if len(first) != len(second) {
		t.Fatal("summary view must be deterministic in length")
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("summary view must be deterministic: %v vs %v", first[i], second[i])
		}
	}
	// The full history is untouched.
	if len(h.Messages) != 3 {
		t.Fatalf("Prompt must not mutate Messages; got %d", len(h.Messages))
	}
}

func TestSummarizeOlderPreservesSystem(t *testing.T) {
	// system: 12 tokens; u1: 400 chars -> 101; u2 (recent): 40 chars -> 11.
	// Budget 120: window budget 120-12=110 cannot fit u1 after u2 (112), so
	// u1 is summarized and the system message leads the view.
	h := NewConversationHistory(120, StrategySummarizeOlder)
	h.Messages = []models.Message{
		{Role: "system", Content: strings.Repeat("s", 40)},
		{Role: "user", Content: strings.Repeat("a", 400)},
		{Role: "user", Content: strings.Repeat("z", 40)},
	}
	view := h.Prompt(context.Background())
	if len(view) != 3 {
		t.Fatalf("expected [system, summary, u2], got %+v", view)
	}
	if view[0].Role != "system" {
		t.Fatalf("system must lead the summarize_older view, got %+v", view[0])
	}
	if !strings.HasPrefix(view[1].Content, summaryPrefix) {
		t.Fatalf("expected summary in the middle, got %+v", view[1])
	}
	if view[2].Content != h.Messages[2].Content {
		t.Fatal("recent message must survive")
	}
	if got := estimateMessages(view); got > h.MaxTokens {
		t.Fatalf("view exceeds budget: %d > %d", got, h.MaxTokens)
	}
}

func TestSummarizeOlderInjectsModelBackedSummarizer(t *testing.T) {
	h := NewConversationHistory(141, StrategySummarizeOlder)
	h.Messages = []models.Message{
		{Role: "user", Content: strings.Repeat("a", 400)},
		{Role: "assistant", Content: strings.Repeat("b", 400)},
		{Role: "user", Content: strings.Repeat("z", 160)},
	}
	h.Summarizer = SummarizerFunc(func(ctx context.Context, msgs []models.Message) (string, error) {
		if len(msgs) != 2 {
			t.Errorf("summarizer should receive the 2 dropped messages, got %d", len(msgs))
		}
		return "MODEL SUMMARY", nil
	})
	view := h.Prompt(context.Background())
	found := false
	for _, m := range view {
		if strings.Contains(m.Content, summaryPrefix+"MODEL SUMMARY") {
			found = true
		}
	}
	if !found {
		t.Fatalf("injected summarizer output must appear in the view: %+v", view)
	}
}

func TestSummarizeOlderSummarizerFailureFallsBackToMechanical(t *testing.T) {
	h := NewConversationHistory(141, StrategySummarizeOlder)
	h.Messages = []models.Message{
		{Role: "user", Content: strings.Repeat("a", 400)},
		{Role: "user", Content: strings.Repeat("z", 160)},
	}
	h.Summarizer = SummarizerFunc(func(ctx context.Context, msgs []models.Message) (string, error) {
		return "", errors.New("model down")
	})
	view := h.Prompt(context.Background())
	found := false
	for _, m := range view {
		if strings.HasPrefix(m.Content, summaryPrefix) && strings.Contains(m.Content, "user:") {
			found = true // mechanical digest
		}
	}
	if !found {
		t.Fatalf("summarizer failure must degrade to the mechanical summary: %+v", view)
	}
}

func TestSummarizeOlderNoRoomOmitsSummary(t *testing.T) {
	// Budget barely fits the recent message: no room for a summary -> the
	// result degrades to the sliding window (no synthetic message).
	h := NewConversationHistory(11, StrategySummarizeOlder)
	h.Messages = []models.Message{
		{Role: "user", Content: strings.Repeat("a", 200)}, // 51 tokens
		{Role: "user", Content: strings.Repeat("z", 40)},  // newest: 11 tokens
	}
	view := h.Prompt(context.Background())
	if len(view) != 1 {
		t.Fatalf("expected only the newest message, got %+v", view)
	}
	for _, m := range view {
		if strings.HasPrefix(m.Content, summaryPrefix) {
			t.Fatal("summary must be omitted when the budget has no room")
		}
	}
}

// --- edge cases ----------------------------------------------------------------

func TestPromptEmptyHistory(t *testing.T) {
	h := NewConversationHistory(100, StrategySlidingWindow)
	if got := h.Prompt(context.Background()); got != nil {
		t.Fatalf("empty history should prompt nil, got %+v", got)
	}
	var nilHistory *ConversationHistory
	if got := nilHistory.Prompt(context.Background()); got != nil {
		t.Fatalf("nil history should prompt nil, got %+v", got)
	}
	if nilHistory.Tokens() != 0 {
		t.Fatal("nil history tokens should be 0")
	}
	nilHistory.Append(models.Message{Role: "user", Content: "x"}) // must not panic
}

func TestPromptOversizedSingleMessageKeptVerbatim(t *testing.T) {
	h := NewConversationHistory(10, StrategySlidingWindow)
	big := strings.Repeat("B", 400) // ~101 tokens, way over the budget
	h.Messages = []models.Message{
		{Role: "user", Content: strings.Repeat("a", 40)},
		{Role: "user", Content: big},
	}
	view := h.Prompt(context.Background())
	if len(view) != 1 {
		t.Fatalf("expected exactly the oversized newest message, got %d", len(view))
	}
	if view[0].Content != big {
		t.Fatal("the oversized newest message must be kept verbatim (never an empty conversation)")
	}
}

func TestPromptDisabledHistoryReturnsEverything(t *testing.T) {
	h := NewConversationHistory(-1, StrategySlidingWindow) // negative = disabled
	h.Messages = []models.Message{
		{Role: "user", Content: strings.Repeat("a", 400)},
		{Role: "user", Content: strings.Repeat("b", 400)},
	}
	view := h.Prompt(context.Background())
	if len(view) != 2 || !h.Disabled() {
		t.Fatalf("disabled history must return the full conversation, got %d (disabled=%v)", len(view), h.Disabled())
	}
}

func TestPromptDoesNotAliasStoredMessages(t *testing.T) {
	h := NewConversationHistory(1000, StrategySlidingWindow)
	h.Append(models.Message{Role: "user", Content: "one"})
	view := h.Prompt(context.Background())
	view[0].Content = "mutated"
	if h.Messages[0].Content != "one" {
		t.Fatal("mutating the Prompt view must not touch the stored history")
	}
	// Appending after prompting keeps the accounting consistent.
	h.Append(models.Message{Role: "assistant", Content: "two"})
	if h.Tokens() != estimateMessages(h.Messages) {
		t.Fatal("Tokens must track the full history")
	}
}

func TestMechanicalSummaryShape(t *testing.T) {
	digest := mechanicalSummary([]models.Message{
		{Role: "user", Content: strings.Repeat("u", 100)},
		{Role: "tool", Name: "calculator", Content: `{"result":42}`},
		{Role: "assistant", Content: "   "},
	})
	if !strings.Contains(digest, "user: "+strings.Repeat("u", snippetChars)+summaryEllipsis) {
		t.Fatalf("digest should truncate long content with the ellipsis marker: %q", digest)
	}
	if !strings.Contains(digest, "tool(calculator): {\"result\":42}") {
		t.Fatalf("tool turns should carry the tool name: %q", digest)
	}
	if !strings.Contains(digest, "assistant: (empty)") {
		t.Fatalf("blank content should render as (empty): %q", digest)
	}
	if mechanicalSummary(nil) != "" {
		t.Fatal("digest of nothing is empty")
	}
}

func TestCapSummary(t *testing.T) {
	if got := capSummary("short", 100); got != "short" {
		t.Fatalf("roomy budget must keep the digest verbatim, got %q", got)
	}
	if got := capSummary("short", 0); got != "" {
		t.Fatalf("no room must omit the summary, got %q", got)
	}
	capped := capSummary(strings.Repeat("x", 1000), 30)
	if !strings.HasSuffix(capped, summaryEllipsis) {
		t.Fatalf("capped digest must carry the ellipsis marker: %q", capped)
	}
	if EstimateTokens(summaryPrefix+capped) > 30 {
		t.Fatalf("capped digest exceeds its budget: %d", EstimateTokens(summaryPrefix+capped))
	}
}
