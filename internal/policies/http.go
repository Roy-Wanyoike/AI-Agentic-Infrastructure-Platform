package policies

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPEvaluator resolves decisions against a remote AgentOS API's
// POST /v1/policies/evaluate endpoint. It exists for processes that hold no
// durable policy state of their own — the pull-mode worker (AGENTOS_API_PULL)
// authenticates with its org API key and evaluates every tool invocation
// against the API's policies, so tool-level enforcement holds in the
// two-process deployment too. The tenant is resolved server-side from the
// credential claims (the evaluate endpoint ignores client org ids), so the
// orgID argument is carried for logging/traceability only.
type HTTPEvaluator struct {
	// BaseURL is the API root, e.g. "http://api:8080" (no /v1 suffix).
	BaseURL string
	// APIKey is the X-API-Key credential used to authenticate.
	APIKey string
	// Client is the HTTP client; nil selects one with a 3s timeout so a
	// slow policy API can never dominate a tool call.
	Client *http.Client
}

// NewHTTPEvaluator builds an evaluator for the given API root and key.
func NewHTTPEvaluator(baseURL, apiKey string) *HTTPEvaluator {
	return &HTTPEvaluator{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 3 * time.Second},
	}
}

// EvaluateCtx implements Evaluator over the remote evaluate endpoint.
func (h *HTTPEvaluator) EvaluateCtx(ctx context.Context, orgID string, req EvaluateRequest) (Decision, error) {
	if h == nil || strings.TrimSpace(h.BaseURL) == "" {
		return Decision{}, fmt.Errorf("policies: http evaluator base url is required")
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Decision{}, fmt.Errorf("policies: encode evaluate request: %w", err)
	}
	url := h.BaseURL + "/v1/policies/evaluate"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Decision{}, fmt.Errorf("policies: build evaluate request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if h.APIKey != "" {
		httpReq.Header.Set("X-API-Key", h.APIKey)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Decision{}, fmt.Errorf("policies: evaluate endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Decision{}, fmt.Errorf("policies: read evaluate response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Decision{}, fmt.Errorf("policies: evaluate endpoint returned %s", resp.Status)
	}
	var decision Decision
	if err := json.Unmarshal(payload, &decision); err != nil {
		return Decision{}, fmt.Errorf("policies: decode evaluate response: %w", err)
	}
	return decision, nil
}
