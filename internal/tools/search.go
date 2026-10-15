package tools

// search.go — the search tool (issue #111) with three modes:
//
//   - web:       GET against the operator-configured search endpoint
//                (AGENTOS_SEARCH_API_URL / AGENTOS_SEARCH_API_KEY, injected
//                via SearchConfig / NewSearchTool). The request/response
//                contract is documented on SearchTool's DescribeTool and in
//                api/fragments/tools-catalog.yaml:
//
//                  GET {endpoint}?q={urlencoded query}&k={limit}
//                  Authorization: Bearer {api key}   (when configured)
//                  Accept: application/json
//
//                The response is either {"results":[{title,url,snippet,score},…]}
//                or a bare array of the same objects; common field aliases
//                (url→link, snippet→description/content, score→relevance/_score)
//                are normalized into {title,url,snippet,score}. The endpoint
//                is operator infrastructure, so model-controlled SSRF checks
//                do not apply here (the model only ever controls the query
//                string and k).
//   - knowledge: delegates to the knowledge service retrieval seam via the
//                narrow KnowledgeSearcher interface (satisfied structurally by
//                *knowledge.Service; internal/knowledge types are imported
//                read-only).
//   - hybrid:    runs web and knowledge concurrently, merges, de-duplicates
//                by URL (falling back to the knowledge chunk id) and
//                re-ranks by score. Ties keep the deterministic web-before-
//                knowledge original order (stable sort). A failure of ONE
//                side degrades to the other side with a warning in the
//                output; failure of both sides is an error.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentos/internal/knowledge"
	"agentos/internal/policies"
)

// SearchToolName is the registry name of the search tool.
const SearchToolName = "search"

// Search bounds (mirroring the knowledge service's search bounds).
const (
	DefaultSearchK = 5
	MaxSearchK     = 50
	// DefaultSearchMaxResponseBytes caps the web endpoint response body.
	DefaultSearchMaxResponseBytes = 1 << 20
	// maxSearchQueryBytes bounds the query string.
	maxSearchQueryBytes = 1024
)

// Search tool errors.
var (
	// ErrSearchUnavailable is returned when the requested mode has no
	// backend wired (no endpoint configured / no knowledge service).
	ErrSearchUnavailable = errors.New("tools: search backend is not configured")
)

// SearchResult is the normalized result shape every mode returns.
type SearchResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url,omitempty"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
	// Source is "web" or "knowledge".
	Source string `json:"source"`
	// Knowledge-only provenance (empty for web results).
	ID           string `json:"id,omitempty"`
	DocumentID   string `json:"document_id,omitempty"`
	ChunkOrdinal int    `json:"chunk_ordinal,omitempty"`
	Citation     string `json:"citation,omitempty"`
}

// KnowledgeSearcher is the narrow retrieval seam over the knowledge service.
// *knowledge.Service satisfies it structurally (Search(ctx, orgID, query, k)).
type KnowledgeSearcher interface {
	Search(ctx context.Context, orgID, query string, k int) ([]knowledge.SearchResult, error)
}

// KnowledgeSearcherFunc adapts a plain function into a KnowledgeSearcher.
type KnowledgeSearcherFunc func(ctx context.Context, orgID, query string, k int) ([]knowledge.SearchResult, error)

// Search implements KnowledgeSearcher.
func (f KnowledgeSearcherFunc) Search(ctx context.Context, orgID, query string, k int) ([]knowledge.SearchResult, error) {
	if f == nil {
		return nil, ErrSearchUnavailable
	}
	return f(ctx, orgID, query, k)
}

// SearchConfig carries the operator-configured web search endpoint.
type SearchConfig struct {
	// EndpointURL is the absolute search endpoint (GET q/k contract above).
	EndpointURL string
	// APIKey is sent as a Bearer token when non-empty.
	APIKey string
}

// SearchConfigFromEnv reads AGENTOS_SEARCH_API_URL and
// AGENTOS_SEARCH_API_KEY. Unset values leave web mode unavailable (the tool
// fails closed with ErrSearchUnavailable instead of guessing an endpoint).
func SearchConfigFromEnv() SearchConfig {
	return SearchConfig{
		EndpointURL: envOr("AGENTOS_SEARCH_API_URL", ""),
		APIKey:      envOr("AGENTOS_SEARCH_API_KEY", ""),
	}
}

// SearchTool implements the search tool over the configured backends.
type SearchTool struct {
	endpointURL string
	apiKey      string
	client      *http.Client
	knowledge   KnowledgeSearcher

	maxResponseBytes int
	gate             *ToolPolicyGate
}

// NewSearchTool returns the tool with the web endpoint (may be empty) and the
// knowledge searcher (may be nil). At least one backend must be wired for a
// mode to work; unwired modes fail closed with ErrSearchUnavailable.
func NewSearchTool(cfg SearchConfig, knowledge KnowledgeSearcher) *SearchTool {
	return &SearchTool{
		endpointURL:      strings.TrimSpace(cfg.EndpointURL),
		apiKey:           cfg.APIKey,
		client:           &http.Client{Timeout: defaultHTTPToolTimeout},
		knowledge:        knowledge,
		maxResponseBytes: DefaultSearchMaxResponseBytes,
	}
}

// WithHTTPClient overrides the web-mode client (tests inject httptest via URL,
// this is for transports with custom dials).
func (t *SearchTool) WithHTTPClient(c *http.Client) *SearchTool {
	t.client = c
	return t
}

// WithPolicyGate attaches the per-tool policy gate (see policy.go).
func (t *SearchTool) WithPolicyGate(gate *ToolPolicyGate) *SearchTool {
	t.gate = gate
	return t
}

// Name implements Tool.
func (t *SearchTool) Name() string { return SearchToolName }

// DescribeTool publishes catalog metadata (issue #111).
func (t *SearchTool) DescribeTool() ToolInfo {
	return ToolInfo{
		Name: t.Name(),
		Description: "Searches with three modes: web (operator-configured " +
			"endpoint via AGENTOS_SEARCH_API_URL/KEY, results normalized to " +
			"{title,url,snippet,score}), knowledge (the organization's knowledge " +
			"base) and hybrid (both, merged, de-duplicated and re-ranked by " +
			"score; one failing backend degrades with a warning).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Search query (max 1 KiB).",
				},
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{"web", "knowledge", "hybrid"},
					"description": "Backend selection; defaults to web when an endpoint is configured, else knowledge.",
				},
				"k": map[string]any{
					"type":        "integer",
					"description": "Maximum results (1-50; defaults to 5).",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Web-mode deadline in milliseconds (defaults to 10000, capped at 60000).",
				},
				"estimated_cost_cents": map[string]any{
					"type":        "integer",
					"description": "Optional estimated cost surfaced to the policy engine (max_cost_cents condition).",
				},
			},
			"required": []string{"query"},
		},
	}
}

// Execute implements Tool using a background context.
func (t *SearchTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware.
func (t *SearchTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t == nil {
		return nil, errors.New("tools: search tool is not initialized")
	}
	if err := t.gate.Allow(ctx, t.Name(), toolEstimatedCostCents(input["estimated_cost_cents"])); err != nil {
		return nil, err
	}

	query, err := stringArg(input["query"])
	if err != nil {
		return nil, fmt.Errorf("%w: query: %v", ErrInvalidToolInput, err)
	}
	query = strings.TrimSpace(query)
	if len(query) > maxSearchQueryBytes {
		return nil, fmt.Errorf("%w: query exceeds %d bytes", ErrInvalidToolInput, maxSearchQueryBytes)
	}
	k := searchKArg(input["k"])
	mode, err := searchModeArg(input["mode"], t)
	if err != nil {
		return nil, err
	}

	timeout := timeoutArg(input["timeout_ms"], defaultHTTPToolTimeout, maxHTTPToolTimeout)

	var (
		results []SearchResult
		warns   []string
	)
	switch mode {
	case "web":
		results, err = t.searchWeb(ctx, query, k, timeout)
		if err != nil {
			return nil, err
		}
	case "knowledge":
		results, err = t.searchKnowledge(ctx, query, k)
		if err != nil {
			return nil, err
		}
	case "hybrid":
		results, warns, err = t.searchHybrid(ctx, query, k, timeout)
		if err != nil {
			return nil, err
		}
	}

	return map[string]any{
		"mode":     mode,
		"query":    query,
		"results":  results,
		"count":    len(results),
		"warnings": warns,
	}, nil
}

// searchWeb performs the GET against the configured endpoint and normalizes
// the response.
func (t *SearchTool) searchWeb(ctx context.Context, query string, k int, timeout time.Duration) ([]SearchResult, error) {
	if t.endpointURL == "" {
		return nil, fmt.Errorf("%w: web mode requires AGENTOS_SEARCH_API_URL", ErrSearchUnavailable)
	}
	endpoint, err := url.Parse(t.endpointURL)
	if err != nil {
		return nil, fmt.Errorf("%w: search endpoint %q is not a valid URL: %v", ErrSearchUnavailable, t.endpointURL, err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("%w: search endpoint scheme must be http(s)", ErrSearchUnavailable)
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("k", strconv.Itoa(k))
	endpoint.RawQuery = params.Encode()

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build search request: %v", ErrSearchUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}

	client := t.client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search endpoint call failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("search endpoint returned status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(t.maxResponseBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("reading search response failed: %v", err)
	}
	if len(raw) > t.maxResponseBytes {
		return nil, fmt.Errorf("%w: search response exceeds %d bytes", ErrInvalidToolInput, t.maxResponseBytes)
	}
	return parseWebResults(raw, k)
}

// parseWebResults accepts {"results":[…]} or a bare array and normalizes
// every item to SearchResult{title,url,snippet,score}.
func parseWebResults(raw []byte, k int) ([]SearchResult, error) {
	var doc struct {
		Results []map[string]any `json:"results"`
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &doc); err == nil && doc.Results != nil {
		items = doc.Results
	} else if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%w: search response is neither {results:[…]} nor an array", ErrInvalidToolInput)
	}
	out := make([]SearchResult, 0, minInt(len(items), k))
	for _, item := range items {
		if item == nil {
			continue
		}
		out = append(out, SearchResult{
			Title:   webStringField(item, "title", "name"),
			URL:     webStringField(item, "url", "link", "href"),
			Snippet: webStringField(item, "snippet", "description", "content", "summary"),
			Score:   webScoreField(item),
			Source:  "web",
		})
		if len(out) >= k {
			break
		}
	}
	return out, nil
}

func webStringField(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := item[key].(string); ok {
			return v
		}
	}
	return ""
}

func webScoreField(item map[string]any) float64 {
	for _, key := range []string{"score", "relevance", "_score"} {
		switch v := item[key].(type) {
		case float64:
			return v
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return f
			}
		}
	}
	return 0
}

// searchKnowledge delegates to the knowledge service seam and maps the
// knowledge result shape onto SearchResult.
func (t *SearchTool) searchKnowledge(ctx context.Context, query string, k int) ([]SearchResult, error) {
	if t.knowledge == nil {
		return nil, fmt.Errorf("%w: knowledge mode requires a knowledge service", ErrSearchUnavailable)
	}
	orgID := policies.RunScopeFromContext(ctx).OrganizationID
	if orgID == "" {
		return nil, errors.New("tools: organization scope is required for knowledge search (run context)")
	}
	hits, err := t.knowledge.Search(ctx, orgID, query, k)
	if err != nil {
		return nil, fmt.Errorf("knowledge search failed: %v", err)
	}
	out := make([]SearchResult, 0, len(hits))
	for _, hit := range hits {
		out = append(out, SearchResult{
			Title:        hit.DocumentTitle,
			URL:          "",
			Snippet:      hit.Content,
			Score:        hit.Score,
			Source:       "knowledge",
			ID:           hit.ChunkID,
			DocumentID:   hit.DocumentID,
			ChunkOrdinal: hit.ChunkOrdinal,
			Citation:     hit.Citation,
		})
	}
	return out, nil
}

// searchHybrid runs both modes concurrently, merges, deduplicates and
// re-ranks by score (ties keep web-before-knowledge order). One failing side
// degrades with a warning; both failing is an error.
func (t *SearchTool) searchHybrid(ctx context.Context, query string, k int, timeout time.Duration) ([]SearchResult, []string, error) {
	var (
		wg            sync.WaitGroup
		webResults    []SearchResult
		knResults     []SearchResult
		webErr, knErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		webResults, webErr = t.searchWeb(ctx, query, k, timeout)
	}()
	go func() {
		defer wg.Done()
		knResults, knErr = t.searchKnowledge(ctx, query, k)
	}()
	wg.Wait()

	var warns []string
	if webErr != nil {
		warns = append(warns, "web search failed: "+webErr.Error())
	}
	if knErr != nil {
		warns = append(warns, "knowledge search failed: "+knErr.Error())
	}
	if webErr != nil && knErr != nil {
		return nil, warns, fmt.Errorf("hybrid search failed on both backends: web=%v; knowledge=%v", webErr, knErr)
	}

	merged := make([]SearchResult, 0, len(webResults)+len(knResults))
	merged = append(merged, webResults...)
	merged = append(merged, knResults...)
	deduped := make([]SearchResult, 0, len(merged))
	seen := make(map[string]bool, len(merged))
	for _, r := range merged {
		key := r.URL
		if key == "" {
			key = "knowledge:" + r.ID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, r)
	}
	// Stable sort: higher score first, ties keep the deterministic
	// web-then-knowledge insertion order.
	sort.SliceStable(deduped, func(i, j int) bool { return deduped[i].Score > deduped[j].Score })
	if len(deduped) > k {
		deduped = deduped[:k]
	}
	return deduped, warns, nil
}

// searchModeArg validates the "mode" argument. Default: web when an endpoint
// is configured, else knowledge when a knowledge service is wired, else the
// honest "nothing is configured" error.
func searchModeArg(value any, t *SearchTool) (string, error) {
	mode := ""
	switch v := value.(type) {
	case string:
		mode = strings.ToLower(strings.TrimSpace(v))
	case nil:
	default:
		return "", fmt.Errorf("%w: mode must be a string, got %T", ErrInvalidToolInput, value)
	}
	switch mode {
	case "web", "knowledge", "hybrid":
		return mode, nil
	case "":
		if t.endpointURL != "" {
			return "web", nil
		}
		if t.knowledge != nil {
			return "knowledge", nil
		}
		return "", fmt.Errorf("%w: no search backend is configured", ErrSearchUnavailable)
	default:
		return "", fmt.Errorf("%w: mode %q is not allowed (web|knowledge|hybrid)", ErrInvalidToolInput, mode)
	}
}

// searchKArg clamps "k" into [1, MaxSearchK] with DefaultSearchK default.
func searchKArg(value any) int {
	k := DefaultSearchK
	switch v := value.(type) {
	case float64:
		if v >= 1 {
			k = int(v)
		}
	case int:
		if v >= 1 {
			k = v
		}
	case int64:
		if v >= 1 {
			k = int(v)
		}
	}
	if k > MaxSearchK {
		return MaxSearchK
	}
	return k
}
