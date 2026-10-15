package runtime

// Issue #134 tests: the ReACT agent pattern. Fake provider + fake tools
// keep every scenario deterministic and offline:
//
//   - multi-tool-per-step honored (and capped by max_tool_calls_per_step);
//   - reflection rounds bounded (and the annotation steps recorded);
//   - StopOnSatisfactory + quality_threshold gating;
//   - the AssessmentLLM seam (and its degrade-to-heuristic fallback);
//   - chunked long-output generation (plan → per-section with tool support
//     → assemble);
//   - token-budget and history-strategy integration UNCHANGED from the base
//     runtime (same budget.go/history.go code paths);
//   - the tool/policy seam (denied tools are never invoked);
//   - determinism with the fake LLM.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/models"
	"agentos/internal/policies"
	"agentos/internal/tools"
)

// countingTool is a deterministic fake tool: echoes its "text" argument back
// and records every invocation for assertions.
type countingTool struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (c *countingTool) Name() string { return "echo" }

func (c *countingTool) Execute(input map[string]any) (map[string]any, error) {
	c.mu.Lock()
	c.calls = append(c.calls, input)
	c.mu.Unlock()
	return map[string]any{"echo": fmt.Sprintf("%v", input["text"])}, nil
}

func (c *countingTool) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// reactProvider is the scripted fake LLM. It routes on the request shape the
// ReACT runner actually produces, so one provider can serve the whole pattern:
//
//   - req.System == assessmentPrompt        → the AssessmentLLM seam;
//   - last user message plans the document  → section planner;
//   - last user message writes a section    → section generator;
//   - anything else                         → the main ReACT loop.
//
// mainFn receives the 1-based main-loop call number and the request; when it
// returns nil the provider answers with a deterministic final answer.
type reactProvider struct {
	mu       sync.Mutex
	requests []models.CompletionRequest

	mainFn    func(n int, req models.CompletionRequest) *models.CompletionResponse
	assessFn  func(req models.CompletionRequest) *models.CompletionResponse
	planFn    func(req models.CompletionRequest) *models.CompletionResponse
	sectionFn func(req models.CompletionRequest) *models.CompletionResponse

	mainCalls, assessCalls, planCalls, sectionCalls int
}

func (p *reactProvider) Name() string { return "react-scripted" }

func (p *reactProvider) Complete(_ context.Context, req models.CompletionRequest) (*models.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)

	if req.System == assessmentPrompt && p.assessFn != nil {
		p.assessCalls++
		return p.assessFn(req), nil
	}
	last := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			last = req.Messages[i].Content
			break
		}
	}
	if p.planFn != nil && strings.HasPrefix(last, "Plan the document") {
		p.planCalls++
		return p.planFn(req), nil
	}
	if p.sectionFn != nil && (strings.Contains(last, "Now write section") || strings.Contains(last, "no tool JSON")) {
		p.sectionCalls++
		return p.sectionFn(req), nil
	}
	p.mainCalls++
	if p.mainFn == nil {
		return &models.CompletionResponse{Text: "done", Model: "react-scripted"}, nil
	}
	if resp := p.mainFn(p.mainCalls, req); resp != nil {
		return resp, nil
	}
	return &models.CompletionResponse{Text: "done", Model: "react-scripted"}, nil
}

// requestCount reports how many provider calls were made in total.
func (p *reactProvider) totalCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// lastUserOf returns the most recent user message of a captured request.
func lastUserOf(req models.CompletionRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	return ""
}

// toolReply builds a scripted multi-tool model reply.
func toolReply(calls ...string) *models.CompletionResponse {
	return &models.CompletionResponse{
		Text:  "{\"tools\":[" + strings.Join(calls, ",") + "]}",
		Model: "react-scripted",
		Usage: models.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
}

// textReply builds a scripted final-answer model reply.
func textReply(text string) *models.CompletionResponse {
	return &models.CompletionResponse{
		Text:  text,
		Model: "react-scripted",
		Usage: models.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
}

// newReactHarness builds a ReACTRunner over a fresh agent + registry with the
// given provider and options, plus the step recorder for assertions.
func newReactHarness(t *testing.T, provider models.Provider, reactOpts []ReACTOption, baseOpts ...Option) (*ReACTRunner, *recordingRecorder, *countingTool, string) {
	t.Helper()
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry()
	tool := &countingTool{}
	registry.Register(tool)
	rec := &recordingRecorder{}
	base := NewRunnerWithOptions(agentService, registry, append([]Option{WithProvider(provider), WithStepRecorder(rec)}, baseOpts...)...)
	rr := NewReACTRunner(base, reactOpts...)
	return rr, rec, tool, agent.ID
}

// reflectionSteps filters the recorded steps for reflection annotations.
func reflectionSteps(steps []Step) []Step {
	var out []Step
	for _, s := range steps {
		if s.Type == StepTypeReflection {
			out = append(out, s)
		}
	}
	return out
}

// toolSteps filters the recorded steps for tool executions.
func toolSteps(steps []Step) []Step {
	var out []Step
	for _, s := range steps {
		if s.Type == StepTypeTool {
			out = append(out, s)
		}
	}
	return out
}

// --- unit tests: parsing & heuristics ---------------------------------------

func TestParseReactToolCallsShapes(t *testing.T) {
	multi := `{"tools":[{"tool":"a","arguments":{"x":1}},{"tool":"b","arguments":{}},{"tool":"","arguments":{}},{"tool":"c"}]}`
	calls := parseReactToolCalls(multi, 5)
	if len(calls) != 3 {
		t.Fatalf("expected 3 valid calls (empty tool names skipped), got %d", len(calls))
	}
	if calls[0].Tool != "a" || calls[1].Tool != "b" || calls[2].Tool != "c" {
		t.Fatalf("unexpected calls: %+v", calls)
	}
	if calls[2].Arguments == nil {
		t.Fatal("nil arguments must be normalized to an empty map")
	}

	// Cap: the FIRST max requests win (deterministic).
	capped := parseReactToolCalls(multi, 2)
	if len(capped) != 2 || capped[0].Tool != "a" || capped[1].Tool != "b" {
		t.Fatalf("cap-first-N violated: %+v", capped)
	}

	// Legacy single-call shape (base runtime compatibility).
	single := parseReactToolCalls(`{"tool":"calc","arguments":{"expression":"1+1"}}`, 3)
	if len(single) != 1 || single[0].Tool != "calc" {
		t.Fatalf("legacy single shape broken: %+v", single)
	}

	// JSON array shape.
	array := parseReactToolCalls(`[{"tool":"a"},{"tool":"b"}]`, 3)
	if len(array) != 2 {
		t.Fatalf("array shape broken: %+v", array)
	}

	// Fenced replies are normalized.
	fenced := parseReactToolCalls("```json\n{\"tools\":[{\"tool\":\"a\"}]}\n```", 3)
	if len(fenced) != 1 || fenced[0].Tool != "a" {
		t.Fatalf("fenced shape broken: %+v", fenced)
	}

	// Plain text is a draft answer.
	if got := parseReactToolCalls("The answer is 42.", 3); len(got) != 0 {
		t.Fatalf("plain text must not parse as tool calls: %+v", got)
	}
}

func TestEstimateLongOutputTokensHeuristic(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"write a summary", 0},
		{"please write a comprehensive overview of X", keywordLongOutputEstimateTokens},
		{"write at least 3000 words about goldfish", 3900},
		{"a 2,500-word essay on tea", 3250},
		{"produce 5 pages of documentation", 3250},
	}
	for _, tc := range cases {
		if got := estimateLongOutputTokens(tc.in); got != tc.want {
			t.Errorf("estimateLongOutputTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseSectionPlanFallbacks(t *testing.T) {
	got := parseSectionPlan(`["Intro", "Body", "End"]`, 8)
	if len(got) != 3 || got[0] != "Intro" {
		t.Fatalf("json plan broken: %v", got)
	}
	lines := parseSectionPlan("- First\n* Second\n\n# Third", 8)
	if len(lines) != 3 || lines[0] != "First" {
		t.Fatalf("line fallback broken: %v", lines)
	}
	if capped := parseSectionPlan(`["a","b","c","d"]`, 2); len(capped) != 2 {
		t.Fatalf("cap broken: %v", capped)
	}
	if one := parseSectionPlan("just prose", 8); len(one) != 1 || one[0] != "just prose" {
		t.Fatalf("single-section fallback broken: %v", one)
	}
}

func TestReactConfigParsingAndMerge(t *testing.T) {
	// Empty document → zero config (defaults apply later).
	if cfg, err := ParseReactConfig(nil); err != nil || cfg != (ReactConfig{}) {
		t.Fatalf("empty doc must parse to zero config, got %+v err %v", cfg, err)
	}
	cfg, err := ParseReactConfig([]byte(`{"react":{"max_tool_calls_per_step":5,"stop_on_satisfactory":true,"quality_threshold":0.9}}`))
	if err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}
	if cfg.MaxToolCallsPerStep != 5 || !cfg.StopOnSatisfactory || cfg.QualityThreshold != 0.9 {
		t.Fatalf("unexpected parse: %+v", cfg)
	}
	for _, bad := range []string{
		`{"react":{"max_tool_calls_per_step":-1}}`,
		`{"react":{"quality_threshold":1.5}}`,
		`{"react":{"temperature":3}}`,
		`{"react":{"max_sections":-2}}`,
		`{not json`,
	} {
		if _, err := ParseReactConfig([]byte(bad)); err == nil {
			t.Errorf("invalid doc %q must be rejected", bad)
		}
	}

	// WithDefaults.
	d := ReactConfig{}.WithDefaults()
	if d.MaxToolCallsPerStep != DefaultMaxToolCallsPerStep || d.MaxReflectionRounds != DefaultMaxReflectionRounds ||
		d.QualityThreshold != DefaultQualityThreshold || d.LongOutputThresholdTokens != DefaultLongOutputThresholdTokens ||
		d.MaxSections != DefaultMaxSections || d.ReflectionPrompt != DefaultReflectionPrompt {
		t.Fatalf("defaults broken: %+v", d)
	}

	// Merge: non-zero override fields win; StopOnSatisfactory true-wins.
	merged := mergeReactConfig(ReactConfig{MaxReflectionRounds: -1, Temperature: 0.3}, cfg)
	if merged.MaxToolCallsPerStep != 5 || merged.QualityThreshold != 0.9 || !merged.StopOnSatisfactory {
		t.Fatalf("merge broken: %+v", merged)
	}
	if merged.MaxReflectionRounds != -1 || merged.Temperature != 0.3 {
		t.Fatalf("merge must keep base fields the override does not set: %+v", merged)
	}
}

// --- integration tests -------------------------------------------------------

func TestReactMultiToolCallsPerStep(t *testing.T) {
	p := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
		if n == 1 {
			return toolReply(
				`{"tool":"echo","arguments":{"text":"alpha"}}`,
				`{"tool":"echo","arguments":{"text":"beta"}}`,
				`{"tool":"echo","arguments":{"text":"gamma"}}`)
		}
		return textReply("all three echoed")
	}}
	rr, rec, tool, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{MaxReflectionRounds: -1})})

	run, err := rr.Run(context.Background(), agentID, "echo three things")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Status != StatusCompleted || run.Output != "all three echoed" {
		t.Fatalf("unexpected run: status=%s output=%q err=%s", run.Status, run.Output, run.Error)
	}
	if tool.count() != 3 {
		t.Fatalf("expected 3 tool invocations, got %d", tool.count())
	}
	steps := toolSteps(rec.all())
	if len(steps) != 3 {
		t.Fatalf("expected 3 recorded tool steps, got %d", len(steps))
	}
	for i, s := range steps {
		if s.Index != 1 {
			t.Errorf("tool step %d must share the reasoning step index 1, got %d", i, s.Index)
		}
		if s.Status != StepSucceeded {
			t.Errorf("tool step %d status = %s", i, s.Status)
		}
	}
	if p.mainCalls != 2 {
		t.Fatalf("expected exactly 2 main model calls, got %d", p.mainCalls)
	}
}

func TestReactMaxToolCallsPerStepCap(t *testing.T) {
	p := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
		if n == 1 {
			return toolReply(
				`{"tool":"echo","arguments":{"text":"one"}}`,
				`{"tool":"echo","arguments":{"text":"two"}}`,
				`{"tool":"echo","arguments":{"text":"three"}}`,
				`{"tool":"echo","arguments":{"text":"four"}}`)
		}
		return textReply("done")
	}}
	rr, rec, tool, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{
		MaxToolCallsPerStep: 2,
		MaxReflectionRounds: -1,
	})})

	if _, err := rr.Run(context.Background(), agentID, "echo four things"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if tool.count() != 2 {
		t.Fatalf("max_tool_calls_per_step=2 must cap executions at 2, got %d", tool.count())
	}
	if len(toolSteps(rec.all())) != 2 {
		t.Fatalf("expected exactly 2 recorded tool steps")
	}
}

func TestReactLegacySingleCallShape(t *testing.T) {
	p := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
		if n == 1 {
			return &models.CompletionResponse{Text: `{"tool":"echo","arguments":{"text":"legacy"}}`, Model: "m"}
		}
		return textReply("ok")
	}}
	rr, _, tool, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{MaxReflectionRounds: -1})})

	run, err := rr.Run(context.Background(), agentID, "use the tool")
	if err != nil || run.Output != "ok" {
		t.Fatalf("run failed: %v output=%q", err, run.Output)
	}
	if tool.count() != 1 {
		t.Fatalf("legacy single-call shape must execute one tool, got %d", tool.count())
	}
}

// weakDraft is a draft the deterministic heuristic always reflects on: short,
// no observation coverage, single sentence.
const weakDraft = "42"

func TestReactReflectionRoundsBounded(t *testing.T) {
	p := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
		switch n {
		case 1:
			return textReply(weakDraft)
		case 2:
			return textReply("draft two")
		default:
			return textReply("draft three (final)")
		}
	}}
	rr, rec, _, agentID := newReactHarness(t, p, nil) // defaults: MaxReflectionRounds=2

	run, err := rr.Run(context.Background(), agentID, "what is the answer?")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Output != "draft three (final)" {
		t.Fatalf("expected the final refined draft, got %q", run.Output)
	}
	if p.mainCalls != 3 {
		t.Fatalf("1 draft + 2 reflection rounds = 3 main calls, got %d", p.mainCalls)
	}
	notes := reflectionSteps(rec.all())
	if len(notes) != 2 {
		t.Fatalf("expected 2 reflection annotation steps (no annotation when rounds exhausted), got %d", len(notes))
	}
	// The reflection feedback must carry the documented default prompt and
	// the structured assessment JSON.
	req2 := lastUserOf(p.requests[1])
	if !strings.Contains(req2, DefaultReflectionPrompt) || !strings.Contains(req2, "Quality assessment of your draft (JSON)") {
		t.Fatalf("reflection feedback message incomplete: %q", req2)
	}
}

func TestReactStopOnSatisfactoryHaltsReflection(t *testing.T) {
	perfect := QualityAssessment{Completeness: 1, Accuracy: 1, Coherence: 1, ShouldReflect: true}
	for _, tc := range []struct {
		name       string
		stop       bool
		wantCalls  int
		wantOutput string
	}{
		{"stops on satisfactory", true, 1, "the only draft"},
		{"reflects when disabled", false, 3, "third draft"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
				switch n {
				case 1:
					return textReply("the only draft")
				case 2:
					return textReply("second draft")
				default:
					return textReply("third draft")
				}
			}}
			rr, _, _, agentID := newReactHarness(t, p, []ReACTOption{
				WithAssessor(AssessorFunc(func(context.Context, AssessRequest) (QualityAssessment, error) {
					return perfect, nil
				})),
				WithReactConfig(ReactConfig{StopOnSatisfactory: tc.stop}),
			})
			run, err := rr.Run(context.Background(), agentID, "q")
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if run.Output != tc.wantOutput {
				t.Fatalf("output = %q, want %q", run.Output, tc.wantOutput)
			}
			if p.mainCalls != tc.wantCalls {
				t.Fatalf("main calls = %d, want %d", p.mainCalls, tc.wantCalls)
			}
		})
	}
}

func TestReactQualityThreshold(t *testing.T) {
	middling := QualityAssessment{Completeness: 0.5, Accuracy: 0.5, Coherence: 0.5, ShouldReflect: true}
	newRunner := func(threshold float64) (*reactProvider, *ReACTRunner, string) {
		p := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
			if n == 1 {
				return textReply("draft one")
			}
			return textReply("refined")
		}}
		rr, _, _, agentID := newReactHarness(t, p, []ReACTOption{
			WithAssessor(AssessorFunc(func(context.Context, AssessRequest) (QualityAssessment, error) {
				return middling, nil
			})),
			WithReactConfig(ReactConfig{StopOnSatisfactory: true, QualityThreshold: threshold}),
		})
		return p, rr, agentID
	}

	// Threshold 0.8 > scores 0.5: never satisfied → rounds bounded at 2.
	p, rr, agentID := newRunner(0.8)
	if _, err := rr.Run(context.Background(), agentID, "q"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if p.mainCalls != 3 {
		t.Fatalf("threshold 0.8 with 0.5 scores must reflect until rounds exhaust (3 calls), got %d", p.mainCalls)
	}

	// Threshold 0.1 <= scores: satisfied immediately → 1 call.
	p2, rr2, agentID2 := newRunner(0.1)
	if _, err := rr2.Run(context.Background(), agentID2, "q"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if p2.mainCalls != 1 {
		t.Fatalf("threshold 0.1 with 0.5 scores must halt reflection (1 call), got %d", p2.mainCalls)
	}
}

func TestReactLLMAssessmentSeam(t *testing.T) {
	assessmentJSON := `{"completeness":0.4,"accuracy":0.4,"coherence":0.4,"should_reflect":true,"notes":"gaps"}`
	p := &reactProvider{
		mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
			if n == 1 {
				return textReply(weakDraft)
			}
			return textReply("refined answer")
		},
		assessFn: func(_ models.CompletionRequest) *models.CompletionResponse {
			return &models.CompletionResponse{Text: assessmentJSON, Model: "assessor", Usage: models.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}}
		},
	}
	rr, rec, _, agentID := newReactHarness(t, p, []ReACTOption{
		WithAssessmentProvider(p, "assessor-model"),
		WithReactConfig(ReactConfig{MaxReflectionRounds: 1}),
	})

	run, err := rr.Run(context.Background(), agentID, "q")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Output != "refined answer" {
		t.Fatalf("expected refined answer, got %q", run.Output)
	}
	if p.assessCalls != 1 {
		t.Fatalf("expected exactly 1 LLM assessment call, got %d", p.assessCalls)
	}
	// The assessment call is a real provider call: budgeted and recorded.
	if run.Tokens.TotalTokens != 40 { // 2 main (15+15) + 1 assessment (10)
		t.Fatalf("assessment usage must be run-accounted, got %d", run.Tokens.TotalTokens)
	}
	notes := reflectionSteps(rec.all())
	if len(notes) != 1 || !strings.Contains(notes[0].Output, "0.4") {
		t.Fatalf("reflection annotation must carry the structured assessment: %+v", notes)
	}
	// The next main call carries the assessment feedback.
	if !strings.Contains(lastUserOf(p.requests[2]), "0.4") {
		t.Fatalf("reflection feedback must include the LLM assessment")
	}
}

func TestReactLLMAssessmentMalformedDegradesToHeuristic(t *testing.T) {
	p := &reactProvider{
		mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
			if n < 3 {
				return textReply(weakDraft) // heuristic reflects on this
			}
			return textReply("final")
		},
		assessFn: func(_ models.CompletionRequest) *models.CompletionResponse {
			return &models.CompletionResponse{Text: "not json at all", Model: "assessor"}
		},
	}
	rr, rec, _, agentID := newReactHarness(t, p, []ReACTOption{WithAssessmentProvider(p, "")})

	run, err := rr.Run(context.Background(), agentID, "q")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Output != "final" {
		t.Fatalf("unexpected output %q", run.Output)
	}
	if p.assessCalls != 2 {
		t.Fatalf("both drafts must be LLM-assessed before degrading, got %d", p.assessCalls)
	}
	notes := reflectionSteps(rec.all())
	if len(notes) != 2 || !strings.Contains(notes[0].Output, "llm assessment failed") {
		t.Fatalf("degraded assessments must be annotated with the fallback reason: %+v", notes)
	}
}

// longSectionSectionIndex extracts the "write section N of M" index.
var sectionIndexRe = regexp.MustCompile(`section (\d+) of`)

// sectionIndexOf finds the "write section N of M" index across the whole
// request history (the section rewrite prompt no longer carries it).
func sectionIndexOf(req models.CompletionRequest) int {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		if m := sectionIndexRe.FindStringSubmatch(req.Messages[i].Content); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

func TestReactLongOutputChunking(t *testing.T) {
	p := &reactProvider{
		planFn: func(_ models.CompletionRequest) *models.CompletionResponse {
			return &models.CompletionResponse{Text: `["Intro","Body","End"]`, Model: "m", Usage: models.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
		},
		sectionFn: func(req models.CompletionRequest) *models.CompletionResponse {
			switch sectionIndexOf(req) {
			case 2:
				if !strings.Contains(lastUserOf(req), "no tool JSON") {
					// The section needs data first: request a tool.
					return toolReply(`{"tool":"echo","arguments":{"text":"sec2data"}}`)
				}
				return textReply("Body section citing sec2data.")
			case 1:
				return textReply("Intro section text.")
			default:
				return textReply("End section text.")
			}
		},
	}
	rr, rec, tool, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{
		MaxSections:         3,
		MaxReflectionRounds: -1, // chunked mode replaces reflection (documented)
	})})

	input := "Write a comprehensive report about goldfish care. It must be at least 3000 words."
	run, err := rr.Run(context.Background(), agentID, input)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	for _, want := range []string{"# Intro\n\nIntro section text.", "# Body\n\nBody section citing sec2data.", "# End\n\nEnd section text."} {
		if !strings.Contains(run.Output, want) {
			t.Fatalf("assembled document missing %q:\n%s", want, run.Output)
		}
	}
	if p.planCalls != 1 {
		t.Fatalf("expected exactly 1 planning call, got %d", p.planCalls)
	}
	if p.sectionCalls != 4 { // 3 sections + 1 tool-assisted rewrite for section 2
		t.Fatalf("expected 4 section calls, got %d", p.sectionCalls)
	}
	if tool.count() != 1 {
		t.Fatalf("section tool support must invoke the tool once, got %d", tool.count())
	}
	if len(toolSteps(rec.all())) != 1 {
		t.Fatalf("the in-section tool call must be recorded as a tool step")
	}
	// The rewrite prompt after the observation must be plain-text-only.
	var sawRewrite bool
	for _, req := range p.requests {
		if strings.Contains(lastUserOf(req), "no tool JSON") {
			sawRewrite = true
		}
	}
	if !sawRewrite {
		t.Fatal("section rewrite prompt missing")
	}
	if run.Steps != 5 { // 1 plan + section1 + section2 tool round + section2 rewrite + section3
		t.Fatalf("unexpected step count %d", run.Steps)
	}
}

func TestReactTokenBudgetExceeded(t *testing.T) {
	p := &reactProvider{mainFn: func(_ int, _ models.CompletionRequest) *models.CompletionResponse {
		return toolReply(`{"tool":"echo","arguments":{"text":"loop"}}`)
	}}
	rr, rec, _, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{MaxReflectionRounds: -1})},
		WithRuntimeConfig(RuntimeConfig{TokenBudget: TokenBudgetConfig{MaxTotalTokens: 25}}))

	run, err := rr.Run(context.Background(), agentID, "loop forever")
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Fatalf("expected ErrTokenBudgetExceeded, got %v", err)
	}
	if run.Status != StatusFailed || !strings.Contains(run.Error, "token_budget_exceeded") {
		t.Fatalf("run must fail with the machine-readable budget marker: %+v", run)
	}
	var budgetSteps []Step
	for _, s := range rec.all() {
		if s.Type == StepTypeBudget && s.Status == StepFailed {
			budgetSteps = append(budgetSteps, s)
		}
	}
	if len(budgetSteps) != 1 {
		t.Fatalf("expected 1 failed budget annotation, got %d", len(budgetSteps))
	}
}

func TestReactHistorySummarizeOlderIntegration(t *testing.T) {
	long := strings.Repeat("observation payload ", 30) // ~600 chars
	p := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
		if n < 3 {
			return toolReply(fmt.Sprintf(`{"tool":"echo","arguments":{"text":"%s"}}`, long))
		}
		return textReply("final answer")
	}}
	rr, _, _, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{MaxReflectionRounds: -1})},
		WithRuntimeConfig(RuntimeConfig{ConversationHistory: ConversationHistoryConfig{
			Strategy:  StrategySummarizeOlder,
			MaxTokens: 250,
		}}))

	run, err := rr.Run(context.Background(), agentID, "gather data then answer")
	if err != nil || run.Output != "final answer" {
		t.Fatalf("run failed: %v output=%q", err, run.Output)
	}
	// The LAST request's prompt view must carry the mechanical summary of the
	// dropped older turns (history strategy reused unchanged).
	last := lastUserOf(p.requests[len(p.requests)-1])
	first := p.requests[len(p.requests)-1].Messages[0]
	if !strings.HasPrefix(first.Content, "Summary of earlier conversation: ") {
		t.Fatalf("summarize_older strategy must produce the synthetic summary, got %q", first.Content)
	}
	_ = last
}

func TestReactLoopDetection(t *testing.T) {
	p := &reactProvider{mainFn: func(_ int, _ models.CompletionRequest) *models.CompletionResponse {
		return toolReply(`{"tool":"echo","arguments":{"text":"same"}}`)
	}}
	rr, _, _, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{MaxReflectionRounds: -1})})

	run, err := rr.Run(context.Background(), agentID, "loop")
	if !errors.Is(err, ErrLoopDetected) {
		t.Fatalf("expected ErrLoopDetected, got %v", err)
	}
	if run.Status != StatusFailed {
		t.Fatalf("expected failed status, got %s", run.Status)
	}
}

func TestReactToolPolicyDenialNeverInvokesTool(t *testing.T) {
	deny := &stubEnforcer{decision: policies.Decision{
		Decision:        policies.EffectDeny,
		MatchedPolicyID: "pol-react",
		Reason:          `deny by policy "block echo"`,
	}}
	p := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
		if n == 1 {
			return toolReply(`{"tool":"echo","arguments":{"text":"secret"}}`)
		}
		return textReply("answered without the tool")
	}}
	rr, rec, tool, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{MaxReflectionRounds: -1})},
		WithPolicyEnforcer(deny.enforcer()))

	run, err := rr.Run(context.Background(), agentID, "use the denied tool")
	if err != nil || run.Output != "answered without the tool" {
		t.Fatalf("run must complete through the denial observation: %v output=%q", err, run.Output)
	}
	if tool.count() != 0 {
		t.Fatalf("a policy-denied tool must NEVER be invoked, got %d invocations", tool.count())
	}
	steps := toolSteps(rec.all())
	if len(steps) != 1 || steps[0].Status != StepFailed || steps[0].Error == "" ||
		!strings.Contains(steps[0].Error, ToolPolicyDeniedCode) || steps[0].Policy == nil {
		t.Fatalf("denial must be visible on the recorded tool step: %+v", steps)
	}
}

func TestReactOfflineDelegatesToBaseRunner(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())
	rr := NewReACTRunner(NewRunner(agentService, registry)) // no provider

	run, err := rr.Run(context.Background(), agent.ID, "What is 2 + 2?")
	if err != nil || run.Output != "4" {
		t.Fatalf("offline delegation broken: %v output=%q", err, run.Output)
	}
}

func TestReactPerAgentConfigSource(t *testing.T) {
	p := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
		if n == 1 {
			return toolReply(
				`{"tool":"echo","arguments":{"text":"1"}}`, `{"tool":"echo","arguments":{"text":"2"}}`,
				`{"tool":"echo","arguments":{"text":"3"}}`, `{"tool":"echo","arguments":{"text":"4"}}`,
				`{"tool":"echo","arguments":{"text":"5"}}`, `{"tool":"echo","arguments":{"text":"6"}}`,
				`{"tool":"echo","arguments":{"text":"7"}}`, `{"tool":"echo","arguments":{"text":"8"}}`)
		}
		return textReply("done")
	}}
	rr, rec, tool, agentID := newReactHarness(t, p, []ReACTOption{
		WithReactConfig(ReactConfig{MaxReflectionRounds: -1, MaxToolCallsPerStep: 3}),
	}, WithAgentConfigSource(func(string) json.RawMessage {
		return []byte(`{"react":{"max_tool_calls_per_step":7,"temperature":0.5}}`)
	}))

	run, err := rr.Run(context.Background(), agentID, "echo eight things")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if tool.count() != 7 {
		t.Fatalf("per-agent override must raise the cap to 7, got %d executions", tool.count())
	}
	if len(toolSteps(rec.all())) != 7 {
		t.Fatalf("expected 7 recorded tool steps")
	}
	// The temperature override rides on every ReACT model call.
	for _, req := range p.requests {
		if req.Temperature != 0.5 {
			t.Fatalf("react.temperature override lost: %+v", req.Temperature)
		}
		break
	}
	// Invalid per-agent documents fall back to the runner-wide defaults
	// (bad config must never break a run).
	p2 := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
		if n == 1 {
			return toolReply(
				`{"tool":"echo","arguments":{"text":"1"}}`, `{"tool":"echo","arguments":{"text":"2"}}`,
				`{"tool":"echo","arguments":{"text":"3"}}`, `{"tool":"echo","arguments":{"text":"4"}}`)
		}
		return textReply("done")
	}}
	rr2, _, tool2, agentID2 := newReactHarness(t, p2, []ReACTOption{WithReactConfig(ReactConfig{MaxToolCallsPerStep: 3, MaxReflectionRounds: -1})},
		WithAgentConfigSource(func(string) json.RawMessage { return []byte(`{"react":{"max_tool_calls_per_step":-5}}`) }))
	if _, err := rr2.Run(context.Background(), agentID2, "x"); err != nil {
		t.Fatalf("invalid react document must not break the run: %v", err)
	}
	if tool2.count() != 3 {
		t.Fatalf("invalid override must fall back to the runner-wide cap, got %d", tool2.count())
	}
	_ = run
}

func TestReactTemperatureOverrideReachesProvider(t *testing.T) {
	p := &reactProvider{}
	rr, _, _, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{Temperature: 0.7, MaxReflectionRounds: -1})})
	if _, err := rr.Run(context.Background(), agentID, "hello"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(p.requests) == 0 || p.requests[0].Temperature != 0.7 {
		t.Fatalf("temperature override must reach the provider request, got %+v", p.requests)
	}
}

func TestReactDeterministicAcrossRuns(t *testing.T) {
	// A substantive draft (>= 96 estimated tokens) that covers the tool
	// observation and carries no failure markers scores 1.0 on every
	// heuristic dimension → satisfied at the default 0.8 threshold →
	// StopOnSatisfactory halts after the first draft.
	strongDraft := "The tool returned data-123456 and it directly answers the question that was asked. " +
		"The observation confirms the expected value without further computation or any additional tool calls. " +
		"Every part of the request has now been addressed and verified against the gathered evidence from the echo tool. " +
		"The final conclusion follows from the verified data alone, with no remaining gaps or open questions left. " +
		"No further reflection is needed because the answer is complete, accurate and coherent as written here."
	if EstimateTokens(strongDraft) < heuristicSubstantiveTokens {
		t.Fatalf("test draft must be substantive, tokens=%d", EstimateTokens(strongDraft))
	}
	var outputs []string
	var callCounts []int
	for i := 0; i < 2; i++ {
		p := &reactProvider{mainFn: func(n int, _ models.CompletionRequest) *models.CompletionResponse {
			if n == 1 {
				return toolReply(`{"tool":"echo","arguments":{"text":"data-123456"}}`)
			}
			return textReply(strongDraft)
		}}
		rr, _, _, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{
			StopOnSatisfactory: true,
			QualityThreshold:   0.8,
		})})
		run, err := rr.Run(context.Background(), agentID, "fetch and answer")
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		outputs = append(outputs, run.Output)
		callCounts = append(callCounts, p.totalCalls())
	}
	if outputs[0] != outputs[1] || callCounts[0] != callCounts[1] {
		t.Fatalf("ReACT runs must be deterministic with a fake LLM: %q vs %q, %v vs %v", outputs[0], outputs[1], callCounts[0], callCounts[1])
	}
	if callCounts[0] != 2 { // 1 tool round + 1 satisfactory draft
		t.Fatalf("satisfactory draft must halt reflection after 2 calls, got %d", callCounts[0])
	}
}

func TestReactStepBudgetCoversReflection(t *testing.T) {
	p := &reactProvider{mainFn: func(_ int, _ models.CompletionRequest) *models.CompletionResponse {
		return textReply(weakDraft) // always reflects
	}}
	rr, _, _, agentID := newReactHarness(t, p, []ReACTOption{WithReactConfig(ReactConfig{MaxReflectionRounds: 5})},
		WithLimits(2, 0))

	_, err := rr.Run(context.Background(), agentID, "q")
	if !errors.Is(err, ErrMaxStepsExceeded) {
		t.Fatalf("every model call shares ONE step budget: expected ErrMaxStepsExceeded, got %v", err)
	}
}

func TestReactShutdownGateRefusesNewRuns(t *testing.T) {
	p := &reactProvider{}
	rr, _, _, agentID := newReactHarness(t, p, nil)
	rr.Stop()
	if _, err := rr.Run(context.Background(), agentID, "q"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("ReACT runs must respect the shutdown gate, got %v", err)
	}
}
