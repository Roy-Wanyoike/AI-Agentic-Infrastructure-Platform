package models

// resilience_test.go — issue #128 tests: JSON repair tables (including
// pathological no-panic inputs), reasoning wrapper stripping edges, wire
// capability detection tables, JSON negotiation + prompt-only fallback,
// truncation retry with reduced prompt, and backoff timing under a fake
// clock — all against httptest fake providers where the wire matters.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// steppingClock is a Clock that fast-forwards instead of blocking: it
// implements the resilience sleeper interface so backoff waits are recorded
// (slept) and instantly elapsed (deterministic, no real sleeps).
type steppingClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newSteppingClock() *steppingClock {
	return &steppingClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep implements the sleeper interface resilienceSleep fast-forwards on.
func (c *steppingClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
}

// capturedChatRequest mirrors the wire payload the stub server receives.
type capturedChatRequest struct {
	Model               string                `json:"model"`
	MaxTokens           int                   `json:"max_tokens"`
	MaxCompletionTokens int                   `json:"max_completion_tokens"`
	Temperature         float64               `json:"temperature"`
	ResponseFormat      *openAIResponseFormat `json:"response_format"`
	Messages            []openAIChatMessage   `json:"messages"`
}

// scriptedChatResponse is one scripted stub answer (the last one repeats
// when the stub runs dry, so tests fail loudly via request-count asserts).
type scriptedChatResponse struct {
	status     int
	body       string
	retryAfter string
}

// newScriptedChatServer spins an OpenAI-compatible stub serving the scripted
// responses in order and capturing every request payload into *captured.
func newScriptedChatServer(t *testing.T, captured *[]capturedChatRequest, responses ...scriptedChatResponse) *httptest.Server {
	t.Helper()
	idx := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var capReq capturedChatRequest
		_ = json.Unmarshal(raw, &capReq) // best-effort capture
		if captured != nil {
			*captured = append(*captured, capReq)
		}
		resp := responses[len(responses)-1]
		if idx < len(responses) {
			resp = responses[idx]
		}
		idx++
		if resp.retryAfter != "" {
			w.Header().Set("Retry-After", resp.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// chatOK builds a 200 scripted response with the given content and finish
// reason.
func chatOK(content, finishReason string) scriptedChatResponse {
	body := fmt.Sprintf(`{"model":"stub","choices":[{"message":{"role":"assistant","content":%s},"finish_reason":%s}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		mustJSONString(content), mustJSONString(finishReason))
	return scriptedChatResponse{status: http.StatusOK, body: body}
}

// mustJSONString encodes s as a JSON string literal (test helper).
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// newResilientProvider builds an OpenAIProvider against srv with an injected
// clock/jitter and the given config (zero config → disabled layer).
func newResilientProvider(t *testing.T, srv *httptest.Server, cfg ResilienceConfig, clk *steppingClock, model string) *OpenAIProvider {
	t.Helper()
	p := NewOpenAIProvider("stub", "k", srv.URL, model, srv.Client())
	p.SetResilience(NewResilience(cfg, WithResilienceClock(clk), WithResilienceJitter(func() float64 { return 0.5 })))
	return p
}

// ---------------------------------------------------------------------------
// 1. RepairJSON
// ---------------------------------------------------------------------------

func TestRepairJSONTable(t *testing.T) {
	hundred := strings.Repeat("[", 100)
	cases := []struct {
		name      string
		input     string
		mode      RepairMode // zero value = RepairFirstComplete
		want      string
		flags     string // expected MetaJSONRepaired value ("" = nothing applied)
		multi     bool
		errTarget error // nil, ErrUnrepairableJSON or ErrAmbiguousJSON
	}{
		{
			name:  "clean object untouched",
			input: `{"a":1}`,
			want:  `{"a":1}`,
		},
		{
			name:  "bare scalar is valid JSON",
			input: `42`,
			want:  `42`,
		},
		{
			name:  "array with surrounding padding",
			input: "  [1,2,3]\n",
			want:  `[1,2,3]`,
			flags: "padding",
		},
		{
			name:  "leading BOM",
			input: "\ufeff{\"a\":1}",
			want:  `{"a":1}`,
			flags: "bom",
		},
		{
			name:  "BOM and padding",
			input: "\ufeff  {\"a\":1} \n",
			want:  `{"a":1}`,
			flags: "bom,padding",
		},
		{
			name:  "markdown fence with language tag",
			input: "```json\n{\"a\":1}\n```",
			want:  `{"a":1}`,
			flags: "padding,fences",
		},
		{
			name:  "markdown fence without language tag",
			input: "```\n{\"a\":1}\n```",
			want:  `{"a":1}`,
			flags: "padding,fences",
		},
		{
			name:  "unclosed fence around truncated JSON",
			input: "```json\n{\"a\": [1, 2",
			want:  `{"a": [1, 2]}`,
			flags: "fences,balanced_closers",
		},
		{
			name:  "trailing prose after object",
			input: `{"a":1} Hope that helps!`,
			want:  `{"a":1}`,
			flags: "trailing_text",
		},
		{
			name:  "leading prose before object",
			input: `Sure, here is the JSON: {"a":1}`,
			want:  `{"a":1}`,
			flags: "leading_text",
		},
		{
			name:  "leading and trailing prose",
			input: `Answer: {"a":1} — hope!`,
			want:  `{"a":1}`,
			flags: "leading_text,trailing_text",
		},
		{
			name:  "prose with brackets prefers the object",
			input: `See [1] for refs: {"a":1}`,
			want:  `{"a":1}`,
			flags: "leading_text",
		},
		{
			name:  "concatenated objects first-complete",
			input: `{"a":1}{"b":2}`,
			want:  `{"a":1}`,
			flags: "trailing_text,multiple_objects",
			multi: true,
		},
		{
			name:      "concatenated objects split-and-error",
			input:     `{"a":1}{"b":2}`,
			mode:      RepairSplitAndError,
			errTarget: ErrAmbiguousJSON,
		},
		{
			name:  "prose between objects is not multiple objects",
			input: `{"a":1} and {"b":2}`,
			want:  `{"a":1}`,
			flags: "trailing_text",
		},
		{
			name:  "missing nested closers balanced",
			input: `{"a": {"b": [1, 2`,
			want:  `{"a": {"b": [1, 2]}}`,
			flags: "balanced_closers",
		},
		{
			name:  "truncated string value closed",
			input: `{"summary": "cut off mid sent`,
			want:  `{"summary": "cut off mid sent"}`,
			flags: "balanced_closers,closed_string",
		},
		{
			name:      "stray closers only",
			input:     `}}}`,
			errTarget: ErrUnrepairableJSON,
		},
		{
			name:      "malformed object with no missing closers",
			input:     `{nan}`,
			errTarget: ErrUnrepairableJSON,
		},
		{
			name:      "empty payload",
			input:     ``,
			errTarget: ErrUnrepairableJSON,
		},
		{
			name:      "whitespace only",
			input:     "   \n\t",
			errTarget: ErrUnrepairableJSON,
		},
		{
			name:      "BOM only",
			input:     "\ufeff",
			errTarget: ErrUnrepairableJSON,
		},
		{
			name:      "empty fence",
			input:     "```json\n```",
			errTarget: ErrUnrepairableJSON,
		},
		{
			name:      "scalar with trailing junk",
			input:     `42 and more`,
			errTarget: ErrUnrepairableJSON,
		},
		{
			name:  "hundred nested arrays balanced",
			input: hundred,
			want:  hundred + strings.Repeat("]", 100),
			flags: "balanced_closers",
		},
		{
			name:      "runaway nesting errors without panic",
			input:     strings.Repeat("[", 4096),
			errTarget: ErrUnrepairableJSON,
		},
		{
			name:      "brace inside leading quote confuses the scan (documented limitation)",
			input:     `He said "{" then: {"a":1}`,
			errTarget: ErrUnrepairableJSON,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode := tc.mode
			if mode == "" {
				mode = RepairFirstComplete
			}
			got, repairs, err := RepairJSON(tc.input, mode)
			if tc.errTarget != nil {
				if err == nil {
					t.Fatalf("RepairJSON(%q) = %q, want error %v", tc.input, got, tc.errTarget)
				}
				if !errors.Is(err, tc.errTarget) {
					t.Fatalf("RepairJSON(%q) error = %v, want %v", tc.input, err, tc.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("RepairJSON(%q) returned error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("RepairJSON(%q) = %q, want %q", tc.input, got, tc.want)
			}
			if flags := repairFlagsString(repairs); flags != tc.flags {
				t.Fatalf("repair flags = %q, want %q (repairs %+v)", flags, tc.flags, repairs)
			}
			if repairs.MultipleObjects != tc.multi {
				t.Fatalf("MultipleObjects = %v, want %v", repairs.MultipleObjects, tc.multi)
			}
			if applied := repairs.Applied(); applied != (tc.flags != "") {
				t.Fatalf("Applied() = %v, inconsistent with flags %q", applied, tc.flags)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("repaired output is not valid JSON: %q", got)
			}
		})
	}
}

// TestRepairJSONNoPanicOnPathologicalInput feeds deterministic pseudo-random
// garbage and structured truncations through RepairJSON: it must never panic,
// must only ever return valid JSON, and errors must be the typed sentinels.
func TestRepairJSONNoPanicOnPathologicalInput(t *testing.T) {
	// Deterministic LCG so failures reproduce.
	state := uint64(0x9E3779B97F4A7C15)
	next := func(n int) int {
		state = state*6364136223846793005 + 1442695040888963407
		return int((state >> 33) % uint64(n))
	}
	alphabet := []byte("{}[]\"\\,:` \n\tabc123\ufeff-")
	base := `{"k": [1, 2, {"deep": "v"}], "tail": null}`
	inputs := make([]string, 0, 600)
	for i := 0; i < 500; i++ {
		n := next(64)
		b := make([]byte, n)
		for j := range b {
			b[j] = alphabet[next(len(alphabet))]
		}
		inputs = append(inputs, string(b))
	}
	// Structured truncations at every offset, with and without a fence.
	for cut := 0; cut <= len(base); cut++ {
		inputs = append(inputs, base[:cut], "```json\n"+base[:cut])
	}

	for i, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("input %d (%q): RepairJSON panicked: %v", i, in, r)
				}
			}()
			out, _, err := RepairJSON(in, RepairFirstComplete)
			if err != nil {
				if !errors.Is(err, ErrUnrepairableJSON) && !errors.Is(err, ErrAmbiguousJSON) {
					t.Fatalf("input %d (%q): unexpected error type: %v", i, in, err)
				}
				return
			}
			if !json.Valid([]byte(out)) {
				t.Fatalf("input %d (%q): success returned invalid JSON %q", i, in, out)
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// 2. Reasoning wrapper stripping
// ---------------------------------------------------------------------------

func TestStripReasoningBlocksTable(t *testing.T) {
	cases := []struct {
		name          string
		input         string
		wantClean     string
		wantReasoning string
		wantFound     bool
	}{
		{
			name:      "no tags untouched",
			input:     "just an answer",
			wantClean: "just an answer",
		},
		{
			name:          "think block",
			input:         "<think>pondering</think>answer",
			wantClean:     "answer",
			wantReasoning: "pondering",
			wantFound:     true,
		},
		{
			name:          "thinking block with surrounding text",
			input:         "pre <thinking>internal</thinking> post",
			wantClean:     "pre  post", // inner whitespace preserved, edges trimmed
			wantReasoning: "internal",
			wantFound:     true,
		},
		{
			name:          "reasoning block",
			input:         "<reasoning>why</reasoning>result",
			wantClean:     "result",
			wantReasoning: "why",
			wantFound:     true,
		},
		{
			name:          "case-insensitive tags",
			input:         "<THINK>up</THINK>down",
			wantClean:     "down",
			wantReasoning: "up",
			wantFound:     true,
		},
		{
			name:          "multiple blocks joined in order",
			input:         "<think>one</think>a<thinking>two</thinking>b",
			wantClean:     "ab",
			wantReasoning: "one\ntwo",
			wantFound:     true,
		},
		{
			name:          "unclosed opener consumes the tail",
			input:         "Answer: <think>cut off mid th",
			wantClean:     "Answer:",
			wantReasoning: "cut off mid th",
			wantFound:     true,
		},
		{
			name:      "incomplete tag marker is not an opener",
			input:     "a <think b> c",
			wantClean: "a <think b> c",
		},
		{
			name:          "unclosed wrapper truncated at tag start",
			input:         "<think>only thoughts, never closed",
			wantClean:     "",
			wantReasoning: "only thoughts, never closed",
			wantFound:     true,
		},
		{
			name:          "nested wrappers best-effort",
			input:         "<think>a<reasoning>b</reasoning>c</think>out",
			wantClean:     "out",
			wantReasoning: "a<reasoning>b</reasoning>c",
			wantFound:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, reasoning, found := StripReasoningBlocks(tc.input)
			if clean != tc.wantClean {
				t.Fatalf("clean = %q, want %q", clean, tc.wantClean)
			}
			if reasoning != tc.wantReasoning {
				t.Fatalf("reasoning = %q, want %q", reasoning, tc.wantReasoning)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Wire capability detection
// ---------------------------------------------------------------------------

func TestWireCapabilitiesDetectionTable(t *testing.T) {
	table := NewWireCapabilityTable(nil)
	cases := []struct {
		model             string
		jsonResp          bool
		maxCompletionToks bool
		reasoning         bool
		ctx               int
		wantModelField    string
	}{
		{model: "gpt-4o", jsonResp: true, ctx: 128000, wantModelField: "gpt-4o"},
		{model: "GPT-4O", jsonResp: true, ctx: 128000, wantModelField: "gpt-4o"},
		{model: "openai/gpt-4o", jsonResp: true, ctx: 128000, wantModelField: "gpt-4o"},
		{model: "gpt-4o-mini", jsonResp: true, ctx: 128000, wantModelField: "gpt-4o-mini"},
		{model: "gpt-4.1", jsonResp: true, ctx: 1000000, wantModelField: "gpt-4.1"},
		{model: "gpt-5", jsonResp: true, maxCompletionToks: true, ctx: 400000, wantModelField: "gpt-5"},
		{model: "gpt-5-nano", jsonResp: true, maxCompletionToks: true, ctx: 400000, wantModelField: "gpt-5-nano"},
		{model: "gpt-5-turbo-custom", jsonResp: true, maxCompletionToks: true, ctx: 400000, wantModelField: "gpt-5-turbo-custom"},
		{model: "o1", jsonResp: true, maxCompletionToks: true, reasoning: true, ctx: 200000, wantModelField: "o1"},
		{model: "o1-mini", maxCompletionToks: true, reasoning: true, ctx: 128000, wantModelField: "o1-mini"},
		{model: "o3", jsonResp: true, maxCompletionToks: true, reasoning: true, ctx: 200000, wantModelField: "o3"},
		{model: "o3-mini", jsonResp: true, maxCompletionToks: true, reasoning: true, ctx: 200000, wantModelField: "o3-mini"},
		{model: "o4-mini", jsonResp: true, maxCompletionToks: true, reasoning: true, ctx: 200000, wantModelField: "o4-mini"},
		{model: "deepseek-chat", jsonResp: true, ctx: 64000, wantModelField: "deepseek-chat"},
		{model: "deepseek-reasoner", reasoning: true, ctx: 64000, wantModelField: "deepseek-reasoner"},
		{model: "llama-3.1-8b-instruct", ctx: 128000, wantModelField: "llama-3.1-8b-instruct"},
		{model: "mistral-small", ctx: 128000, wantModelField: "mistral-small"},
		{model: "totally-unknown-model", wantModelField: "totally-unknown-model"},
		{model: "", wantModelField: ""},
	}
	for _, tc := range cases {
		got := table.Detect(tc.model)
		if got.SupportsJSONResponse != tc.jsonResp ||
			got.UsesMaxCompletionTokens != tc.maxCompletionToks ||
			got.SupportsReasoning != tc.reasoning ||
			got.MaxContextTokens != tc.ctx ||
			got.Model != tc.wantModelField {
			t.Fatalf("Detect(%q) = %+v, want json=%v mct=%v reasoning=%v ctx=%d model=%q",
				tc.model, got, tc.jsonResp, tc.maxCompletionToks, tc.reasoning, tc.ctx, tc.wantModelField)
		}
	}
}

func TestWireCapabilitiesOverrides(t *testing.T) {
	table := NewWireCapabilityTable(nil)
	// A vendor-scoped override matches exactly; other vendors keep built-ins.
	table.Register(WireCapabilities{Model: "together/gpt-4o", SupportsJSONResponse: true, MaxContextTokens: 42})
	if got := table.Detect("together/gpt-4o"); got.MaxContextTokens != 42 {
		t.Fatalf("exact vendor override must win, got %+v", got)
	}
	if got := table.Detect("other/gpt-4o"); got.MaxContextTokens != 128000 {
		t.Fatalf("unregistered vendor prefix must resolve via built-in suffix, got %+v", got)
	}
	// A bare-id override REPLACES the built-in — narrowing must be possible
	// (a gateway may reject features the raw model supports) — and, per the
	// suffix resolution rule, also covers "vendor/gpt-4o" ids.
	table.Register(WireCapabilities{Model: "GPT-4O", MaxContextTokens: 999})
	if got := table.Detect("gpt-4o"); got.SupportsJSONResponse || got.MaxContextTokens != 999 {
		t.Fatalf("override must replace the built-in entry, got %+v", got)
	}
	if got := table.Detect("any/gpt-4o"); got.MaxContextTokens != 999 {
		t.Fatalf("bare-id override must cover vendor-suffixed ids, got %+v", got)
	}
	// Blank ids are ignored.
	table.Register(WireCapabilities{MaxContextTokens: 7})
	if got := table.Detect("mystery"); got.MaxContextTokens != 0 {
		t.Fatalf("blank override must be ignored, got %+v", got)
	}
}

func TestWireCapabilitiesFromEnv(t *testing.T) {
	t.Setenv(WireCapabilitiesEnvVar, `[{"model":"gpt-4o","supports_json_response":false,"max_context_tokens":555},{"model":"custom-x","uses_max_completion_tokens":true}]`)
	table := WireCapabilityTableFromEnv()
	if got := table.Detect("gpt-4o"); got.SupportsJSONResponse || got.MaxContextTokens != 555 {
		t.Fatalf("env override must narrow gpt-4o, got %+v", got)
	}
	if got := table.Detect("custom-x"); !got.UsesMaxCompletionTokens {
		t.Fatalf("env override must add custom-x, got %+v", got)
	}
	// Invalid JSON is ignored (built-ins stay effective).
	t.Setenv(WireCapabilitiesEnvVar, `{not-an-array`)
	table2 := WireCapabilityTableFromEnv()
	if got := table2.Detect("gpt-4o"); !got.SupportsJSONResponse {
		t.Fatalf("invalid override JSON must keep built-ins, got %+v", got)
	}
}

func TestWireCapabilitiesFromJSONValidation(t *testing.T) {
	if _, err := WireCapabilitiesFromJSON(""); err == nil {
		t.Fatal("empty payload must be rejected")
	}
	if _, err := WireCapabilitiesFromJSON(`{`); err == nil {
		t.Fatal("invalid JSON must be rejected")
	}
	if _, err := WireCapabilitiesFromJSON(`[{"supports_json_response":true}]`); err == nil {
		t.Fatal("entry without a model id must be rejected")
	}
	entries, err := WireCapabilitiesFromJSON(`[{"model":"a"},{"model":"b","supports_json_response":true}]`)
	if err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	if len(entries) != 2 || entries[1].Model != "b" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

// ---------------------------------------------------------------------------
// 4. Prompt shrink (token-limit retry)
// ---------------------------------------------------------------------------

func TestShrinkRequestForTokenLimitTable(t *testing.T) {
	big := strings.Repeat("x", 4000)
	cases := []struct {
		name        string
		req         CompletionRequest
		ctxTokens   int
		wantChanged bool
		wantRoles   []string // nil = don't assert
	}{
		{
			name:        "prompt-only form has nothing to drop",
			req:         CompletionRequest{Prompt: big},
			wantChanged: false,
		},
		{
			name: "system-only conversation is untouched",
			req: CompletionRequest{Messages: []Message{
				{Role: "system", Content: "s1"},
				{Role: "system", Content: "s2"},
			}},
			wantChanged: false,
		},
		{
			name: "oldest non-system messages dropped first, system kept",
			req: CompletionRequest{Messages: []Message{
				{Role: "system", Content: big},
				{Role: "user", Content: big},
				{Role: "assistant", Content: big},
				{Role: "user", Content: big},
			}},
			wantChanged: true,
			// Unknown context → halve the estimate: dropping the two oldest
			// non-system messages already fits, so the last user turn stays.
			wantRoles: []string{"system", "user"},
		},
		{
			name: "known roomy context needs no drop",
			req: CompletionRequest{Messages: []Message{
				{Role: "system", Content: "s"},
				{Role: "user", Content: "u"},
			}},
			ctxTokens:   128000,
			wantChanged: false,
		},
		{
			name: "tiny known context drops everything non-system",
			req: CompletionRequest{
				MaxTokens: 4096,
				Messages: []Message{
					{Role: "system", Content: "keep"},
					{Role: "user", Content: big},
				},
			},
			ctxTokens:   3000,
			wantChanged: true,
			wantRoles:   []string{"system"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrunk, changed := ShrinkRequestForTokenLimit(tc.req, tc.ctxTokens)
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if tc.wantRoles == nil {
				return
			}
			roles := make([]string, 0, len(shrunk.Messages))
			for _, m := range shrunk.Messages {
				roles = append(roles, m.Role)
			}
			if strings.Join(roles, ",") != strings.Join(tc.wantRoles, ",") {
				t.Fatalf("remaining roles = %v, want %v", roles, tc.wantRoles)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 5. Retry-After parsing and backoff math
// ---------------------------------------------------------------------------

func TestParseRetryAfterTable(t *testing.T) {
	if d, ok := ParseRetryAfter("2"); !ok || d != 2*time.Second {
		t.Fatalf("seconds value: %v %v", d, ok)
	}
	if d, ok := ParseRetryAfter(" 0 "); !ok || d != 0 {
		t.Fatalf("zero value must parse: %v %v", d, ok)
	}
	if _, ok := ParseRetryAfter("-3"); ok {
		t.Fatal("negative seconds must be rejected")
	}
	if _, ok := ParseRetryAfter(""); ok {
		t.Fatal("empty must be rejected")
	}
	if _, ok := ParseRetryAfter("soon"); ok {
		t.Fatal("garbage must be rejected")
	}
	future := time.Now().UTC().Add(30 * time.Second).Format(http.TimeFormat)
	d, ok := ParseRetryAfter(future)
	if !ok || d < 29*time.Second || d > 31*time.Second {
		t.Fatalf("HTTP-date: %v %v", d, ok)
	}
	if d, ok := ParseRetryAfter("Wed, 11 Nov 1999 00:00:00 GMT"); !ok || d != 0 {
		t.Fatalf("past HTTP-date must clamp to 0: %v %v", d, ok)
	}
}

func TestBackoffDelayFullJitterAndCaps(t *testing.T) {
	r := NewResilience(ResilienceConfig{
		Enabled: true,
		Retry: RetryConfig{
			Enabled:         true,
			MaxRetries:      3,
			BaseDelay:       200 * time.Millisecond,
			MaxDelay:        8 * time.Second,
			HonorRetryAfter: false,
		},
	}, WithResilienceJitter(func() float64 { return 0.5 }))
	// Full jitter: 0.5 * min(200ms*2^attempt, 8s).
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	for attempt, w := range want {
		if got := r.BackoffDelay(attempt, ""); got != w {
			t.Fatalf("attempt %d delay = %v, want %v", attempt, got, w)
		}
	}
	// The exponent cap: far-future attempts ride MaxDelay.
	if got := r.BackoffDelay(40, ""); got != 8*time.Second/2 {
		t.Fatalf("capped exponent delay = %v, want %v", got, 4*time.Second)
	}
	// Zero config fields take the documented defaults (jitter 0.5 → half the
	// default base delay).
	zero := NewResilience(ResilienceConfig{Enabled: true, Retry: RetryConfig{Enabled: true}}, WithResilienceJitter(func() float64 { return 0.5 }))
	if got := zero.BackoffDelay(0, ""); got != DefaultRetryBaseDelay/2 {
		t.Fatalf("zero base delay must default, got %v", got)
	}
}

func TestBackoffDelayHonorsRetryAfter(t *testing.T) {
	r := NewResilience(DefaultResilienceConfig(), WithResilienceJitter(func() float64 { return 0 }))
	if got := r.BackoffDelay(0, "5"); got != 5*time.Second {
		t.Fatalf("Retry-After must win over backoff, got %v", got)
	}
	// Retry-After is capped at MaxDelay.
	if got := r.BackoffDelay(0, "999"); got != DefaultRetryMaxDelay {
		t.Fatalf("Retry-After must cap at MaxDelay, got %v", got)
	}
	// HonorRetryAfter=false ignores the header (jitter 1 → the full base).
	noHonor := NewResilience(ResilienceConfig{
		Enabled: true,
		Retry:   RetryConfig{Enabled: true, MaxRetries: 1, BaseDelay: time.Second, MaxDelay: time.Second},
	}, WithResilienceJitter(func() float64 { return 1 }))
	if got := noHonor.BackoffDelay(0, "5"); got != time.Second {
		t.Fatalf("disabled Retry-After must use backoff, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// 6. JSON negotiation over the wire
// ---------------------------------------------------------------------------

func TestOpenAINegotiatesJSONFormat(t *testing.T) {
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured, chatOK(`{"a":1}`, "stop"))
	p := NewOpenAIProvider("stub", "k", srv.URL, "gpt-4o", srv.Client())

	resp, err := p.Complete(context.Background(), CompletionRequest{Prompt: "give me json", WantJSON: true, MaxTokens: 128})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != `{"a":1}` {
		t.Fatalf("text = %q", resp.Text)
	}
	if len(captured) != 1 {
		t.Fatalf("requests = %d, want 1", len(captured))
	}
	if captured[0].ResponseFormat == nil || captured[0].ResponseFormat.Type != "json_object" {
		t.Fatalf("response_format json_object must be negotiated for supported models: %+v", captured[0])
	}
	if captured[0].MaxTokens != 128 {
		t.Fatalf("legacy max_tokens must carry the budget, got %+v", captured[0])
	}

	// Clean JSON on a supported model: no repair metadata, no fallback.
	if resp.Metadata[MetaJSONFallback] != "" || resp.Metadata[MetaJSONRepaired] != "" {
		t.Fatalf("unexpected resilience metadata: %+v", resp.Metadata)
	}

	// Unsupported model: the parameter is never sent.
	var captured2 []capturedChatRequest
	srv2 := newScriptedChatServer(t, &captured2, chatOK(`{"a":1}`, "stop"))
	p2 := NewOpenAIProvider("stub", "k", srv2.URL, "llama-3.1-8b-instruct", srv2.Client())
	if _, err := p2.Complete(context.Background(), CompletionRequest{Prompt: "json please", WantJSON: true}); err != nil {
		t.Fatalf("unsupported-model Complete: %v", err)
	}
	if len(captured2) != 1 || captured2[0].ResponseFormat != nil {
		t.Fatalf("response_format must not be sent to unsupported models: %+v", captured2)
	}
}

func TestOpenAIUsesMaxCompletionTokensForGPT5(t *testing.T) {
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured, chatOK("ok", "stop"))
	p := NewOpenAIProvider("stub", "k", srv.URL, "gpt-5", srv.Client())

	if _, err := p.Complete(context.Background(), CompletionRequest{Prompt: "hi", MaxTokens: 777}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if captured[0].MaxTokens != 0 || captured[0].MaxCompletionTokens != 777 {
		t.Fatalf("gpt-5 must use max_completion_tokens: %+v", captured[0])
	}
}

func TestOpenAINegotiationFallbackOnRejection(t *testing.T) {
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured,
		scriptedChatResponse{status: http.StatusBadRequest, body: `{"error":{"message":"response_format json_object not supported"}}`},
		chatOK(`{"a":1}`, "stop"),
	)
	p := NewOpenAIProvider("stub", "k", srv.URL, "gpt-4o", srv.Client())

	resp, err := p.Complete(context.Background(), CompletionRequest{Prompt: "json", WantJSON: true})
	if err != nil {
		t.Fatalf("Complete after fallback: %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("requests = %d, want 2 (negotiated + prompt-only fallback)", len(captured))
	}
	if captured[0].ResponseFormat == nil {
		t.Fatal("the first attempt must carry response_format")
	}
	if captured[1].ResponseFormat != nil {
		t.Fatal("the fallback attempt must be prompt-only (no response_format)")
	}
	if resp.Metadata[MetaJSONFallback] != "prompt_only" {
		t.Fatalf("fallback must be recorded in metadata: %+v", resp.Metadata)
	}
}

func TestOpenAINegotiationRejectionOfFallbackSurfaces(t *testing.T) {
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured,
		scriptedChatResponse{status: http.StatusBadRequest, body: `{"error":"response_format rejected"}`},
		scriptedChatResponse{status: http.StatusBadRequest, body: `{"error":"still bad"}`},
	)
	p := NewOpenAIProvider("stub", "k", srv.URL, "gpt-4o", srv.Client())

	_, err := p.Complete(context.Background(), CompletionRequest{Prompt: "json", WantJSON: true})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("the second rejection must surface, got %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("the fallback must be a one-shot retry, saw %d requests", len(captured))
	}
}

// ---------------------------------------------------------------------------
// 7. Response post-processing: stripping + repair
// ---------------------------------------------------------------------------

func TestOpenAIRepairsAndStripsResponse(t *testing.T) {
	var captured []capturedChatRequest
	content := "<think>internal draft</think>```json\n{\"a\": 1} hope that helps\n```"
	srv := newScriptedChatServer(t, &captured, chatOK(content, "stop"))
	p := NewOpenAIProvider("stub", "k", srv.URL, "gpt-4o", srv.Client())

	resp, err := p.Complete(context.Background(), CompletionRequest{Prompt: "json", WantJSON: true})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != `{"a": 1}` {
		t.Fatalf("repaired text = %q, want the clean JSON", resp.Text)
	}
	if resp.Reasoning != "internal draft" {
		t.Fatalf("reasoning = %q, want the stripped block", resp.Reasoning)
	}
	if resp.Metadata[MetaReasoningStripped] != "true" {
		t.Fatalf("reasoning_stripped metadata missing: %+v", resp.Metadata)
	}
	if got := resp.Metadata[MetaJSONRepaired]; got != "padding,fences,trailing_text" {
		t.Fatalf("json_repaired = %q", got)
	}
}

// TestOpenAIFenceRepairIsBestEffortForUnpromptedPayloads pins the
// regression guard: repair must never fail a request that did not ask for
// JSON — a fenced non-JSON block (e.g. a code snippet) comes back untouched.
func TestOpenAIFenceRepairIsBestEffortForUnpromptedPayloads(t *testing.T) {
	content := "```go\nfunc main() { println(\"hi\") }\n```"
	srv := newScriptedChatServer(t, nil, chatOK(content, "stop"))
	p := NewOpenAIProvider("stub", "k", srv.URL, "gpt-4o", srv.Client())

	resp, err := p.Complete(context.Background(), CompletionRequest{Prompt: "write go code"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != content {
		t.Fatalf("unprompted fenced payload must be returned untouched, got %q", resp.Text)
	}
}

func TestOpenAIUnrepairableJSONSurfacesForJSONRequests(t *testing.T) {
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured, chatOK(`{"a": [1,2}`, "stop"))
	p := NewOpenAIProvider("stub", "k", srv.URL, "gpt-4o", srv.Client())

	// Prompt-only request: nothing droppable → no token-limit retry.
	_, err := p.Complete(context.Background(), CompletionRequest{Prompt: "json", WantJSON: true})
	if !errors.Is(err, ErrUnrepairableJSON) || !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("unrepairable JSON must surface as a typed error, got %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("requests = %d, want 1 (nothing to shrink)", len(captured))
	}
}

func TestOpenAITruncationRetryWithReducedPrompt(t *testing.T) {
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured,
		chatOK(`{"a": [1,2}`, "length"), // truncated + unrepairable
		chatOK(`{"ok":true}`, "stop"),
	)
	// Unknown model → unknown context → the heuristic half-budget applies
	// and the oldest non-system messages are dropped.
	p := NewOpenAIProvider("stub", "k", srv.URL, "shrug-unknown-model", srv.Client())

	req := CompletionRequest{WantJSON: true, Messages: []Message{
		{Role: "system", Content: "you are terse"},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "earlier answer"},
		{Role: "user", Content: "final question"},
	}}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete after truncation retry: %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("requests = %d, want 2 (original + one truncation retry)", len(captured))
	}
	if n1, n2 := len(captured[0].Messages), len(captured[1].Messages); n2 >= n1 {
		t.Fatalf("the retry must carry a reduced prompt: %d -> %d messages", n1, n2)
	}
	if captured[1].Messages[0].Role != "system" {
		t.Fatalf("system messages must be preserved, got %+v", captured[1].Messages[0])
	}
	if resp.Text != `{"ok":true}` {
		t.Fatalf("retry response text = %q", resp.Text)
	}
	if resp.Metadata[MetaTruncationRetry] != "true" {
		t.Fatalf("truncation_retry metadata missing: %+v", resp.Metadata)
	}
}

// TestOpenAITruncationRetryOnFinishReasonLength pins trigger #1: a
// finish_reason=length response is retried ONCE with a reduced prompt even
// when the payload is plain text (nothing to repair), and the retry bound
// holds when the retry itself is truncated again.
func TestOpenAITruncationRetryOnFinishReasonLength(t *testing.T) {
	// Plain-text truncation: one retry with the oldest non-system messages
	// dropped; the complete answer wins.
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured,
		chatOK("partial ans", "length"),
		chatOK("complete answer", "stop"),
	)
	p := NewOpenAIProvider("stub", "k", srv.URL, "shrug-unknown-model", srv.Client())

	resp, err := p.Complete(context.Background(), CompletionRequest{Messages: []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "older question"},
		{Role: "assistant", Content: "older answer"},
		{Role: "user", Content: "current question"},
	}})
	if err != nil {
		t.Fatalf("Complete after finish_reason=length retry: %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("requests = %d, want 2 (original + one length retry)", len(captured))
	}
	if n1, n2 := len(captured[0].Messages), len(captured[1].Messages); n2 >= n1 {
		t.Fatalf("the retry must carry a reduced prompt: %d -> %d messages", n1, n2)
	}
	if captured[1].Messages[0].Role != "system" {
		t.Fatalf("system messages must be preserved, got %+v", captured[1].Messages[0])
	}
	if resp.Text != "complete answer" {
		t.Fatalf("text = %q", resp.Text)
	}
	if resp.Metadata[MetaTruncationRetry] != "true" {
		t.Fatalf("truncation_retry metadata missing: %+v", resp.Metadata)
	}

	// The retry is bounded to ONE even when it truncates again: the second
	// (still length-cut) answer is returned as-is.
	var captured2 []capturedChatRequest
	srv2 := newScriptedChatServer(t, &captured2,
		chatOK("cut 1", "length"),
		chatOK("cut 2", "length"),
	)
	p2 := NewOpenAIProvider("stub", "k", srv2.URL, "shrug-unknown-model", srv2.Client())
	resp2, err := p2.Complete(context.Background(), CompletionRequest{Messages: []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u1"},
		{Role: "user", Content: "u2"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(captured2) != 2 {
		t.Fatalf("requests = %d, want 2 (ONE retry, never more)", len(captured2))
	}
	if resp2.Text != "cut 2" {
		t.Fatalf("text after the bounded retry = %q", resp2.Text)
	}
}

func TestOpenAITruncationRetryOptOutAndRoomyContext(t *testing.T) {
	// Opt-out: the unrepairable error surfaces after a single attempt.
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured, chatOK(`{"a": [1,2}`, "length"))
	cfg := DefaultResilienceConfig()
	cfg.TruncationRetry.Enabled = false
	p := newResilientProvider(t, srv, cfg, newSteppingClock(), "shrug-unknown-model")
	if _, err := p.Complete(context.Background(), CompletionRequest{
		WantJSON: true,
		Messages: []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
	}); !errors.Is(err, ErrUnrepairableJSON) {
		t.Fatalf("opted-out truncation retry must surface the error, got %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("requests = %d, want 1", len(captured))
	}

	// Known roomy context: the reduced prompt would not help, so no retry.
	var captured2 []capturedChatRequest
	srv2 := newScriptedChatServer(t, &captured2, chatOK(`{"a": [1,2}`, "length"))
	p2 := NewOpenAIProvider("stub", "k", srv2.URL, "gpt-4o", srv2.Client())
	if _, err := p2.Complete(context.Background(), CompletionRequest{
		WantJSON: true,
		Messages: []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
	}); !errors.Is(err, ErrUnrepairableJSON) {
		t.Fatalf("roomy-context unrepairable JSON must surface, got %v", err)
	}
	if len(captured2) != 1 {
		t.Fatalf("requests = %d, want 1 (shrinking cannot help a roomy context)", len(captured2))
	}
}

// ---------------------------------------------------------------------------
// 8. Backoff retry over the wire (fake clock, deterministic)
// ---------------------------------------------------------------------------

func TestOpenAIBackoffRetries429HonoringRetryAfter(t *testing.T) {
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured,
		scriptedChatResponse{status: http.StatusTooManyRequests, body: `{"error":"slow down"}`, retryAfter: "2"},
		chatOK("after wait", "stop"),
	)
	clk := newSteppingClock()
	p := newResilientProvider(t, srv, DefaultResilienceConfig(), clk, "gpt-4o")

	resp, err := p.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "after wait" {
		t.Fatalf("text = %q", resp.Text)
	}
	if len(captured) != 2 {
		t.Fatalf("requests = %d, want 2", len(captured))
	}
	if len(clk.slept) != 1 || clk.slept[0] != 2*time.Second {
		t.Fatalf("Retry-After must be honored: slept %v", clk.slept)
	}
}

func TestOpenAIBackoffSequenceAndExhaustion(t *testing.T) {
	// Three 5xx then success: sleeps follow full jitter (0.5×) over the
	// exponential schedule.
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured,
		scriptedChatResponse{status: http.StatusInternalServerError, body: `{"error":"1"}`},
		scriptedChatResponse{status: http.StatusBadGateway, body: `{"error":"2"}`},
		scriptedChatResponse{status: http.StatusServiceUnavailable, body: `{"error":"3"}`},
		chatOK("recovered", "stop"),
	)
	clk := newSteppingClock()
	p := newResilientProvider(t, srv, DefaultResilienceConfig(), clk, "gpt-4o")

	resp, err := p.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "recovered" || len(captured) != 4 {
		t.Fatalf("requests = %d text = %q, want 4 requests and recovery", len(captured), resp.Text)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	if len(clk.slept) != len(want) {
		t.Fatalf("sleeps = %v, want %v", clk.slept, want)
	}
	for i, w := range want {
		if clk.slept[i] != w {
			t.Fatalf("sleep[%d] = %v, want %v", i, clk.slept[i], w)
		}
	}

	// Exhaustion: every attempt 5xx → the last typed error surfaces after
	// MaxRetries retries (4 attempts total with the default policy).
	var captured2 []capturedChatRequest
	srv2 := newScriptedChatServer(t, &captured2,
		scriptedChatResponse{status: http.StatusInternalServerError, body: `{"error":"down"}`})
	clk2 := newSteppingClock()
	p2 := newResilientProvider(t, srv2, DefaultResilienceConfig(), clk2, "gpt-4o")
	if _, err := p2.Complete(context.Background(), CompletionRequest{Prompt: "hi"}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("exhausted retries must surface ErrProviderUnavailable, got %v", err)
	}
	if len(captured2) != DefaultMaxRetries+1 {
		t.Fatalf("requests = %d, want %d (1 + %d retries)", len(captured2), DefaultMaxRetries+1, DefaultMaxRetries)
	}
	if len(clk2.slept) != DefaultMaxRetries {
		t.Fatalf("sleeps = %v, want %d", clk2.slept, DefaultMaxRetries)
	}
}

func TestOpenAIBackoffNonTransientNeverRetried(t *testing.T) {
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured,
		scriptedChatResponse{status: http.StatusUnauthorized, body: `{"error":"bad key"}`})
	clk := newSteppingClock()
	p := newResilientProvider(t, srv, DefaultResilienceConfig(), clk, "gpt-4o")

	if _, err := p.Complete(context.Background(), CompletionRequest{Prompt: "hi"}); !errors.Is(err, ErrProviderAuth) {
		t.Fatalf("auth failure must surface immediately, got %v", err)
	}
	if len(captured) != 1 || len(clk.slept) != 0 {
		t.Fatalf("auth failure must not retry: %d requests, sleeps %v", len(captured), clk.slept)
	}
}

func TestOpenAIBackoffRespectsContextCancellationDuringWait(t *testing.T) {
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured,
		scriptedChatResponse{status: http.StatusTooManyRequests, body: `{"error":"429"}`})
	// Real clock here (no sleeper): the wait is longer than the ctx budget,
	// so the sleep is interrupted deterministically.
	cfg := DefaultResilienceConfig()
	cfg.Retry.BaseDelay = 50 * time.Millisecond
	cfg.Retry.MaxDelay = 50 * time.Millisecond
	p := NewOpenAIProvider("stub", "k", srv.URL, "gpt-4o", srv.Client())
	p.SetResilience(NewResilience(cfg, WithResilienceJitter(func() float64 { return 0.5 })))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := p.Complete(ctx, CompletionRequest{Prompt: "hi"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation during the backoff wait must surface ctx.Err(), got %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("requests = %d, want 1 (no retry after cancellation)", len(captured))
	}
}

// ---------------------------------------------------------------------------
// 9. Opt-out / legacy behavior
// ---------------------------------------------------------------------------

func TestOpenAIResilienceDefaultsOn(t *testing.T) {
	p := NewOpenAIProvider("stub", "k", "http://localhost:1/v1", "gpt-4o", nil)
	if p.Resilience() == nil {
		t.Fatal("NewOpenAIProvider must install the resilience layer (defaults ON)")
	}
	if got, want := p.Resilience().Config(), DefaultResilienceConfig(); got != want {
		t.Fatalf("default config = %+v, want %+v", got, want)
	}
}

func TestOpenAILegacyBehaviorWithNilResilience(t *testing.T) {
	// 5xx: exactly one attempt, no metadata rewriting, typed error.
	var captured []capturedChatRequest
	srv := newScriptedChatServer(t, &captured,
		scriptedChatResponse{status: http.StatusInternalServerError, body: `{"error":"down"}`})
	p := NewOpenAIProvider("stub", "k", srv.URL, "gpt-4o", srv.Client())
	p.SetResilience(nil)

	if _, err := p.Complete(context.Background(), CompletionRequest{Prompt: "hi"}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("legacy behavior must surface the error, got %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("legacy behavior must be single-attempt, saw %d", len(captured))
	}

	// Even gpt-5 keeps the legacy max_tokens field when the layer is off.
	var captured2 []capturedChatRequest
	srv2 := newScriptedChatServer(t, &captured2, chatOK("ok", "stop"))
	p2 := NewOpenAIProvider("stub", "k", srv2.URL, "gpt-5", srv2.Client())
	p2.SetResilience(nil)
	if _, err := p2.Complete(context.Background(), CompletionRequest{Prompt: "hi", MaxTokens: 99}); err != nil {
		t.Fatalf("legacy Complete: %v", err)
	}
	if captured2[0].MaxTokens != 99 || captured2[0].MaxCompletionTokens != 0 {
		t.Fatalf("legacy payload must use max_tokens: %+v", captured2[0])
	}

	// Reasoning wrappers stay in the text when stripping is off.
	var captured3 []capturedChatRequest
	srv3 := newScriptedChatServer(t, &captured3, chatOK("<think>x</think>answer", "stop"))
	p3 := NewOpenAIProvider("stub", "k", srv3.URL, "gpt-4o", srv3.Client())
	p3.SetResilience(nil)
	resp, err := p3.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("legacy Complete: %v", err)
	}
	if resp.Text != "<think>x</think>answer" || resp.Reasoning != "" {
		t.Fatalf("legacy behavior must not post-process: %+v", resp)
	}
}

func TestOpenAIResilienceConfigZeroDisablesConcerns(t *testing.T) {
	var captured []capturedChatRequest
	// Enabled=true but every concern off: plain single-attempt passthrough
	// with reasoning wrappers intact and no negotiation.
	srv := newScriptedChatServer(t, &captured, chatOK("<think>x</think>plain text", "stop"))
	cfg := ResilienceConfig{Enabled: true}
	p := newResilientProvider(t, srv, cfg, newSteppingClock(), "gpt-4o")

	resp, err := p.Complete(context.Background(), CompletionRequest{Prompt: "hi", WantJSON: true})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "<think>x</think>plain text" || resp.Reasoning != "" {
		t.Fatalf("disabled concerns must not post-process: %+v", resp)
	}
	if captured[0].ResponseFormat != nil {
		t.Fatal("disabled negotiation must not send response_format")
	}
}
