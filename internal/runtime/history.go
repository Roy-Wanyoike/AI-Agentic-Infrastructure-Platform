// history.go — issue #107: conversation history management for the agent
// runtime. Long model-in-the-loop conversations grow without bound (every
// step appends the assistant turn and the tool observation), so the prompts
// sent to the provider are budgeted: the history is truncated to a
// configurable token budget using one of three deterministic strategies.
//
// Token estimation uses the standard ~4-characters-per-token heuristic
// (see EstimateTokens). Estimation is for BUDGETING only — billing and
// enforcement always use the provider-reported usage (TokenBudget,
// budget.go), and the two systems are intentionally independent: the history
// budget sizes the request BEFORE the call, the token budget accounts the
// usage reported AFTER the call.
//
// Strategies (TruncationStrategy):
//
//   - sliding_window (default): the leading system messages are always
//     preserved, then the most recent messages that fit the remaining
//     budget are kept; older messages are dropped.
//   - drop_oldest: the same newest-first fit applied to ALL messages with
//     NO system-preservation nuance — an old system message is dropped like
//     any other. Callers who rely on system prompts should prefer
//     sliding_window (the runtime passes its system prompt separately, so
//     the difference only matters for histories that carry system messages
//     inline).
//   - summarize_older: the most recent messages that fit are kept and every
//     dropped older message is compressed into one synthetic
//     "Summary of earlier conversation: ..." message placed right after the
//     preserved system block. The default summarization is MECHANICAL and
//     deterministic (truncated role+content snippets, no model call); a
//     model-backed summarizer can be injected via Summarizer /
//     WithHistorySummarizer, and its failures degrade to the mechanical
//     summary (a summarizer must never break a run).
//
// Guaranteed invariants for every strategy:
//   - the returned view is never empty while the full history is non-empty:
//     the newest message is always kept even when it alone exceeds the whole
//     budget (truncation must never make a run contextless — a provider call
//     without messages would fail);
//   - the stored Messages field is never mutated: Prompt returns a new slice
//     every call;
//   - the result is a deterministic function of (Messages, MaxTokens,
//     Strategy, summarizer output).
package runtime

import (
	"context"
	"strings"

	"agentos/internal/models"
)

// TruncationStrategy selects how a conversation is shortened to fit its
// token budget. The zero value is not valid; NewConversationHistory and the
// runtime config parsing map it to StrategySlidingWindow.
type TruncationStrategy string

// Supported truncation strategies.
const (
	// StrategySlidingWindow keeps the leading system messages plus the most
	// recent messages that fit the budget (default).
	StrategySlidingWindow TruncationStrategy = "sliding_window"
	// StrategySummarizeOlder keeps the recent window and compresses every
	// dropped older message into one synthetic summary message.
	StrategySummarizeOlder TruncationStrategy = "summarize_older"
	// StrategyDropOldest keeps the most recent messages that fit the budget
	// without preserving system messages (documented nuance; see the file
	// header).
	StrategyDropOldest TruncationStrategy = "drop_oldest"
)

// validStrategy reports whether s is one of the pinned strategies.
func validStrategy(s TruncationStrategy) bool {
	switch s {
	case StrategySlidingWindow, StrategySummarizeOlder, StrategyDropOldest:
		return true
	}
	return false
}

// history truncation knobs. DefaultHistoryMaxTokens is generous enough that
// short conversations are untouched (backwards-compatible default) while
// runaway tool loops still get bounded.
const (
	// DefaultHistoryMaxTokens is the default conversation budget (issue #107
	// safe default).
	DefaultHistoryMaxTokens = 8000
	// DefaultHistoryStrategy is the default truncation strategy.
	DefaultHistoryStrategy = StrategySlidingWindow
	// summaryRole is the role used for the synthetic summary message. "user"
	// is the only role every provider accepts inside the message list (some
	// providers reject a second "system" message or a bare "tool" turn), and
	// the summary reads naturally as injected context in the user channel.
	summaryRole = "user"
	// summaryPrefix opens every synthetic summary message.
	summaryPrefix = "Summary of earlier conversation: "
	// snippetChars caps each message's contribution to the mechanical digest.
	snippetChars = 64
	// summaryEllipsis marks a truncated digest.
	summaryEllipsis = "...[truncated]"
)

// EstimateTokens applies the standard ~4-characters-per-token heuristic:
// (len+3)/4 rounds UP so a partial trailing token is never under-counted.
// It is deterministic and dependency-free. Used for history budgeting only
// (see the file header); billing/enforcement use provider-reported usage.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// EstimateMessageTokens estimates one conversation message: the content plus
// the small framing overhead of the role and (for tool turns) the tool name,
// each rounded up independently so the estimate is conservative (never
// under-counts the pieces it models).
func EstimateMessageTokens(m models.Message) int {
	return EstimateTokens(m.Content) + EstimateTokens(m.Role) + EstimateTokens(m.Name)
}

// estimateMessages sums EstimateMessageTokens over a message slice.
func estimateMessages(msgs []models.Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateMessageTokens(m)
	}
	return total
}

// Summarizer produces a summary for older conversation turns behind
// summarize_older. The runtime ships MechanicalSummarizer (deterministic, no
// model call) as the default; a model-backed implementation may be injected
// via WithHistorySummarizer. Implementations must return quickly and must
// not mutate the input.
type Summarizer interface {
	// Summarize renders the given (already dropped) messages as one summary
	// string. Errors are non-fatal: the caller degrades to the mechanical
	// summary.
	Summarize(ctx context.Context, messages []models.Message) (string, error)
}

// MechanicalSummarizer is the deterministic default Summarizer: it
// concatenates truncated "role: content" snippets of every dropped message.
// It never fails and never calls a model.
type MechanicalSummarizer struct{}

// Summarize implements Summarizer mechanically (see mechanicalSummary).
func (MechanicalSummarizer) Summarize(_ context.Context, messages []models.Message) (string, error) {
	return mechanicalSummary(messages), nil
}

// SummarizerFunc adapts a plain function to Summarizer.
type SummarizerFunc func(ctx context.Context, messages []models.Message) (string, error)

// Summarize implements Summarizer.
func (f SummarizerFunc) Summarize(ctx context.Context, messages []models.Message) (string, error) {
	return f(ctx, messages)
}

// mechanicalSummary renders the digest: "user: hello... | assistant: hi...".
// Each message contributes role and at most the first snippetChars
// characters of its content, so the digest is bounded and deterministic.
func mechanicalSummary(messages []models.Message) string {
	if len(messages) == 0 {
		return ""
	}
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		role := m.Role
		if role == "" {
			role = "message"
		}
		label := role
		if m.Role == "tool" && strings.TrimSpace(m.Name) != "" {
			label = role + "(" + m.Name + ")"
		}
		parts = append(parts, label+": "+snippet(m.Content, snippetChars))
	}
	return strings.Join(parts, " | ")
}

// snippet returns the first max characters of s, appending summaryEllipsis
// when it had to cut.
func snippet(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty)"
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + summaryEllipsis
}

// ConversationHistory is the bounded conversation state of one run: the full
// message list plus the budget and strategy used to derive the prompt view.
// Append records turns in full; Prompt returns the budgeted view that is
// actually sent to the provider. The zero value is usable (defaults apply),
// but NewConversationHistory is the intended constructor.
//
// A ConversationHistory is single-run state: it is not safe for concurrent
// use (the runtime loop is single-goroutine per run).
type ConversationHistory struct {
	// Messages is the FULL conversation (never truncated, never mutated by
	// Prompt). System messages, when present, lead the slice.
	Messages []models.Message
	// MaxTokens is the prompt budget. 0 applies DefaultHistoryMaxTokens;
	// a negative value disables history management entirely (Prompt returns
	// the full history) for agents that manage context themselves.
	MaxTokens int
	// Strategy selects the truncation strategy; the zero value and any
	// unknown value fall back to StrategySlidingWindow.
	Strategy TruncationStrategy
	// Summarizer optionally overrides the mechanical summary behind
	// summarize_older (nil = MechanicalSummarizer).
	Summarizer Summarizer
}

// NewConversationHistory returns a history with the safe defaults applied:
// maxTokens==0 -> DefaultHistoryMaxTokens; an empty/unknown strategy ->
// DefaultHistoryStrategy. A negative maxTokens disables truncation.
func NewConversationHistory(maxTokens int, strategy TruncationStrategy) *ConversationHistory {
	if maxTokens == 0 {
		maxTokens = DefaultHistoryMaxTokens
	}
	if !validStrategy(strategy) {
		strategy = DefaultHistoryStrategy
	}
	return &ConversationHistory{MaxTokens: maxTokens, Strategy: strategy}
}

// Append adds messages to the full history. It never truncates: truncation
// happens only in the Prompt view.
func (h *ConversationHistory) Append(msgs ...models.Message) {
	if h == nil {
		return
	}
	h.Messages = append(h.Messages, msgs...)
}

// Tokens reports the estimated token count of the FULL history.
func (h *ConversationHistory) Tokens() int {
	if h == nil {
		return 0
	}
	return estimateMessages(h.Messages)
}

// Disabled reports whether history management is switched off (negative
// MaxTokens): Prompt then returns the full history unchanged.
func (h *ConversationHistory) Disabled() bool {
	return h != nil && h.MaxTokens < 0
}

// Prompt returns the messages to send to the provider under the configured
// budget and strategy. It never mutates h.Messages and never returns a
// non-empty result for an empty history. The ctx parameter exists for
// model-backed summarizers (summarize_older); the mechanical default ignores
// it. A summarizer failure (or a nil/empty summary) degrades to the
// mechanical summary — truncation must never break a run.
func (h *ConversationHistory) Prompt(ctx context.Context) []models.Message {
	if h == nil || len(h.Messages) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if h.Disabled() {
		return append([]models.Message(nil), h.Messages...)
	}
	max := h.MaxTokens
	if max == 0 {
		max = DefaultHistoryMaxTokens
	}
	strategy := h.Strategy
	if !validStrategy(strategy) {
		strategy = DefaultHistoryStrategy
	}

	// drop_oldest applies the pure newest-first fit to ALL messages with no
	// system-preservation nuance (documented behavior).
	if strategy == StrategyDropOldest {
		kept, _ := fitNewest(h.Messages, max)
		return cloneMessages(kept)
	}

	// sliding_window / summarize_older preserve the leading system block and
	// spend the rest of the budget on the most recent turns.
	sysEnd := 0
	for sysEnd < len(h.Messages) && h.Messages[sysEnd].Role == "system" {
		sysEnd++
	}
	system := h.Messages[:sysEnd]
	rest := h.Messages[sysEnd:]
	sysTokens := estimateMessages(system)

	budget := max - sysTokens
	if budget < 0 {
		// The system block alone exceeds the budget: keep it verbatim (the
		// agent's instructions must survive) and still return at least the
		// newest message (fitNewest's keep-newest guarantee).
		budget = 0
	}
	kept, dropped := fitNewest(rest, budget)
	if strategy == StrategySlidingWindow || len(dropped) == 0 {
		return cloneMessages(append(append([]models.Message(nil), system...), kept...))
	}

	// summarize_older: build the synthetic summary for the dropped turns and
	// cap it to the budget room that remains after system + window.
	out := append([]models.Message(nil), system...)
	summaryRoom := max - sysTokens - estimateMessages(kept)
	if summary := h.summarize(ctx, dropped); summary != "" && summaryRoom > 0 {
		summary = capSummary(summary, summaryRoom)
		if summary != "" {
			out = append(out, models.Message{Role: summaryRole, Content: summaryPrefix + summary})
		}
	}
	return append(out, kept...)
}

// summarize produces the digest for the dropped messages: the injected
// Summarizer when wired, the mechanical default otherwise or on any failure.
func (h *ConversationHistory) summarize(ctx context.Context, dropped []models.Message) string {
	if h != nil && h.Summarizer != nil {
		if s, err := h.Summarizer.Summarize(ctx, dropped); err == nil && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return mechanicalSummary(dropped)
}

// capSummary trims a digest so summaryPrefix + digest fits within maxTokens
// estimated tokens (chars ~= 4*tokens). Returns "" when there is no room at
// all (the caller then omits the summary, degrading to a sliding window).
func capSummary(summary string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	maxChars := maxTokens * 4 // inverse of the chars/4 heuristic
	full := summaryPrefix + summary
	if len(full) <= maxChars {
		return summary
	}
	keep := maxChars - len(summaryPrefix) - len(summaryEllipsis)
	if keep <= 0 {
		return ""
	}
	return summary[:keep] + summaryEllipsis
}

// fitNewest walks messages newest-first and keeps the longest contiguous
// tail whose estimated tokens fit within budget. The NEWEST message is
// always kept even when it alone exceeds the whole budget (a truncation must
// never make the conversation empty). Returns the kept tail and the dropped
// older prefix.
func fitNewest(msgs []models.Message, budget int) (kept, dropped []models.Message) {
	if len(msgs) == 0 {
		return nil, nil
	}
	total := EstimateMessageTokens(msgs[len(msgs)-1]) // newest always kept
	n := 1
	for i := len(msgs) - 2; i >= 0; i-- {
		t := EstimateMessageTokens(msgs[i])
		if total+t > budget {
			break
		}
		total += t
		n++
	}
	return msgs[len(msgs)-n:], msgs[:len(msgs)-n]
}

// cloneMessages copies a message slice (shallow: messages are value types).
func cloneMessages(msgs []models.Message) []models.Message {
	out := make([]models.Message, len(msgs))
	copy(out, msgs)
	return out
}
