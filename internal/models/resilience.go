package models

// resilience.go — issue #128: LLM response resilience layer.
//
// This file carries the four resilience concerns the OpenAI provider's
// response path weaves in additively (see openai.go Complete):
//
//  1. Wire-format capability detection (WireCapabilities): per-model facts
//     the RESPONSE path needs — whether the model accepts
//     response_format=json_object, whether it requires
//     max_completion_tokens instead of max_tokens (gpt-5+ and the o-series),
//     whether it is a reasoning model, and its context window. Resolved from
//     a name-based table + operator overrides (AGENTOS_WIRE_MODELS_JSON),
//     deterministic prefix rules for unknown variants, and a conservative
//     unknown default. Deliberately separate from the ROUTING capability
//     registry in capabilities.go: that file answers "which model CAN serve
//     this request well", this one answers "how do I call this model".
//
//  2. JSON repair (RepairJSON): salvages near-JSON payloads — BOM,
//     surrounding padding, markdown fences, leading/trailing prose around the
//     first complete JSON value, concatenated objects (first-complete or
//     split-and-error mode) and missing closers (bounded depth). Never
//     panics: pathological inputs surface as typed errors.
//
//  3. Reasoning wrapper stripping (StripReasoningBlocks): removes
//     <reasoning>/<thinking>/<think> blocks from the response text and hands
//     them back separately so they can be preserved on
//     CompletionResponse.Reasoning instead of polluting the answer.
//
//  4. Bounded recovery policies (Resilience): exponential backoff with full
//     jitter for transient failures (429/5xx/network, honoring Retry-After)
//     and a single token-limit retry with a reduced prompt
//     (ShrinkRequestForTokenLimit). Both are injectable (clock, jitter
//     source) and conservative by default; every behavior is opt-out-able
//     through ResilienceConfig.
//
// Defaults: repair, reasoning stripping, JSON negotiation and the recovery
// policies are ON when a provider installs DefaultResilience (the
// OpenAIProvider constructor does). Passing a zero ResilienceConfig — or
// SetResilience(nil) — turns the layer off and restores the legacy
// single-attempt wire behavior exactly.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Typed resilience errors. Callers must use errors.Is to classify.
var (
	// ErrUnrepairableJSON indicates a payload that should have been JSON but
	// could not be salvaged (missing-closer balancing failed or was a no-op).
	ErrUnrepairableJSON = errors.New("models: unrepairable JSON response")
	// ErrAmbiguousJSON indicates multiple concatenated JSON values under
	// RepairSplitAndError (the mode refuses to guess which one is meant).
	ErrAmbiguousJSON = errors.New("models: ambiguous JSON response")
)

// Response metadata keys written by the resilience layer (see
// CompletionResponse.Metadata). All keys are advisory observability notes.
const (
	// MetaJSONFallback is set to "prompt_only" when the provider rejected the
	// negotiated response_format=json_object and the request was retried
	// without it.
	MetaJSONFallback = "json_fallback"
	// MetaJSONRepaired lists the repair passes applied to the response text
	// (comma-separated; see repairFlagsString).
	MetaJSONRepaired = "json_repaired"
	// MetaTruncationRetry is set to "true" when a token-limit retry with a
	// reduced prompt served the final response.
	MetaTruncationRetry = "truncation_retry"
	// MetaReasoningStripped is set to "true" when reasoning wrapper blocks
	// were removed from the response text (preserved on Reasoning).
	MetaReasoningStripped = "reasoning_stripped"
)

// ---------------------------------------------------------------------------
// 1. Wire-format capability detection
// ---------------------------------------------------------------------------

// WireCapabilitiesEnvVar names the environment variable holding operator
// overrides for the wire-format capability table: a JSON array of
// WireCapabilities objects. Override entries REPLACE the built-in entry with
// the same (case-insensitive) model id — unlike the routing registry in
// capabilities.go they may NARROW capability, because a gateway in front of a
// model can legitimately reject wire features the raw model supports.
const WireCapabilitiesEnvVar = "AGENTOS_WIRE_MODELS_JSON"

// WireCapabilities describes how the OpenAI-compatible wire protocol must be
// shaped for one model (issue #128). The zero value is the conservative
// unknown-model answer: no negotiated features, legacy max_tokens field,
// unknown context window.
type WireCapabilities struct {
	// Model is the canonical model id the entry resolves (echoed back so
	// callers can log which table row matched).
	Model string `json:"model"`
	// SupportsJSONResponse reports whether the endpoint accepts
	// response_format={"type":"json_object"} for this model.
	SupportsJSONResponse bool `json:"supports_json_response"`
	// UsesMaxCompletionTokens reports whether the model requires the
	// max_completion_tokens parameter instead of the legacy max_tokens
	// (gpt-5+ and the o-series reasoning models).
	UsesMaxCompletionTokens bool `json:"uses_max_completion_tokens"`
	// SupportsReasoning marks o1/o3-style reasoning models (their latency
	// profile differs and they may emit reasoning content).
	SupportsReasoning bool `json:"supports_reasoning"`
	// MaxContextTokens is the model's context window (input+output); 0 means
	// unknown (the token-limit retry then uses a heuristic budget).
	MaxContextTokens int `json:"max_context_tokens"`
}

// builtinWireCapabilities is the name-based wire capability table. Context
// figures mirror the routing registry in capabilities.go (same documented
// data-source rationale: vendor model docs at the time of writing). JSON
// response_format support is marked only where the vendor documents it for
// OpenAI-compatible endpoints; conservative false elsewhere.
var builtinWireCapabilities = []WireCapabilities{
	// OpenAI chat models: json_object documented; legacy max_tokens field.
	{Model: "gpt-4o", SupportsJSONResponse: true, MaxContextTokens: 128000},
	{Model: "gpt-4o-mini", SupportsJSONResponse: true, MaxContextTokens: 128000},
	{Model: "gpt-4.1", SupportsJSONResponse: true, MaxContextTokens: 1000000},
	{Model: "gpt-4.1-mini", SupportsJSONResponse: true, MaxContextTokens: 1000000},
	{Model: "gpt-4.1-nano", SupportsJSONResponse: true, MaxContextTokens: 1000000},
	// gpt-5 family: json_object, max_completion_tokens (no max_tokens).
	{Model: "gpt-5", SupportsJSONResponse: true, UsesMaxCompletionTokens: true, MaxContextTokens: 400000},
	{Model: "gpt-5-mini", SupportsJSONResponse: true, UsesMaxCompletionTokens: true, MaxContextTokens: 400000},
	{Model: "gpt-5-nano", SupportsJSONResponse: true, UsesMaxCompletionTokens: true, MaxContextTokens: 400000},
	// o-series reasoning models: max_completion_tokens; o1-mini does not
	// document json_object (conservative false).
	{Model: "o1", SupportsJSONResponse: true, UsesMaxCompletionTokens: true, SupportsReasoning: true, MaxContextTokens: 200000},
	{Model: "o1-mini", UsesMaxCompletionTokens: true, SupportsReasoning: true, MaxContextTokens: 128000},
	{Model: "o3", SupportsJSONResponse: true, UsesMaxCompletionTokens: true, SupportsReasoning: true, MaxContextTokens: 200000},
	{Model: "o3-mini", SupportsJSONResponse: true, UsesMaxCompletionTokens: true, SupportsReasoning: true, MaxContextTokens: 200000},
	{Model: "o4-mini", SupportsJSONResponse: true, UsesMaxCompletionTokens: true, SupportsReasoning: true, MaxContextTokens: 200000},
	// Anthropic via OpenAI-compatible gateways: response_format support
	// varies by gateway (conservative false); context per vendor docs.
	{Model: "claude-3-5-sonnet-latest", MaxContextTokens: 200000},
	{Model: "claude-sonnet-4", MaxContextTokens: 200000},
	{Model: "claude-3-5-haiku-latest", MaxContextTokens: 200000},
	{Model: "claude-3-opus-latest", MaxContextTokens: 200000},
	{Model: "claude-opus-4", MaxContextTokens: 200000},
	{Model: "claude-3-haiku-20240307", MaxContextTokens: 200000},
	// Google Gemini OpenAI-compat layer documents response_format json_object.
	{Model: "gemini-1.5-pro", SupportsJSONResponse: true, MaxContextTokens: 2000000},
	{Model: "gemini-1.5-flash", SupportsJSONResponse: true, MaxContextTokens: 1000000},
	{Model: "gemini-2.0-flash", SupportsJSONResponse: true, MaxContextTokens: 1000000},
	{Model: "gemini-2.5-pro", SupportsJSONResponse: true, MaxContextTokens: 1000000},
	{Model: "gemini-2.5-flash", SupportsJSONResponse: true, MaxContextTokens: 1000000},
	// DeepSeek: chat documents json_object; reasoner does not.
	{Model: "deepseek-chat", SupportsJSONResponse: true, MaxContextTokens: 64000},
	{Model: "deepseek-reasoner", SupportsReasoning: true, MaxContextTokens: 64000},
	// Self-host/open-weight stacks (llama.cpp, vLLM, Ollama): features vary
	// by server build — conservative defaults, documented context windows.
	{Model: "llama-3.1-8b-instruct", MaxContextTokens: 128000},
	{Model: "llama-3.1-70b-instruct", MaxContextTokens: 128000},
	{Model: "llama-3.1-405b-instruct", MaxContextTokens: 128000},
	{Model: "mistral-small", MaxContextTokens: 128000},
	{Model: "mistral-small-latest", MaxContextTokens: 128000},
}

// builtinWirePrefixRules resolve UNKNOWN model ids by family prefix
// (longest prefix wins), so a vendor variant like "gpt-5-turbo-custom"
// inherits its family's wire behavior without a table entry. Rules apply
// only after the exact and suffix lookups miss.
var builtinWirePrefixRules = []struct {
	prefix string
	caps   WireCapabilities
}{
	{"gpt-5", WireCapabilities{SupportsJSONResponse: true, UsesMaxCompletionTokens: true, MaxContextTokens: 400000}},
	{"gpt-4", WireCapabilities{SupportsJSONResponse: true, MaxContextTokens: 128000}},
	{"o1", WireCapabilities{UsesMaxCompletionTokens: true, SupportsReasoning: true, MaxContextTokens: 200000}},
	{"o3", WireCapabilities{SupportsJSONResponse: true, UsesMaxCompletionTokens: true, SupportsReasoning: true, MaxContextTokens: 200000}},
	{"o4", WireCapabilities{SupportsJSONResponse: true, UsesMaxCompletionTokens: true, SupportsReasoning: true, MaxContextTokens: 200000}},
}

// WireCapabilityTable maps model ids to WireCapabilities: built-ins, optional
// operator overrides (exact-id REPLACE semantics) and deterministic prefix
// rules for unknown variants. It is safe for concurrent use. The zero value
// resolves built-ins only; construct via NewWireCapabilityTable /
// WireCapabilityTableFromEnv.
type WireCapabilityTable struct {
	mu        sync.RWMutex
	overrides map[string]WireCapabilities
}

// NewWireCapabilityTable builds a table from operator overrides (nil/empty
// entries are skipped; entries without a model id are ignored).
func NewWireCapabilityTable(overrides []WireCapabilities) *WireCapabilityTable {
	t := &WireCapabilityTable{overrides: make(map[string]WireCapabilities, len(overrides))}
	for _, o := range overrides {
		t.Register(o)
	}
	return t
}

// WireCapabilityTableFromEnv builds a table with overrides parsed from
// AGENTOS_WIRE_MODELS_JSON. Invalid override JSON is ignored (built-ins stay
// effective — capability data must never break a run; validate at startup
// with WireCapabilitiesFromJSON when strictness is wanted).
func WireCapabilityTableFromEnv() *WireCapabilityTable {
	t := NewWireCapabilityTable(nil)
	if raw := strings.TrimSpace(envOr(WireCapabilitiesEnvVar, "")); raw != "" {
		if entries, err := WireCapabilitiesFromJSON(raw); err == nil {
			for _, e := range entries {
				t.Register(e)
			}
		}
	}
	return t
}

// Register adds or replaces one override entry (exact, case-insensitive id).
// Entries with a blank model id are ignored. Overrides REPLACE the resolved
// built-in (they may narrow capability — see WireCapabilitiesEnvVar).
func (t *WireCapabilityTable) Register(caps WireCapabilities) {
	id := strings.ToLower(strings.TrimSpace(caps.Model))
	if id == "" {
		return
	}
	caps.Model = strings.TrimSpace(caps.Model)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.overrides == nil {
		t.overrides = make(map[string]WireCapabilities, 4)
	}
	t.overrides[id] = caps
}

// Detect resolves the wire capabilities for one model id. Resolution order
// (all deterministic):
//
//  1. operator override — exact id, then the bare suffix of a "vendor/model"
//     id (OpenRouter-style ids resolve against overrides first);
//  2. built-in table — exact id, then the bare suffix;
//  3. family prefix rules (longest prefix wins);
//  4. conservative unknown default (no negotiated features, legacy
//     max_tokens, unknown context).
func (t *WireCapabilityTable) Detect(model string) WireCapabilities {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return WireCapabilities{}
	}
	t.mu.RLock()
	override, hasOverride := lookupWireCaps(t.overrides, id)
	t.mu.RUnlock()
	if hasOverride {
		return override
	}
	for _, c := range builtinWireCapabilities {
		if strings.ToLower(c.Model) == id {
			return c
		}
	}
	if idx := strings.LastIndex(id, "/"); idx >= 0 && idx+1 < len(id) {
		for _, c := range builtinWireCapabilities {
			if strings.ToLower(c.Model) == id[idx+1:] {
				return c
			}
		}
	}
	if best := longestWirePrefixRule(id); best >= 0 {
		caps := builtinWirePrefixRules[best].caps
		caps.Model = strings.TrimSpace(model)
		return caps
	}
	return WireCapabilities{Model: strings.TrimSpace(model)}
}

// longestWirePrefixRule returns the index of the longest prefix rule matching
// id (-1 when none does). Rules are few, so the linear scan is cheap.
func longestWirePrefixRule(id string) int {
	best := -1
	for i, rule := range builtinWirePrefixRules {
		if strings.HasPrefix(id, rule.prefix) &&
			(best < 0 || len(rule.prefix) > len(builtinWirePrefixRules[best].prefix)) {
			best = i
		}
	}
	return best
}

// lookupWireCaps resolves id against a map of overrides: exact match, then
// the bare suffix after the last "/" (vendor/model ids).
func lookupWireCaps(m map[string]WireCapabilities, id string) (WireCapabilities, bool) {
	if m == nil {
		return WireCapabilities{}, false
	}
	if c, ok := m[id]; ok {
		return c, true
	}
	if idx := strings.LastIndex(id, "/"); idx >= 0 && idx+1 < len(id) {
		if c, ok := m[id[idx+1:]]; ok {
			return c, true
		}
	}
	return WireCapabilities{}, false
}

// WireCapabilitiesFromJSON parses an AGENTOS_WIRE_MODELS_JSON payload: a JSON
// array of WireCapabilities objects. Entries without a model id are rejected;
// duplicate ids keep the last occurrence. Useful for startup validation.
func WireCapabilitiesFromJSON(raw string) ([]WireCapabilities, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("models: empty %s payload", WireCapabilitiesEnvVar)
	}
	var entries []WireCapabilities
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("models: invalid %s payload: %v", WireCapabilitiesEnvVar, err)
	}
	for i, e := range entries {
		if strings.TrimSpace(e.Model) == "" {
			return nil, fmt.Errorf("models: %s entry %d is missing \"model\"", WireCapabilitiesEnvVar, i)
		}
	}
	return entries, nil
}

// ---------------------------------------------------------------------------
// 2. JSON repair
// ---------------------------------------------------------------------------

// maxRepairJSONDepth bounds the balancing scanner: nesting deeper than this
// is treated as unrepairable (pathological input must produce an error, not
// unbounded work).
const maxRepairJSONDepth = 128

// RepairMode selects how RepairJSON handles multiple concatenated JSON
// values.
type RepairMode string

const (
	// RepairFirstComplete (default) returns the FIRST complete JSON value
	// and strips whatever follows it.
	RepairFirstComplete RepairMode = "first_complete"
	// RepairSplitAndError refuses to guess: multiple concatenated values are
	// reported as ErrAmbiguousJSON so the caller can split them itself.
	RepairSplitAndError RepairMode = "split_and_error"
)

// Repairs records which repair passes modified a payload (observability for
// the MetaJSONRepaired metadata key).
type Repairs struct {
	// StrippedBOM: a leading UTF-8 byte-order mark was removed.
	StrippedBOM bool
	// StrippedPadding: surrounding whitespace was trimmed.
	StrippedPadding bool
	// StrippedFences: a markdown code fence around the payload was removed.
	StrippedFences bool
	// StrippedLeadingText: prose before the first JSON value was removed.
	StrippedLeadingText bool
	// StrippedTrailingText: text after the first complete JSON value was
	// removed.
	StrippedTrailingText bool
	// MultipleObjects: more than one concatenated top-level JSON value was
	// detected (does not itself modify the payload in first-complete mode).
	MultipleObjects bool
	// BalancedClosers: missing closing brackets/braces were appended.
	BalancedClosers bool
	// ClosedOpenString: a terminating quote was appended for a string left
	// open by truncation.
	ClosedOpenString bool
}

// Applied reports whether any pass actually modified the payload.
func (r Repairs) Applied() bool {
	return r.StrippedBOM || r.StrippedPadding || r.StrippedFences ||
		r.StrippedLeadingText || r.StrippedTrailingText ||
		r.BalancedClosers || r.ClosedOpenString
}

// repairFlagsString renders the applied passes as a stable comma-joined list
// (the MetaJSONRepaired metadata value).
func repairFlagsString(r Repairs) string {
	flags := make([]string, 0, 8)
	add := func(b bool, name string) {
		if b {
			flags = append(flags, name)
		}
	}
	add(r.StrippedBOM, "bom")
	add(r.StrippedPadding, "padding")
	add(r.StrippedFences, "fences")
	add(r.StrippedLeadingText, "leading_text")
	add(r.StrippedTrailingText, "trailing_text")
	add(r.MultipleObjects, "multiple_objects")
	add(r.BalancedClosers, "balanced_closers")
	add(r.ClosedOpenString, "closed_string")
	return strings.Join(flags, ",")
}

// RepairJSON salvages a near-JSON payload produced by an LLM and returns the
// repaired string, a record of the passes applied, or a typed error. It never
// panics: pathological inputs (random bytes, runaway nesting, unterminated
// strings) surface as ErrUnrepairableJSON / ErrAmbiguousJSON.
//
// Passes, in order:
//
//  1. strip a leading UTF-8 BOM and surrounding whitespace;
//  2. strip a markdown code fence wrapping the payload (```json ... ``` —
//     also without a closing fence, so truncated fenced payloads can still
//     be balanced);
//  3. if the payload is already valid JSON, return it unchanged;
//  4. otherwise locate the first JSON object/array (leading prose is
//     skipped; when the payload does not start with '{' a '{' is preferred
//     over '[' so prose containing bracketed citations does not win);
//  5. decode the first value: on success, anything after it is stripped
//     (trailing text; a second decodable value means concatenated objects —
//     first-complete mode keeps the first, split-and-error mode errors);
//  6. on decode failure, balance missing closers with a string-aware scanner
//     (bounded depth): unterminated strings get a closing quote and open
//     brackets/braces get their closers in reverse order; the result must
//     then validate, otherwise the payload is unrepairable.
//
// Known (documented) limitations: bare scalars with trailing junk are not
// repaired, and fence/prose scanning is a best-effort heuristic — the repair
// output is ALWAYS revalidated, so a wrong guess yields an error, never
// invalid JSON.
func RepairJSON(input string, mode RepairMode) (string, Repairs, error) {
	var rp Repairs
	s := input
	if strings.HasPrefix(s, "\ufeff") {
		s = strings.TrimPrefix(s, "\ufeff")
		rp.StrippedBOM = true
	}
	if trimmed := strings.TrimSpace(s); trimmed != s {
		rp.StrippedPadding = true
		s = trimmed
	}
	if s == "" {
		return "", rp, fmt.Errorf("%w: empty payload", ErrUnrepairableJSON)
	}
	if strings.HasPrefix(s, "```") {
		rest := s[3:]
		if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
			rest = rest[nl+1:] // drop the language-tag line
		} else {
			rest = ""
		}
		if idx := strings.LastIndex(rest, "```"); idx >= 0 {
			rest = rest[:idx] // drop the closing fence when present
		}
		rp.StrippedFences = true
		if trimmed := strings.TrimSpace(rest); trimmed != rest {
			rp.StrippedPadding = true
			rest = trimmed
		}
		s = rest
		if s == "" {
			return "", rp, fmt.Errorf("%w: empty payload inside markdown fence", ErrUnrepairableJSON)
		}
	}
	if json.Valid([]byte(s)) {
		return s, rp, nil
	}

	// Locate the first JSON value: prefer an object start; fall back to an
	// array start (see the doc comment for why '{' wins over '[').
	start := -1
	if s[0] == '{' || s[0] == '[' {
		start = 0
	} else if idx := strings.IndexByte(s, '{'); idx >= 0 {
		start, rp.StrippedLeadingText = idx, true
	} else if idx := strings.IndexByte(s, '['); idx >= 0 {
		start, rp.StrippedLeadingText = idx, true
	}
	if start < 0 {
		return "", rp, fmt.Errorf("%w: no JSON object or array found", ErrUnrepairableJSON)
	}
	cand := s[start:]

	dec := json.NewDecoder(strings.NewReader(cand))
	var first json.RawMessage
	if err := dec.Decode(&first); err == nil {
		offset := dec.InputOffset()
		if offset < int64(len(cand)) {
			rp.StrippedTrailingText = true
		}
		// Detect a second concatenated value (Decode skips whitespace).
		var second json.RawMessage
		if err2 := dec.Decode(&second); err2 == nil {
			rp.MultipleObjects = true
			if mode == RepairSplitAndError {
				return "", rp, fmt.Errorf("%w: concatenated JSON values under %q mode", ErrAmbiguousJSON, RepairSplitAndError)
			}
		}
		return string(first), rp, nil
	}

	// The first value is incomplete (truncation) or malformed: balance
	// missing closers and revalidate.
	fixed, closedStr, err := balanceJSON(cand)
	if err != nil {
		return "", rp, err
	}
	if fixed == cand && !closedStr {
		// Nothing to add: malformed beyond missing closers.
		return "", rp, fmt.Errorf("%w: malformed JSON (no missing closers detected)", ErrUnrepairableJSON)
	}
	rp.BalancedClosers = fixed != cand
	rp.ClosedOpenString = closedStr
	if !json.Valid([]byte(fixed)) {
		return "", rp, fmt.Errorf("%w: closer balancing did not yield valid JSON", ErrUnrepairableJSON)
	}
	return fixed, rp, nil
}

// balanceJSON appends the closers missing from a truncated JSON payload:
// unterminated strings get a closing quote and open brackets/braces get
// their closers in reverse order. The scanner is string- and escape-aware
// and bounded by maxRepairJSONDepth. Stray closers without a matching opener
// are left in place (the caller's revalidation decides the outcome). The
// second return reports whether a string had to be closed.
func balanceJSON(s string) (string, bool, error) {
	var stack []byte
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		b := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case b == '\\':
				esc = true
			case b == '"':
				inStr = false
			}
			continue
		}
		switch b {
		case '"':
			inStr = true
		case '{', '[':
			stack = append(stack, b)
			if len(stack) > maxRepairJSONDepth {
				return "", false, fmt.Errorf("%w: nesting exceeds %d levels", ErrUnrepairableJSON, maxRepairJSONDepth)
			}
		case '}', ']':
			popJSONCloser(&stack, b)
		}
	}
	if len(stack) == 0 && !inStr {
		return s, false, nil
	}
	var out strings.Builder
	out.Grow(len(s) + len(stack) + 1)
	out.WriteString(s)
	if inStr {
		out.WriteByte('"')
	}
	// Append the closers for the remaining openers in reverse order.
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			out.WriteByte('}')
		} else {
			out.WriteByte(']')
		}
	}
	return out.String(), inStr, nil
}

// popJSONCloser pops the stack up to and including the opener matching the
// closer just consumed (a mismatched interleaved closer implicitly closes
// the openers above its match); closers without any matching opener are
// ignored.
func popJSONCloser(stack *[]byte, closer byte) {
	want := byte('[')
	if closer == '}' {
		want = '{'
	}
	for i := len(*stack) - 1; i >= 0; i-- {
		if (*stack)[i] == want {
			*stack = (*stack)[:i]
			return
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Reasoning wrapper stripping
// ---------------------------------------------------------------------------

// reasoningTagNames are the reasoning wrapper tags stripped from response
// content (matched case-insensitively as exact "<tag>"/"</tag>" markers).
var reasoningTagNames = []string{"reasoning", "thinking", "think"}

// StripReasoningBlocks removes reasoning wrapper blocks
// (<reasoning>...</reasoning>, <thinking>...</thinking>, <think>...</think>
// — case-insensitive) from model output. It returns the cleaned content, the
// extracted reasoning text (blocks joined with newlines, order preserved)
// and whether any block was found. An opener without a closer moves the rest
// of the content into the reasoning text (the common "generation stopped
// mid-thought" shape). Nested or interleaved wrappers are handled
// best-effort: the first opener consumes text up to the first matching
// closer.
func StripReasoningBlocks(content string) (string, string, bool) {
	lower := strings.ToLower(content)
	var parts []string
	var clean strings.Builder
	clean.Grow(len(content))
	pos := 0
	for {
		open, tagLen := -1, 0
		for _, name := range reasoningTagNames {
			if idx := strings.Index(lower[pos:], "<"+name+">"); idx >= 0 {
				abs := pos + idx
				if open < 0 || abs < open {
					open, tagLen = abs, len(name)+2
				}
			}
		}
		if open < 0 {
			break
		}
		openEnd := open + tagLen
		closeMarker := "</" + lower[open+1:open+tagLen-1] + ">"
		closeIdx := strings.Index(lower[openEnd:], closeMarker)
		if closeIdx < 0 {
			// Unclosed wrapper: the tail is reasoning, the head is content.
			parts = append(parts, content[openEnd:])
			clean.WriteString(content[pos:open])
			pos = len(content)
			break
		}
		closeStart := openEnd + closeIdx
		parts = append(parts, content[openEnd:closeStart])
		clean.WriteString(content[pos:open])
		pos = closeStart + len(closeMarker)
	}
	if len(parts) == 0 {
		return content, "", false
	}
	clean.WriteString(content[pos:])
	return strings.TrimSpace(clean.String()),
		strings.TrimSpace(strings.Join(parts, "\n")), true
}

// ---------------------------------------------------------------------------
// 4. Recovery policies: config, backoff, shrink
// ---------------------------------------------------------------------------

// Retry defaults (issue #128): up to 3 retries per request for transient
// failures, exponential backoff with full jitter between 200ms and 8s,
// honoring the provider's Retry-After header when present.
const (
	DefaultMaxRetries     = 3
	DefaultRetryBaseDelay = 200 * time.Millisecond
	DefaultRetryMaxDelay  = 8 * time.Second
)

// defaultTruncationRetryMargin is the output-token headroom subtracted from
// a known context window when computing the token-limit retry budget.
const defaultTruncationRetryMargin = 1024

// ResilienceConfig configures the resilience layer. The zero value disables
// every concern (explicit opt-out); DefaultResilienceConfig returns the
// documented defaults-ON configuration.
type ResilienceConfig struct {
	// Enabled is the master switch: false makes the provider behave exactly
	// as before the resilience layer existed (single attempt, legacy wire
	// shape, no post-processing).
	Enabled bool
	// NegotiateJSON sends response_format=json_object when the request asks
	// for JSON (CompletionRequest.WantJSON) and the model supports it, and
	// falls back to a prompt-only retry when the provider rejects it.
	NegotiateJSON bool
	// RepairJSON runs RepairJSON over responses that asked for JSON (or that
	// arrive markdown-fenced). Best-effort for unprompted payloads.
	RepairJSON bool
	// StripReasoning removes reasoning wrapper blocks into
	// CompletionResponse.Reasoning.
	StripReasoning bool
	// Retry bounds the transient-failure backoff loop (429/5xx/network).
	Retry RetryConfig
	// TruncationRetry bounds the token-limit retry (finish_reason=length or
	// unrepairable JSON): ONE retry with a reduced prompt.
	TruncationRetry TruncationRetryConfig
}

// RetryConfig configures the transient-failure retry loop.
type RetryConfig struct {
	// Enabled gates the loop entirely.
	Enabled bool
	// MaxRetries is the number of RETRIES after the initial attempt
	// (default 3 → at most 4 attempts). Non-positive disables retries.
	MaxRetries int
	// BaseDelay is the backoff base (exponential: base*2^attempt, capped).
	BaseDelay time.Duration
	// MaxDelay caps every individual wait (including Retry-After).
	MaxDelay time.Duration
	// HonorRetryAfter prefers the provider's Retry-After header over the
	// computed backoff (capped at MaxDelay).
	HonorRetryAfter bool
}

// TruncationRetryConfig configures the single token-limit retry.
type TruncationRetryConfig struct {
	// Enabled gates the retry entirely.
	Enabled bool
	// TokenMargin is the output-token headroom subtracted from a known
	// context window when computing the reduced-prompt budget (default
	// defaultTruncationRetryMargin when zero).
	TokenMargin int
}

// DefaultResilienceConfig returns the issue #128 defaults: everything ON,
// conservative retry bounds, Retry-After honored.
func DefaultResilienceConfig() ResilienceConfig {
	return ResilienceConfig{
		Enabled:        true,
		NegotiateJSON:  true,
		RepairJSON:     true,
		StripReasoning: true,
		Retry: RetryConfig{
			Enabled:         true,
			MaxRetries:      DefaultMaxRetries,
			BaseDelay:       DefaultRetryBaseDelay,
			MaxDelay:        DefaultRetryMaxDelay,
			HonorRetryAfter: true,
		},
		TruncationRetry: TruncationRetryConfig{Enabled: true, TokenMargin: defaultTruncationRetryMargin},
	}
}

// ResilienceOption configures a Resilience at construction.
type ResilienceOption func(*Resilience)

// WithResilienceClock injects the clock used for backoff waits. A clock that
// also implements the sleeper interface (Sleep advancing virtual time) is
// fast-forwarded instead of blocking, which keeps backoff timing tests
// deterministic and instant. Nil keeps the wall clock.
func WithResilienceClock(c Clock) ResilienceOption {
	return func(r *Resilience) {
		if c != nil {
			r.clock = c
		}
	}
}

// WithResilienceJitter injects the full-jitter source: a function returning
// multipliers in [0,1). Nil keeps the default (math/rand/v2 Float64).
func WithResilienceJitter(f func() float64) ResilienceOption {
	return func(r *Resilience) {
		if f != nil {
			r.jitter = f
		}
	}
}

// WithResilienceWireTable injects the wire capability table (nil keeps the
// env-initialized default).
func WithResilienceWireTable(t *WireCapabilityTable) ResilienceOption {
	return func(r *Resilience) {
		if t != nil {
			r.table = t
		}
	}
}

// Resilience bundles the issue #128 policies used by the OpenAI provider's
// request/response path. It is immutable after construction; the provider
// reads it without locking.
type Resilience struct {
	cfg    ResilienceConfig
	table  *WireCapabilityTable
	clock  Clock
	jitter func() float64
}

// NewResilience builds the layer from cfg (zero fields mean the concern is
// disabled) plus options. The wire capability table defaults to
// WireCapabilityTableFromEnv (built-ins + AGENTOS_WIRE_MODELS_JSON).
func NewResilience(cfg ResilienceConfig, opts ...ResilienceOption) *Resilience {
	r := &Resilience{
		cfg:    cfg,
		table:  WireCapabilityTableFromEnv(),
		clock:  realClock{},
		jitter: rand.Float64,
	}
	for _, o := range opts {
		if o != nil {
			o(r)
		}
	}
	return r
}

// DefaultResilience returns the defaults-ON layer with the wall clock.
func DefaultResilience() *Resilience {
	return NewResilience(DefaultResilienceConfig())
}

// Config returns the active configuration (diagnostics/tests).
func (r *Resilience) Config() ResilienceConfig {
	if r == nil {
		return ResilienceConfig{}
	}
	return r.cfg
}

// Detect resolves wire capabilities for a model through the layer's table.
func (r *Resilience) Detect(model string) WireCapabilities {
	if r == nil {
		return WireCapabilities{}
	}
	if r.table != nil {
		return r.table.Detect(model)
	}
	return WireCapabilityTableFromEnv().Detect(model)
}

// BackoffDelay computes the wait before retry `attempt` (0-based): the
// provider's Retry-After when honored (capped at MaxDelay), otherwise full
// jitter over min(BaseDelay*2^attempt, MaxDelay) — uniform in [0, cap), the
// classic AWS formulation. Deterministic under injected clock/jitter.
func (r *Resilience) BackoffDelay(attempt int, retryAfter string) time.Duration {
	base, maxDelay := r.cfg.Retry.BaseDelay, r.cfg.Retry.MaxDelay
	if base <= 0 {
		base = DefaultRetryBaseDelay
	}
	if maxDelay <= 0 {
		maxDelay = DefaultRetryMaxDelay
	}
	if base > maxDelay {
		base = maxDelay
	}
	if r.cfg.Retry.HonorRetryAfter {
		if d, ok := ParseRetryAfter(retryAfter); ok {
			if d > maxDelay {
				d = maxDelay
			}
			return d
		}
	}
	shift := attempt
	if shift > 16 {
		shift = 16 // cap the exponent; beyond that MaxDelay governs anyway
	}
	exp := base * time.Duration(1<<uint(shift))
	if exp > maxDelay || exp <= 0 {
		exp = maxDelay // overflow guard
	}
	f := r.jitter()
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return time.Duration(float64(exp) * f)
}

// ParseRetryAfter parses a Retry-After header value: delay seconds or an
// HTTP-date. It returns (duration, true) for valid non-negative values and
// (0, false) otherwise; the caller applies any policy cap.
func ParseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// resilienceSleep waits for d, honoring cancellation. Under a sleeper clock
// (tests) the wait is fast-forwarded instead of blocking.
func resilienceSleep(ctx context.Context, c Clock, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	if s, ok := c.(interface{ Sleep(time.Duration) }); ok {
		s.Sleep(d)
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

// completionRequestTokens estimates the token footprint of a request
// (prompt/system/messages plus the requested output budget) — the same ~4
// chars/token heuristic the capability filter uses.
func completionRequestTokens(req CompletionRequest) int {
	total := estimateTokens(req.Prompt) + estimateTokens(req.System) + req.MaxTokens
	for _, m := range req.Messages {
		total += estimateTokens(m.Content)
	}
	return total
}

// ShrinkRequestForTokenLimit reduces a request's prompt so a token-limit
// retry can fit: it drops the OLDEST non-system messages until the estimated
// footprint fits the budget. The budget is (known context − requested output
// − margin) when the model's window is known, otherwise half the original
// estimate. Requests without Messages (the Prompt/System form) have nothing
// to drop and are returned unchanged (changed=false); a request consisting
// only of system messages is also returned unchanged. Best effort: when
// messages run out the shrunk request is returned even if still over budget.
func ShrinkRequestForTokenLimit(req CompletionRequest, maxContextTokens int) (CompletionRequest, bool) {
	if len(req.Messages) == 0 {
		return req, false
	}
	budget := 0
	if maxContextTokens > 0 {
		budget = maxContextTokens - req.MaxTokens - defaultTruncationRetryMargin
		if budget < 1 {
			budget = 1
		}
	} else {
		budget = completionRequestTokens(req) / 2
	}
	shrunk := req
	shrunk.Messages = append([]Message(nil), req.Messages...)
	changed := false
	for completionRequestTokens(shrunk) > budget {
		drop := -1
		for i := range shrunk.Messages {
			if !strings.EqualFold(strings.TrimSpace(shrunk.Messages[i].Role), "system") {
				drop = i
				break
			}
		}
		if drop < 0 {
			break // only system messages remain — they are never dropped
		}
		shrunk.Messages = append(shrunk.Messages[:drop], shrunk.Messages[drop+1:]...)
		changed = true
	}
	return shrunk, changed
}
