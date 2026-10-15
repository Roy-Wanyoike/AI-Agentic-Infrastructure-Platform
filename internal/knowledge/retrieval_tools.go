package knowledge

// retrieval_tools.go — the tiered retrieval tools (issue #132): a multi-depth
// search hierarchy exposed as agent tools that register like platform
// built-ins.
//
//   - quick_search    (TierQuick):    fast keyword retrieval, top-K
//     (default 5), low cost. Runs the platform's keyword leg — the lexical
//     token-overlap scorer behind KeywordSearch/KeywordSource (issue #113) —
//     over the tier-filtered candidate window.
//   - panorama_search (TierPanorama): broad retrieval with full per-result
//     context (content + document source/metadata/timestamps); the
//     include_archived flag widens the window to archived/expired documents.
//     Medium cost.
//   - insight_forge   (TierInsight):  auto-decomposes the query into
//     deterministic sub-questions, retrieves per sub-question and
//     synthesizes one coherent insight. Highest cost.
//
// WIRING: the tools satisfy the platform tool contract structurally
// (internal/tools.Tool: Name + Execute; plus ContextAware for deadline/cancel
// propagation). internal/tools imports internal/knowledge, so this package
// cannot name the tools.Tool type — the structural RetrievalTool interface
// below pins the shape and RegisterRetrievalTools does the registration.
// The integrator wires the whole tier in ONE line at the registry build site:
//
//      knowledge.RegisterRetrievalTools(reg,
//              knowledge.NewQuickSearchTool(src),
//              knowledge.NewPanoramaSearchTool(src),
//              knowledge.NewInsightForgeTool(src))
//
// (reg is the *tools.Registry; the generic T resolves to tools.Tool and the
// compiler verifies interface compliance at that call site.)
//
// POLICY: every tool exposes TierName() so policy/tool-allowlist wiring can
// restrict tiers (TierForTool maps registry names back to tiers). No
// internal/policies changes are made or needed: the existing tool_allowlist
// condition matches the registry names, and the runtime seam stays the
// authoritative enforcement point.
//
// ARCHIVE/EXPIRY CONVENTION (no schema change): document visibility is read
// from the existing knowledge_documents.metadata JSONB by
// DefaultTierClassifier — metadata["archived"]=true, metadata["status"]
// "archived"/"expired", or metadata["expires_at"] (RFC3339) in the past.
// Quick/insight tiers search ACTIVE documents only; panorama opts into the
// archived/expired window with include_archived=true.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RetrievalTier names one depth tier of the retrieval hierarchy. Tiers are
// policy-restrictable: allowlist wiring can admit quick searches for every
// agent while reserving panorama/insight for trusted runs.
type RetrievalTier string

// The retrieval tiers, cheapest first.
const (
	// TierQuick is the low-cost keyword tier (top-K, default 5).
	TierQuick RetrievalTier = "quick"
	// TierPanorama is the broad full-context tier (optionally including
	// archived/expired documents).
	TierPanorama RetrievalTier = "panorama"
	// TierInsight is the decompose-retrieve-synthesize tier.
	TierInsight RetrievalTier = "insight"
)

// Registry names of the tiered retrieval tools (the keys they register under
// and the values tool_allowlist policies match against).
const (
	ToolNameQuickSearch    = "quick_search"
	ToolNamePanoramaSearch = "panorama_search"
	ToolNameInsightForge   = "insight_forge"
)

// Tier bounds (per-tool top-K).
const (
	// DefaultPanoramaK is the panorama tier's broader default result count.
	DefaultPanoramaK = 20
	// MaxSubQuestions caps the insight tier's decomposition fan-out (bounds
	// the cost of one insight call).
	MaxSubQuestions = 6
	// DefaultSubQuestionK is the per-sub-question retrieval size of the
	// insight tier.
	DefaultSubQuestionK = 3
	// MaxSynthesisEvidence caps the evidence bullets in the deterministic
	// synthesis summary.
	MaxSynthesisEvidence = 5
	// maxSynthesisEvidenceRunes bounds each evidence bullet's content.
	maxSynthesisEvidenceRunes = 180
	// minClauseLen drops split noise (conjunction stubs, single letters).
	minClauseLen = 3
)

// TierForTool maps a registry tool name to its retrieval tier (used by
// allowlist wiring to restrict by tier: collect the tier's tool name, then
// match the existing tool_allowlist condition). Unknown names report false.
func TierForTool(name string) (RetrievalTier, bool) {
	switch name {
	case ToolNameQuickSearch:
		return TierQuick, true
	case ToolNamePanoramaSearch:
		return TierPanorama, true
	case ToolNameInsightForge:
		return TierInsight, true
	}
	return "", false
}

// RetrievalTool is the structural tool contract of the tiered retrieval
// tools: exactly the shape of the platform tool interface (Name + Execute).
// Declared here because internal/tools imports internal/knowledge (a reverse
// import would cycle); the compile-time proof that the concrete tools
// satisfy internal/tools.Tool lives in retrieval_tools_test.go (package
// knowledge_test).
type RetrievalTool interface {
	Name() string
	Execute(input map[string]any) (map[string]any, error)
}

// Compile-time proof every tier tool satisfies the structural contract.
var (
	_ RetrievalTool = (*QuickSearchTool)(nil)
	_ RetrievalTool = (*PanoramaSearchTool)(nil)
	_ RetrievalTool = (*InsightForgeTool)(nil)
)

// RetrievalTiered is implemented by the tier tools for policy/allowlist
// wiring: it exposes which retrieval depth tier the tool belongs to.
type RetrievalTiered interface {
	TierName() RetrievalTier
}

// ---------------------------------------------------------------------------
// Candidate source
// ---------------------------------------------------------------------------

// TierCandidate is one retrieval candidate enriched with the document fields
// the tiers need for scoping and full context (the ChunkRow join plus the
// document's metadata and creation time).
type TierCandidate struct {
	ChunkRow
	DocumentMetadata  map[string]any
	DocumentCreatedAt time.Time
}

// TierCandidateSource is the org-scoped candidate window of the tier
// pipeline: at most limit chunk candidates for one organization (tenant
// guard), in a deterministic order. It is the tier tools' retrieval backend
// — the same window the classic search path reads, widened with document
// metadata so tiers can scope active vs archived/expired content.
type TierCandidateSource interface {
	TierCandidates(ctx context.Context, orgID string, limit int) ([]TierCandidate, error)
}

// NewStoreTierSource adapts a knowledge Store into the tier candidate
// source: one metadata query (ListDocuments) joined in memory against the
// bounded chunk window (ListChunksWithDocuments) — two org-guarded queries
// per call, no schema change. Chunks whose document fell out of the
// metadata listing (window truncation) keep the title/source from the chunk
// row and empty metadata (classified active — the conservative default).
func NewStoreTierSource(store Store) TierCandidateSource {
	return storeTierSource{store: store}
}

type storeTierSource struct {
	store Store
}

// TierCandidates implements TierCandidateSource over the Store seam.
func (s storeTierSource) TierCandidates(ctx context.Context, orgID string, limit int) ([]TierCandidate, error) {
	if s.store == nil {
		return nil, errors.New("knowledge: tier source store is nil")
	}
	docs, err := s.store.ListDocuments(ctx, orgID)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListChunksWithDocuments(ctx, orgID, limit)
	if err != nil {
		return nil, err
	}
	meta := make(map[string]Document, len(docs))
	for _, d := range docs {
		meta[d.ID] = *d
	}
	out := make([]TierCandidate, 0, len(rows))
	for _, row := range rows {
		c := TierCandidate{ChunkRow: row}
		if doc, ok := meta[row.DocumentID]; ok {
			c.DocumentMetadata = doc.Metadata
			c.DocumentCreatedAt = doc.CreatedAt
		}
		out = append(out, c)
	}
	return out, nil
}

// NewServiceTierSource adapts a knowledge Service (either backing mode) into
// the tier candidate source through its public read surface: ListDocuments
// for the org's document set (metadata included) and GetDocument for each
// document's ordered chunks. Intended for the zero-infrastructure mode and
// tests; the Postgres path should use NewStoreTierSource (two queries
// instead of one per document).
func NewServiceTierSource(svc *Service) TierCandidateSource {
	return serviceTierSource{svc: svc}
}

type serviceTierSource struct {
	svc *Service
}

// TierCandidates implements TierCandidateSource over the Service seam.
func (s serviceTierSource) TierCandidates(ctx context.Context, orgID string, limit int) ([]TierCandidate, error) {
	if s.svc == nil {
		return nil, errors.New("knowledge: tier source service is nil")
	}
	docs, err := s.svc.ListDocuments(ctx, orgID)
	if err != nil {
		return nil, err
	}
	// Deterministic window (oldest first), mirroring the SQL candidate path.
	sort.Slice(docs, func(i, j int) bool {
		if !docs[i].CreatedAt.Equal(docs[j].CreatedAt) {
			return docs[i].CreatedAt.Before(docs[j].CreatedAt)
		}
		return docs[i].ID < docs[j].ID
	})
	out := make([]TierCandidate, 0, len(docs))
	for _, doc := range docs {
		if limit > 0 && len(out) >= limit {
			break
		}
		_, chunks, err := s.svc.GetDocument(ctx, orgID, doc.ID)
		if err != nil {
			if errors.Is(err, ErrDocumentNotFound) {
				continue // raced with deletion; skip, never leak across orgs
			}
			return nil, err
		}
		for _, chunk := range chunks {
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
			out = append(out, TierCandidate{
				ChunkRow:          ChunkRow{Chunk: chunk, DocumentTitle: doc.Title, DocumentSource: doc.Source},
				DocumentMetadata:  doc.Metadata,
				DocumentCreatedAt: doc.CreatedAt,
			})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Tier classification (archive/expiry convention)
// ---------------------------------------------------------------------------

// TierVisibility is the retrieval visibility class of one document.
type TierVisibility string

// Visibility classes.
const (
	// VisibilityActive documents are searchable by every tier.
	VisibilityActive TierVisibility = "active"
	// VisibilityArchived documents are operator-marked archived; only the
	// panorama tier with include_archived=true surfaces them.
	VisibilityArchived TierVisibility = "archived"
	// VisibilityExpired documents passed their metadata expiry; only the
	// panorama tier with include_archived=true surfaces them.
	VisibilityExpired TierVisibility = "expired"
)

// TierClassifier assigns the retrieval visibility of one candidate. It is
// the tier scoping seam: tests and operators can plug a custom classifier
// (the default reads the documented metadata convention).
type TierClassifier interface {
	Classify(candidate TierCandidate, now time.Time) TierVisibility
}

// DefaultTierClassifier implements the archive/expiry metadata convention on
// the EXISTING knowledge_documents.metadata JSONB (no schema change):
//
//   - metadata["archived"] truthy (bool true, or the strings "true"/"1")
//     -> VisibilityArchived;
//   - metadata["status"] "archived" or "expired" (case-insensitive)
//     -> VisibilityArchived / VisibilityExpired;
//   - metadata["expires_at"] parsing as RFC3339 and before now
//     -> VisibilityExpired.
//
// Everything else is VisibilityActive. Missing/unparsable metadata never
// hides a document (fail open for search — the active window is the default
// surface; archiving is an explicit operator act).
type DefaultTierClassifier struct{}

// Classify implements TierClassifier.
func (DefaultTierClassifier) Classify(c TierCandidate, now time.Time) TierVisibility {
	meta := c.DocumentMetadata
	if len(meta) == 0 {
		return VisibilityActive
	}
	if tierMetadataTruthy(meta["archived"]) {
		return VisibilityArchived
	}
	switch strings.ToLower(strings.TrimSpace(tierMetadataString(meta["status"]))) {
	case "archived":
		return VisibilityArchived
	case "expired":
		return VisibilityExpired
	}
	if raw := strings.TrimSpace(tierMetadataString(meta["expires_at"])); raw != "" {
		if exp, err := time.Parse(time.RFC3339, raw); err == nil && now.After(exp) {
			return VisibilityExpired
		}
	}
	return VisibilityActive
}

// tierMetadataTruthy reports whether a metadata value is truthy for the
// archive convention (bool true or the strings "true"/"1", case-insensitive).
func tierMetadataTruthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1":
			return true
		}
	}
	return false
}

// tierMetadataString flattens a metadata scalar to its string form (the
// JSONB round-trip yields string/float64/bool).
func tierMetadataString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// ---------------------------------------------------------------------------
// Metrics (observability seam)
// ---------------------------------------------------------------------------

// RetrievalMetrics is the tier tools' observability seam: one observation
// per executed tool call. Wire a platform-backed implementation for real
// telemetry; the default NoopRetrievalMetrics drops everything (zero cost).
type RetrievalMetrics interface {
	// ObserveTierCall records one tier invocation: the tier, the org scope,
	// the number of results returned to the caller, and the deterministic
	// token-cost estimate of the call (query + returned content, see
	// EstimateTokens). Must not block the search path.
	ObserveTierCall(tier RetrievalTier, orgID string, results int, estimatedTokens int)
}

// NoopRetrievalMetrics is the zero-cost default sink: every observation is
// dropped. The nil-safe shape of the seam.
type NoopRetrievalMetrics struct{}

// ObserveTierCall implements RetrievalMetrics (drops the observation).
func (NoopRetrievalMetrics) ObserveTierCall(RetrievalTier, string, int, int) {}

// TierUsage aggregates the observations of one tier.
type TierUsage struct {
	Calls           int64 `json:"calls"`
	Results         int64 `json:"results"`
	EstimatedTokens int64 `json:"estimated_tokens"`
}

// MemoryRetrievalMetrics is a thread-safe in-process RetrievalMetrics sink
// (counters per tier). It is the diagnostics default for operators wiring
// the tools without a metrics backend, and the test double for the tier
// usage assertions.
type MemoryRetrievalMetrics struct {
	mu    sync.Mutex
	usage map[RetrievalTier]TierUsage
}

// NewMemoryRetrievalMetrics returns an empty memory sink.
func NewMemoryRetrievalMetrics() *MemoryRetrievalMetrics {
	return &MemoryRetrievalMetrics{usage: make(map[RetrievalTier]TierUsage)}
}

// ObserveTierCall implements RetrievalMetrics (additive counters).
func (m *MemoryRetrievalMetrics) ObserveTierCall(tier RetrievalTier, orgID string, results int, estimatedTokens int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.usage[tier]
	u.Calls++
	u.Results += int64(results)
	u.EstimatedTokens += int64(estimatedTokens)
	m.usage[tier] = u
}

// Usage returns a copy of the per-tier counters keyed by tier name.
func (m *MemoryRetrievalMetrics) Usage() map[RetrievalTier]TierUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[RetrievalTier]TierUsage, len(m.usage))
	for tier, u := range m.usage {
		out[tier] = u
	}
	return out
}

// EstimateTokens is the deterministic token-cost estimate shared by the tier
// tools: the platform's ~4-characters-per-token heuristic (one token per
// four characters of text), matching internal/runtime's budgeting estimate.
// It feeds the per-tier estimated_tokens observability counters — it is a
// budgeting signal, NOT billing.
func EstimateTokens(text string) int {
	n := len([]rune(text))
	if n == 0 {
		return 0
	}
	return (n + 3) / 4
}

// ---------------------------------------------------------------------------
// Shared tier pipeline
// ---------------------------------------------------------------------------

// tierToolConfig is the shared wiring of a tier tool: candidate source,
// classifier, metrics sink and clock (the clock is injected for
// deterministic expiry tests).
type tierToolConfig struct {
	source     TierCandidateSource
	classifier TierClassifier
	metrics    RetrievalMetrics
	now        func() time.Time
}

func (c *tierToolConfig) normalize() {
	if c.classifier == nil {
		c.classifier = DefaultTierClassifier{}
	}
	if c.metrics == nil {
		c.metrics = NoopRetrievalMetrics{}
	}
	if c.now == nil {
		c.now = time.Now
	}
}

func (c *tierToolConfig) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

func (c *tierToolConfig) observe(tier RetrievalTier, orgID string, results int, estimatedTokens int) {
	if c.metrics == nil {
		return
	}
	c.metrics.ObserveTierCall(tier, orgID, results, estimatedTokens)
}

// TierHit is one ranked hit of the tier pipeline: the contract search-result
// fields plus the panorama context fields (document source, metadata and
// document creation time).
type TierHit struct {
	DocumentID       string         `json:"document_id"`
	DocumentTitle    string         `json:"document_title"`
	DocumentSource   string         `json:"document_source,omitempty"`
	DocumentMetadata map[string]any `json:"document_metadata,omitempty"`
	ChunkID          string         `json:"chunk_id"`
	ChunkOrdinal     int            `json:"chunk_ordinal"`
	Content          string         `json:"content"`
	// DocumentCreatedAt is the document's creation time (panorama context;
	// zero for candidates whose document metadata was not joined).
	DocumentCreatedAt time.Time `json:"-"`
	Score             float64   `json:"score"`
	Citation          string    `json:"citation"`
}

// tierVisibilitySet reports which visibility classes a tier's search admits.
type tierVisibilitySet map[TierVisibility]bool

// tierSearch runs the shared tier pipeline: fetch the org-scoped candidate
// window, filter by visibility class, score with the keyword leg's lexical
// scorer (tokenSet + lexicalScore — the same text-search path
// Service.KeywordSearch implements for the hybrid keyword leg), drop
// below-floor noise, and rank deterministically (score desc, then document
// title, then chunk ordinal, then chunk id).
func tierSearch(ctx context.Context, cfg *tierToolConfig, orgID, query string, k int, admit tierVisibilitySet) ([]TierHit, error) {
	if cfg == nil || cfg.source == nil {
		return nil, errors.New("knowledge: tier candidate source is required")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("query is required")
	}
	if k <= 0 {
		k = DefaultSearchK
	}
	if k > MaxSearchK {
		k = MaxSearchK
	}
	candidates, err := cfg.source.TierCandidates(ctx, orgID, searchCandidateLimit)
	if err != nil {
		return nil, err
	}
	classifier := cfg.classifier
	if classifier == nil {
		classifier = DefaultTierClassifier{}
	}
	now := cfg.clock()
	queryTokens := tokenSet(query)
	type scoredHit struct {
		candidate TierCandidate
		score     float64
	}
	scored := make([]scoredHit, 0, len(candidates))
	for _, c := range candidates {
		if !admit[classifier.Classify(c, now)] {
			continue
		}
		score := lexicalScore(queryTokens, c.Content)
		if score <= scoreFloor {
			continue // same hash-collision floor as the classic search path
		}
		scored = append(scored, scoredHit{candidate: c, score: score})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].candidate.DocumentTitle != scored[j].candidate.DocumentTitle {
			return scored[i].candidate.DocumentTitle < scored[j].candidate.DocumentTitle
		}
		if scored[i].candidate.Ordinal != scored[j].candidate.Ordinal {
			return scored[i].candidate.Ordinal < scored[j].candidate.Ordinal
		}
		return scored[i].candidate.ID < scored[j].candidate.ID
	})
	if len(scored) > k {
		scored = scored[:k]
	}
	hits := make([]TierHit, 0, len(scored))
	for _, s := range scored {
		hits = append(hits, TierHit{
			DocumentID:        s.candidate.DocumentID,
			DocumentTitle:     s.candidate.DocumentTitle,
			DocumentSource:    s.candidate.DocumentSource,
			DocumentMetadata:  s.candidate.DocumentMetadata,
			ChunkID:           s.candidate.ID,
			ChunkOrdinal:      s.candidate.Ordinal,
			Content:           s.candidate.Content,
			DocumentCreatedAt: s.candidate.DocumentCreatedAt,
			Score:             s.score,
			Citation:          buildCitation(s.candidate.DocumentTitle, s.candidate.DocumentSource, s.candidate.Ordinal),
		})
	}
	return hits, nil
}

// tierResultsJSON flattens TierHits into the tool-output item shape
// (fullContext adds the panorama context fields to every item).
func tierResultsJSON(hits []TierHit, fullContext bool) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		item := map[string]any{
			"document_id":    h.DocumentID,
			"document_title": h.DocumentTitle,
			"chunk_id":       h.ChunkID,
			"chunk_ordinal":  h.ChunkOrdinal,
			"content":        h.Content,
			"score":          h.Score,
			"citation":       h.Citation,
		}
		if fullContext {
			item["document_source"] = h.DocumentSource
			item["document_metadata"] = h.DocumentMetadata
			if !h.DocumentCreatedAt.IsZero() {
				item["created_at"] = h.DocumentCreatedAt.UTC().Format(time.RFC3339)
			}
		}
		out = append(out, item)
	}
	return out
}

// tierEstimatedTokens is the deterministic per-call token estimate: the
// query plus every returned chunk's content (synthesis text is added by the
// insight tier).
func tierEstimatedTokens(query string, hits []TierHit) int {
	total := EstimateTokens(query)
	for _, h := range hits {
		total += EstimateTokens(h.Content)
	}
	return total
}

// ---------------------------------------------------------------------------
// Tool argument helpers (local to the tier tools)
// ---------------------------------------------------------------------------

// tierStringArg reads a required string argument.
func tierStringArg(input map[string]any, key string) (string, error) {
	switch v := input[key].(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("%s is required", key)
		}
		return v, nil
	case nil:
		return "", fmt.Errorf("%s is required", key)
	default:
		return "", fmt.Errorf("%s must be a string, got %T", key, input[key])
	}
}

// tierIntArg reads an optional positive integer argument (JSON number or
// numeric int) with a default; zero/negative/garbage values resolve to the
// default.
func tierIntArg(input map[string]any, key string, def int) int {
	switch v := input[key].(type) {
	case float64:
		if v >= 1 {
			return int(v)
		}
	case int:
		if v >= 1 {
			return v
		}
	case int64:
		if v >= 1 {
			return int(v)
		}
	}
	return def
}

// tierBoolArg reads an optional boolean argument (default false).
func tierBoolArg(input map[string]any, key string) bool {
	b, _ := input[key].(bool)
	return b
}

// ---------------------------------------------------------------------------
// QuickSearchTool — the quick tier
// ---------------------------------------------------------------------------

// QuickSearchTool is the QUICK tier: fast keyword retrieval, top-K (default
// 5), low cost. It runs the platform's keyword leg — the lexical
// token-overlap scorer behind KeywordSearch/KeywordSource (issue #113) —
// over the tier-filtered candidate window: only ACTIVE documents
// (archived/expired content is excluded; that is the panorama tier's
// opt-in).
type QuickSearchTool struct {
	cfg tierToolConfig
}

// NewQuickSearchTool returns the quick tier tool over the given candidate
// source. A nil source disables the tool (every call fails closed).
func NewQuickSearchTool(source TierCandidateSource) *QuickSearchTool {
	t := &QuickSearchTool{cfg: tierToolConfig{source: source}}
	t.cfg.normalize()
	return t
}

// WithMetrics attaches an observability sink (nil keeps the no-op default).
func (t *QuickSearchTool) WithMetrics(m RetrievalMetrics) *QuickSearchTool {
	if m != nil {
		t.cfg.metrics = m
	}
	return t
}

// WithClassifier overrides the visibility classifier (nil keeps the default).
func (t *QuickSearchTool) WithClassifier(c TierClassifier) *QuickSearchTool {
	if c != nil {
		t.cfg.classifier = c
	}
	return t
}

// WithClock overrides the wall clock (deterministic expiry tests).
func (t *QuickSearchTool) WithClock(now func() time.Time) *QuickSearchTool {
	if now != nil {
		t.cfg.now = now
	}
	return t
}

// Name implements Tool.
func (t *QuickSearchTool) Name() string { return ToolNameQuickSearch }

// TierName implements RetrievalTiered.
func (t *QuickSearchTool) TierName() RetrievalTier { return TierQuick }

// Execute implements Tool (background context; see ExecuteContext).
func (t *QuickSearchTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware: the keyword leg over the active
// candidate window, top-K (argument "k", default 5, capped at 50).
func (t *QuickSearchTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	orgID, err := tierStringArg(input, "org_id")
	if err != nil {
		return nil, err
	}
	query, err := tierStringArg(input, "query")
	if err != nil {
		return nil, err
	}
	k := tierIntArg(input, "k", DefaultSearchK)
	hits, err := tierSearch(ctx, &t.cfg, orgID, query, k, tierVisibilitySet{VisibilityActive: true})
	if err != nil {
		return nil, err
	}
	tokens := tierEstimatedTokens(query, hits)
	t.cfg.observe(TierQuick, orgID, len(hits), tokens)
	return map[string]any{
		"tier":             string(TierQuick),
		"org_id":           orgID,
		"query":            query,
		"k":                k,
		"results":          tierResultsJSON(hits, false),
		"count":            len(hits),
		"estimated_tokens": tokens,
	}, nil
}

// ---------------------------------------------------------------------------
// PanoramaSearchTool — the panorama tier
// ---------------------------------------------------------------------------

// PanoramaSearchTool is the PANORAMA tier: broad retrieval with FULL context
// per result (content + document source, metadata and creation time). The
// include_archived flag widens the candidate window to archived/expired
// documents (the documented metadata convention); the default window is
// active-only. Medium cost.
type PanoramaSearchTool struct {
	cfg tierToolConfig
}

// NewPanoramaSearchTool returns the panorama tier tool over the given
// candidate source. A nil source disables the tool (every call fails closed).
func NewPanoramaSearchTool(source TierCandidateSource) *PanoramaSearchTool {
	t := &PanoramaSearchTool{cfg: tierToolConfig{source: source}}
	t.cfg.normalize()
	return t
}

// WithMetrics attaches an observability sink (nil keeps the no-op default).
func (t *PanoramaSearchTool) WithMetrics(m RetrievalMetrics) *PanoramaSearchTool {
	if m != nil {
		t.cfg.metrics = m
	}
	return t
}

// WithClassifier overrides the visibility classifier (nil keeps the default).
func (t *PanoramaSearchTool) WithClassifier(c TierClassifier) *PanoramaSearchTool {
	if c != nil {
		t.cfg.classifier = c
	}
	return t
}

// WithClock overrides the wall clock (deterministic expiry tests).
func (t *PanoramaSearchTool) WithClock(now func() time.Time) *PanoramaSearchTool {
	if now != nil {
		t.cfg.now = now
	}
	return t
}

// Name implements Tool.
func (t *PanoramaSearchTool) Name() string { return ToolNamePanoramaSearch }

// TierName implements RetrievalTiered.
func (t *PanoramaSearchTool) TierName() RetrievalTier { return TierPanorama }

// Execute implements Tool (background context; see ExecuteContext).
func (t *PanoramaSearchTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware: the keyword leg over the (widened)
// candidate window with full per-result context. Arguments: org_id, query,
// k (default 20, capped at 50), include_archived (bool, default false).
func (t *PanoramaSearchTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	orgID, err := tierStringArg(input, "org_id")
	if err != nil {
		return nil, err
	}
	query, err := tierStringArg(input, "query")
	if err != nil {
		return nil, err
	}
	k := tierIntArg(input, "k", DefaultPanoramaK)
	includeArchived := tierBoolArg(input, "include_archived")
	admit := tierVisibilitySet{VisibilityActive: true}
	if includeArchived {
		admit[VisibilityArchived] = true
		admit[VisibilityExpired] = true
	}
	hits, err := tierSearch(ctx, &t.cfg, orgID, query, k, admit)
	if err != nil {
		return nil, err
	}
	tokens := tierEstimatedTokens(query, hits)
	t.cfg.observe(TierPanorama, orgID, len(hits), tokens)
	return map[string]any{
		"tier":             string(TierPanorama),
		"org_id":           orgID,
		"query":            query,
		"k":                k,
		"include_archived": includeArchived,
		"results":          tierResultsJSON(hits, true),
		"count":            len(hits),
		"estimated_tokens": tokens,
	}, nil
}

// ---------------------------------------------------------------------------
// InsightForgeTool — the insight tier
// ---------------------------------------------------------------------------

// Decomposer splits a query into sub-questions for the insight tier.
// Implementations MUST be deterministic: the same query always yields the
// same sub-questions in the same order (the tier's retrieval and synthesis
// inherit that determinism).
type Decomposer interface {
	Decompose(query string) []string
}

// RuleBasedDecomposer is the default deterministic decomposer. Documented
// rules, applied in order:
//
//  1. normalize whitespace (collapse runs, trim);
//  2. split into clauses on sentence boundaries (. ! ?), semicolons,
//     colons and newlines;
//  3. split each clause further on standalone coordinating conjunctions —
//     the words "and", "or", "but" at word boundaries — and on commas
//     immediately followed by a question word (what/which/who/whom/whose/
//     when/where/why/how), which marks a trailing interrogative clause;
//  4. strip leading discourse connectors (also, plus, moreover,
//     furthermore, additionally, and, but, or) and punctuation from each
//     clause, then drop fragments shorter than 3 characters or without any
//     letter/digit, drop case-insensitive duplicates (first occurrence
//     wins), and cap the result at MaxSubQuestions;
//  5. if nothing survives, the original query is returned as the single
//     sub-question (the tier still runs, just without fan-out).
type RuleBasedDecomposer struct{}

// Decompose implements Decomposer (pure function of the input).
func (RuleBasedDecomposer) Decompose(query string) []string {
	query = strings.Join(strings.Fields(query), " ")
	if query == "" {
		return nil
	}
	raw := tierSplitClauses(query)
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, clause := range raw {
		clause = tierStripConnectors(strings.TrimSpace(clause))
		if len([]rune(clause)) < minClauseLen || !strings.ContainsFunc(clause, func(r rune) bool {
			return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		}) {
			continue
		}
		key := strings.ToLower(clause)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, clause)
		if len(out) >= MaxSubQuestions {
			return out
		}
	}
	if len(out) == 0 {
		return []string{query}
	}
	return out
}

// tierStripConnectors removes leading discourse connectors and punctuation
// from a clause (rule 4), e.g. "Also, where …" -> "where …".
func tierStripConnectors(clause string) string {
	clause = strings.TrimLeft(clause, " ,;:.")
	for {
		words := strings.SplitN(clause, " ", 2)
		first := strings.ToLower(strings.Trim(words[0], " ,;:."))
		if _, hit := tierConnectors[first]; !hit {
			return clause
		}
		if len(words) == 1 {
			return ""
		}
		clause = strings.TrimLeft(words[1], " ,;:.")
	}
}

// tierQuestionWords are the interrogative openers that trigger a comma
// split (rule 3b).
var tierQuestionWords = map[string]struct{}{
	"what": {}, "which": {}, "who": {}, "whom": {}, "whose": {},
	"when": {}, "where": {}, "why": {}, "how": {},
}

// tierCoordinators are the coordinating conjunctions that trigger a split
// (rule 3a).
var tierCoordinators = map[string]struct{}{
	"and": {}, "or": {}, "but": {},
}

// tierConnectors are the discourse connectors stripped from clause starts
// (rule 4).
var tierConnectors = map[string]struct{}{
	"also": {}, "plus": {}, "moreover": {}, "furthermore": {},
	"additionally": {}, "and": {}, "but": {}, "or": {},
}

// tierSplitClauses implements the splitting rules (2 + 3) of
// RuleBasedDecomposer over the whitespace-normalized query.
func tierSplitClauses(query string) []string {
	clauses := tierSplitAny(query, '.', '?', '!', ';', ':', '\n')
	// Rule 3a: standalone coordinating conjunctions at word boundaries.
	withCoord := make([]string, 0, len(clauses))
	for _, clause := range clauses {
		withCoord = append(withCoord, tierSplitWords(clause, tierCoordinators)...)
	}
	// Rule 3b: commas followed by a question word.
	withQ := make([]string, 0, len(withCoord))
	for _, clause := range withCoord {
		withQ = append(withQ, tierSplitCommaQuestion(clause)...)
	}
	return withQ
}

// tierSplitAny splits s on every rune in breaks.
func tierSplitAny(s string, breaks ...rune) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		for _, b := range breaks {
			if r == b {
				return true
			}
		}
		return false
	})
}

// tierSplitWords splits s on standalone words listed in triggers
// (word-boundary match, case-insensitive).
func tierSplitWords(s string, triggers map[string]struct{}) []string {
	words := strings.Split(s, " ")
	out := make([]string, 0, len(words))
	current := make([]string, 0, len(words))
	flush := func() {
		if len(current) > 0 {
			out = append(out, strings.Join(current, " "))
			current = current[:0]
		}
	}
	for _, w := range words {
		if _, hit := triggers[strings.ToLower(strings.Trim(w, ",;:"))]; hit {
			flush()
			continue
		}
		current = append(current, w)
	}
	flush()
	return out
}

// tierSplitCommaQuestion splits s on commas immediately followed by a
// question word ("…, how does X work").
func tierSplitCommaQuestion(s string) []string {
	words := strings.Split(s, " ")
	out := make([]string, 0, len(words))
	current := make([]string, 0, len(words))
	flush := func() {
		if len(current) > 0 {
			out = append(out, strings.Join(current, " "))
			current = current[:0]
		}
	}
	for i, w := range words {
		if i > 0 && strings.HasSuffix(words[i-1], ",") {
			if _, hit := tierQuestionWords[strings.ToLower(strings.Trim(w, ",;:"))]; hit {
				// Drop the trailing comma of the previous clause.
				if len(current) > 0 {
					current[len(current)-1] = strings.TrimRight(current[len(current)-1], ",")
				}
				flush()
			}
		}
		current = append(current, w)
	}
	flush()
	return out
}

// Synthesizer composes the per-sub-question findings of the insight tier
// into one coherent insight text. Optional seam: the deterministic default
// (DeterministicSynthesizer) is used when nil.
type Synthesizer interface {
	Synthesize(query string, subQuestions []string, findings map[string][]TierHit) string
}

// DeterministicSynthesizer is the default insight composition: dedup + rank
// + compose, all deterministic.
//
//   - Dedup: findings are collapsed by chunk id across sub-questions (the
//     first sub-question that surfaced a chunk wins — earlier sub-questions
//     come first in the deterministic iteration order).
//   - Rank: score descending with the tier pipeline's tie-breaks (document
//     title, then chunk ordinal, then chunk id).
//   - Compose: a header line naming the query and the evidence count, then
//     at most MaxSynthesisEvidence evidence bullets, each the hit's citation
//     plus its content truncated to maxSynthesisEvidenceRunes runes (a
//     Unicode ellipsis marks truncation).
type DeterministicSynthesizer struct{}

// Synthesize implements Synthesizer (pure function of its inputs).
func (DeterministicSynthesizer) Synthesize(query string, subQuestions []string, findings map[string][]TierHit) string {
	// Dedup by chunk id in deterministic sub-question order.
	ordered := make([]TierHit, 0, 8)
	seen := make(map[string]struct{}, 8)
	for _, sub := range subQuestions {
		for _, hit := range findings[sub] {
			if _, dup := seen[hit.ChunkID]; dup {
				continue
			}
			seen[hit.ChunkID] = struct{}{}
			ordered = append(ordered, hit)
		}
	}
	// Rank: score desc, then title, ordinal, id (same tie-breaks as the
	// tier pipeline).
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Score != ordered[j].Score {
			return ordered[i].Score > ordered[j].Score
		}
		if ordered[i].DocumentTitle != ordered[j].DocumentTitle {
			return ordered[i].DocumentTitle < ordered[j].DocumentTitle
		}
		if ordered[i].ChunkOrdinal != ordered[j].ChunkOrdinal {
			return ordered[i].ChunkOrdinal < ordered[j].ChunkOrdinal
		}
		return ordered[i].ChunkID < ordered[j].ChunkID
	})
	var b strings.Builder
	fmt.Fprintf(&b, "Insight for %q from %d distinct evidence chunk(s):", query, len(ordered))
	for i, hit := range ordered {
		if i >= MaxSynthesisEvidence {
			fmt.Fprintf(&b, "\n… and %d more (see findings).", len(ordered)-i)
			break
		}
		fmt.Fprintf(&b, "\n%d. %s — %s", i+1, hit.Citation, tierTruncate(hit.Content, maxSynthesisEvidenceRunes))
	}
	return b.String()
}

// tierTruncate truncates s to at most limit runes, appending a Unicode
// ellipsis when truncation happened.
func tierTruncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit <= 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

// InsightForgeTool is the INSIGHT tier: it auto-decomposes the query into
// sub-questions (deterministic RuleBasedDecomposer by default), retrieves
// per sub-question over the active candidate window, and synthesizes the
// findings into one coherent insight (deterministic
// DeterministicSynthesizer by default; a Synthesizer LLM seam can be wired
// with WithSynthesizer). Highest cost of the three tiers.
type InsightForgeTool struct {
	cfg         tierToolConfig
	decomposer  Decomposer
	synthesizer Synthesizer
	subK        int
}

// NewInsightForgeTool returns the insight tier tool over the given candidate
// source with the default deterministic decomposer and synthesizer. A nil
// source disables the tool (every call fails closed).
func NewInsightForgeTool(source TierCandidateSource) *InsightForgeTool {
	t := &InsightForgeTool{
		cfg:        tierToolConfig{source: source},
		decomposer: RuleBasedDecomposer{},
		subK:       DefaultSubQuestionK,
	}
	t.cfg.normalize()
	return t
}

// WithMetrics attaches an observability sink (nil keeps the no-op default).
func (t *InsightForgeTool) WithMetrics(m RetrievalMetrics) *InsightForgeTool {
	if m != nil {
		t.cfg.metrics = m
	}
	return t
}

// WithClassifier overrides the visibility classifier (nil keeps the default).
func (t *InsightForgeTool) WithClassifier(c TierClassifier) *InsightForgeTool {
	if c != nil {
		t.cfg.classifier = c
	}
	return t
}

// WithClock overrides the wall clock (deterministic expiry tests).
func (t *InsightForgeTool) WithClock(now func() time.Time) *InsightForgeTool {
	if now != nil {
		t.cfg.now = now
	}
	return t
}

// WithDecomposer overrides the query decomposer (nil keeps the
// deterministic RuleBasedDecomposer).
func (t *InsightForgeTool) WithDecomposer(d Decomposer) *InsightForgeTool {
	if d != nil {
		t.decomposer = d
	}
	return t
}

// WithSynthesizer wires the optional Synthesizer LLM seam (nil keeps the
// deterministic DeterministicSynthesizer — the default has no model
// dependency and no network cost).
func (t *InsightForgeTool) WithSynthesizer(s Synthesizer) *InsightForgeTool {
	if s != nil {
		t.synthesizer = s
	}
	return t
}

// Name implements Tool.
func (t *InsightForgeTool) Name() string { return ToolNameInsightForge }

// TierName implements RetrievalTiered.
func (t *InsightForgeTool) TierName() RetrievalTier { return TierInsight }

// Execute implements Tool (background context; see ExecuteContext).
func (t *InsightForgeTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware: decompose -> per-sub-question
// retrieval (top DefaultSubQuestionK each, active window) -> synthesis.
// Arguments: org_id, query.
func (t *InsightForgeTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	orgID, err := tierStringArg(input, "org_id")
	if err != nil {
		return nil, err
	}
	query, err := tierStringArg(input, "query")
	if err != nil {
		return nil, err
	}
	decomposer := t.decomposer
	if decomposer == nil {
		decomposer = RuleBasedDecomposer{}
	}
	subQuestions := decomposer.Decompose(query)
	if len(subQuestions) == 0 {
		subQuestions = []string{query}
	}

	findings := make(map[string][]TierHit, len(subQuestions))
	tokens := EstimateTokens(query)
	distinct := make(map[string]struct{}, 8)
	for _, sub := range subQuestions {
		hits, err := tierSearch(ctx, &t.cfg, orgID, sub, t.subK, tierVisibilitySet{VisibilityActive: true})
		if err != nil {
			return nil, err
		}
		findings[sub] = hits
		tokens += tierEstimatedTokens(sub, hits)
		for _, h := range hits {
			distinct[h.ChunkID] = struct{}{}
		}
	}

	synthesizer := t.synthesizer
	if synthesizer == nil {
		synthesizer = DeterministicSynthesizer{}
	}
	synthesis := synthesizer.Synthesize(query, subQuestions, findings)
	tokens += EstimateTokens(synthesis)

	t.cfg.observe(TierInsight, orgID, len(distinct), tokens)
	return map[string]any{
		"tier":             string(TierInsight),
		"org_id":           orgID,
		"query":            query,
		"sub_questions":    subQuestions,
		"findings":         tierFindingsJSON(findings),
		"synthesis":        synthesis,
		"sub_count":        len(subQuestions),
		"finding_count":    len(distinct),
		"estimated_tokens": tokens,
	}, nil
}

// tierFindingsJSON flattens the per-sub-question findings into the
// tool-output shape (deterministic sub-question order via the slice, map
// keyed by sub-question for direct lookup).
func tierFindingsJSON(findings map[string][]TierHit) map[string][]map[string]any {
	out := make(map[string][]map[string]any, len(findings))
	for sub, hits := range findings {
		out[sub] = tierResultsJSON(hits, false)
	}
	return out
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// RegisterRetrievalTools registers the tiered retrieval tools into reg —
// any registry exposing Register(T); the platform *tools.Registry satisfies
// it with T = tools.Tool (type inference resolves T from the registry).
//
// THE one integrator wiring line (at the registry build site, next to the
// built-in catalog; internal/tools' own catalog is intentionally NOT
// modified by this package):
//
//	knowledge.RegisterRetrievalTools(reg,
//	        knowledge.NewQuickSearchTool(src),
//	        knowledge.NewPanoramaSearchTool(src),
//	        knowledge.NewInsightForgeTool(src))
//
// Each concrete tool is passed explicitly so the compiler verifies tool
// interface compliance at the wiring site. Registration is additive and
// idempotent per tool name (the registry overwrites same-named entries,
// exactly like Registry.Register). Returns the registered names in tier
// order (quick, panorama, insight) so callers can assert the wiring. A nil
// registry is a no-op.
func RegisterRetrievalTools[T any](reg interface{ Register(T) }, ts ...T) []string {
	if reg == nil {
		return nil
	}
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		reg.Register(t)
		if named, ok := any(t).(interface{ Name() string }); ok {
			names = append(names, named.Name())
		}
	}
	return names
}
