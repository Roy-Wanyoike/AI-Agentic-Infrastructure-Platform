package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DefaultOpenAIBaseURL is used when NewOpenAIProvider receives an empty
// baseURL. Because the base URL is always caller-supplied, the provider works
// with any OpenAI-compatible endpoint: OpenAI, OpenRouter, Groq, Together,
// local vLLM/Ollama, etc. Nothing is read from the environment.
const DefaultOpenAIBaseURL = "https://api.openai.com/v1"

const (
	// DefaultOpenAITimeout bounds a single HTTP attempt when the caller does
	// not supply an http.Client.
	DefaultOpenAITimeout = 30 * time.Second
	// maxOpenAIResponseBytes bounds how much of a response body is read.
	maxOpenAIResponseBytes = 8 << 20
	// errSnippetLen caps provider error bodies embedded in typed errors.
	errSnippetLen = 256
)

// OpenAIProvider implements Provider against any OpenAI-compatible
// /chat/completions endpoint.
type OpenAIProvider struct {
	name       string
	apiKey     string
	baseURL    string
	model      string
	client     *http.Client
	resilience *Resilience
}

// NewOpenAIProvider builds an OpenAI-compatible provider. The caller passes
// all configuration explicitly (no env reads):
//
//   - name:     provider instance name (defaults to "openai")
//   - apiKey:   bearer token ("Authorization: Bearer <apiKey>"); may be empty
//     for unauthenticated local endpoints (Ollama, vLLM)
//   - baseURL:  API root, e.g. "https://api.openai.com/v1",
//     "https://openrouter.ai/api/v1", "http://localhost:11434/v1";
//     trailing slashes are normalized; empty defaults to OpenAI
//   - model:    default model id (requests may override via req.Model)
//   - httpClient: optional client; timeouts should be set on it — when nil,
//     a client with DefaultOpenAITimeout is used.
func NewOpenAIProvider(name, apiKey, baseURL, model string, httpClient *http.Client) *OpenAIProvider {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		trimmed = DefaultOpenAIBaseURL
	}
	if strings.TrimSpace(name) == "" {
		name = "openai"
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultOpenAITimeout}
	}
	return &OpenAIProvider{
		name:       strings.TrimSpace(name),
		apiKey:     strings.TrimSpace(apiKey),
		baseURL:    trimmed,
		model:      strings.TrimSpace(model),
		client:     httpClient,
		resilience: DefaultResilience(),
	}
}

// SetResilience replaces the response resilience layer (issue #128); nil
// disables the layer entirely and restores the legacy single-attempt wire
// behavior exactly. The zero ResilienceConfig inside a NewResilience also
// disables every individual concern. Not safe to call concurrently with
// Complete.
func (p *OpenAIProvider) SetResilience(r *Resilience) { p.resilience = r }

// Resilience returns the active resilience layer (nil when disabled).
func (p *OpenAIProvider) Resilience() *Resilience { return p.resilience }

// Name implements Provider.
func (p *OpenAIProvider) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

// BaseURL returns the normalized endpoint root (useful for diagnostics).
func (p *OpenAIProvider) BaseURL() string { return p.baseURL }

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

type openAIChatRequest struct {
	Model               string                `json:"model"`
	Messages            []openAIChatMessage   `json:"messages"`
	MaxTokens           int                   `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                   `json:"max_completion_tokens,omitempty"`
	Temperature         float64               `json:"temperature,omitempty"`
	ResponseFormat      *openAIResponseFormat `json:"response_format,omitempty"`
}

// openAIResponseFormat is the response_format parameter used by the JSON
// negotiation (issue #128): {"type":"json_object"}.
type openAIResponseFormat struct {
	Type string `json:"type"`
}

type openAIChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Complete implements Provider by POSTing {baseURL}/chat/completions. The
// resilience layer (issue #128) weaves into the request/response path when
// enabled (the constructor installs DefaultResilience; SetResilience(nil)
// restores the legacy single-attempt behavior exactly):
//
//   - request: max_tokens is emitted as max_completion_tokens when the
//     model's wire capabilities say so, and response_format=json_object is
//     negotiated for WantJSON requests on models that support it — with a
//     one-shot prompt-only fallback when the provider rejects it
//     (MetaJSONFallback metadata);
//   - transport: transient failures (429/5xx/network) are retried with
//     exponential backoff + full jitter, honoring Retry-After (Retry config);
//   - response: reasoning wrapper blocks are stripped into Reasoning
//     (MetaReasoningStripped), near-JSON payloads are repaired
//     (MetaJSONRepaired), and an unrepairable/truncated payload triggers ONE
//     token-limit retry with a reduced prompt (MetaTruncationRetry).
func (p *OpenAIProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	if p == nil {
		return nil, errors.New("models: provider is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	model := p.model
	if strings.TrimSpace(req.Model) != "" {
		model = strings.TrimSpace(req.Model)
	}
	if model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}

	res := p.resilience
	resOn := res != nil && res.cfg.Enabled
	var caps WireCapabilities
	if resOn {
		caps = res.Detect(model)
	}

	// JSON negotiation state: jsonSent tracks whether the wire payload wants
	// response_format; jsonFallback records the prompt-only downgrade after
	// a provider rejection (never un-set once tripped, so the fallback is a
	// one-shot retry).
	jsonSent := resOn && res.cfg.NegotiateJSON && req.WantJSON && caps.SupportsJSONResponse
	jsonFallback := false
	truncationRetried := false

	messages, err := buildOpenAIMessages(req)
	if err != nil {
		return nil, err
	}

	for {
		payload, err := encodeChatPayload(model, messages, req, resOn && caps.UsesMaxCompletionTokens, jsonSent && !jsonFallback)
		if err != nil {
			return nil, err
		}

		wire, err := p.attemptWithBackoff(ctx, payload, res)
		if err != nil {
			// Negotiation fallback (issue #128): the provider rejected the
			// request that carried response_format (400/422 class). Retry ONCE
			// prompt-only; a rejection of the retry surfaces normally. The
			// fallback is recorded on the eventual response's metadata.
			if jsonSent && !jsonFallback && errors.Is(err, ErrInvalidRequest) {
				jsonFallback = true
				continue
			}
			return nil, err
		}

		resp := &CompletionResponse{
			Text:  wire.Choices[0].Message.Content,
			Model: wire.Model,
			Usage: Usage{
				PromptTokens:     wire.Usage.PromptTokens,
				CompletionTokens: wire.Usage.CompletionTokens,
				TotalTokens:      wire.Usage.TotalTokens,
			},
		}
		if resp.Usage.TotalTokens == 0 {
			resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		}

		if !resOn {
			return resp, nil
		}

		// Token-limit retry trigger #1 (issue #128): the provider cut
		// the response off at the output budget (finish_reason=length).
		// ONE retry with a reduced prompt when dropping old messages
		// can help; the unrepairable-JSON branch below is trigger #2
		// and truncationRetried bounds the whole request to one retry.
		if strings.EqualFold(wire.Choices[0].FinishReason, "length") &&
			!truncationRetried && res.cfg.TruncationRetry.Enabled {
			if shrunk, changed := ShrinkRequestForTokenLimit(req, caps.MaxContextTokens); changed {
				truncationRetried = true
				req = shrunk
				messages, err = buildOpenAIMessages(req)
				if err != nil {
					return nil, err
				}
				continue
			}
		}

		// ---- response-path resilience (issue #128) ----
		if res.cfg.StripReasoning {
			if clean, reasoning, found := StripReasoningBlocks(resp.Text); found {
				resp.Text, resp.Reasoning = clean, reasoning
				metaSet(resp, MetaReasoningStripped, "true")
			}
		}

		// JSON repair: hard requirement when the caller asked for JSON,
		// best-effort salvage for unprompted (but markdown-fenced) payloads.
		if res.cfg.RepairJSON && (req.WantJSON || looksJSONFenced(resp.Text)) {
			repaired, repairs, rerr := RepairJSON(resp.Text, RepairFirstComplete)
			switch {
			case rerr == nil && (repairs.Applied() || repairs.MultipleObjects):
				resp.Text = repaired
				metaSet(resp, MetaJSONRepaired, repairFlagsString(repairs))
			case rerr == nil:
				// Already clean JSON — nothing to record.
			default:
				// Unrepairable: ONE token-limit retry with a reduced prompt
				// (finish_reason=length / truncated JSON), then surface a
				// typed error for JSON requests. Unprompted payloads fall back
				// to the raw text — repair must never fail a request that did
				// not ask for JSON.
				if !truncationRetried && res.cfg.TruncationRetry.Enabled {
					if shrunk, changed := ShrinkRequestForTokenLimit(req, caps.MaxContextTokens); changed {
						truncationRetried = true
						req = shrunk
						messages, err = buildOpenAIMessages(req)
						if err != nil {
							return nil, err
						}
						continue
					}
				}
				if req.WantJSON {
					// Double-%w keeps both sentinels matchable via errors.Is.
					return nil, fmt.Errorf("%w: %w", ErrInvalidResponse, rerr)
				}
			}
		}

		if truncationRetried {
			metaSet(resp, MetaTruncationRetry, "true")
		}
		if jsonFallback {
			metaSet(resp, MetaJSONFallback, "prompt_only")
		}
		return resp, nil
	}
}

// looksJSONFenced reports whether a payload starts with a markdown code
// fence — the shape models use when they emit JSON unprompted (the repair
// pass is then applied best-effort; see Complete).
func looksJSONFenced(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "```")
}

// encodeChatPayload maps the request onto the wire payload. When
// useMaxCompletionTokens (resolved wire capability) the output budget rides
// max_completion_tokens instead of the legacy max_tokens; sendJSONFormat
// attaches the negotiated response_format (issue #128).
func encodeChatPayload(model string, messages []openAIChatMessage, req CompletionRequest, useMaxCompletionTokens, sendJSONFormat bool) ([]byte, error) {
	wire := openAIChatRequest{
		Model:       model,
		Messages:    messages,
		Temperature: req.Temperature,
	}
	if useMaxCompletionTokens {
		wire.MaxCompletionTokens = req.MaxTokens
	} else {
		wire.MaxTokens = req.MaxTokens
	}
	if sendJSONFormat {
		wire.ResponseFormat = &openAIResponseFormat{Type: "json_object"}
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode request: %v", ErrInvalidRequest, err)
	}
	return payload, nil
}

// sendChat performs ONE HTTP round trip: POST the payload, read the body
// (bounded) and return the status, response headers and body. Transport and
// body-read failures come back as typed errors; classification of non-2xx
// statuses is the caller's job (attemptWithBackoff).
func (p *OpenAIProvider) sendChat(ctx context.Context, payload []byte) (int, http.Header, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: cannot build request: %v", ErrInvalidRequest, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return 0, nil, nil, p.wrapTransportError(ctx, err)
	}
	defer httpResp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(httpResp.Body, maxOpenAIResponseBytes))
	if readErr != nil {
		return httpResp.StatusCode, httpResp.Header, nil,
			fmt.Errorf("%w: reading response body failed: %v", ErrInvalidResponse, readErr)
	}
	return httpResp.StatusCode, httpResp.Header, body, nil
}

// attemptWithBackoff runs the transient-failure retry loop (issue #128):
// 429 / 5xx / network failures are retried up to the configured MaxRetries
// with exponential backoff + full jitter (honoring Retry-After when
// configured); auth failures, invalid requests, malformed responses and
// cancellation surface immediately. res==nil or a disabled RetryConfig means
// a single attempt (the legacy behavior). Returns the parsed chat response
// or the last typed error.
func (p *OpenAIProvider) attemptWithBackoff(ctx context.Context, payload []byte, res *Resilience) (*openAIChatResponse, error) {
	maxRetries := 0
	if res != nil && res.cfg.Enabled && res.cfg.Retry.Enabled {
		if n := res.cfg.Retry.MaxRetries; n > 0 {
			maxRetries = n
		}
	}
	for attempt := 0; ; attempt++ {
		status, header, body, err := p.sendChat(ctx, payload)
		if err == nil && status >= 200 && status <= 299 {
			var parsed openAIChatResponse
			if jerr := json.Unmarshal(body, &parsed); jerr != nil {
				return nil, fmt.Errorf("%w: malformed JSON from provider: %v", ErrInvalidResponse, jerr)
			}
			if len(parsed.Choices) == 0 {
				return nil, fmt.Errorf("%w: provider returned no choices", ErrInvalidResponse)
			}
			return &parsed, nil
		}
		clsErr := err
		if clsErr == nil {
			clsErr = p.statusError(status, body)
		}
		if !IsTransient(clsErr) {
			return nil, clsErr
		}
		if attempt >= maxRetries {
			return nil, clsErr
		}
		var retryAfter string
		if header != nil {
			retryAfter = header.Get("Retry-After")
		}
		if waitErr := resilienceSleep(ctx, res.clock, res.BackoffDelay(attempt, retryAfter)); waitErr != nil {
			return nil, waitErr
		}
	}
}

// buildOpenAIMessages maps the vendor-agnostic request onto chat messages.
// Full conversation (req.Messages) wins over the simple Prompt/System form.
func buildOpenAIMessages(req CompletionRequest) ([]openAIChatMessage, error) {
	if len(req.Messages) > 0 {
		messages := make([]openAIChatMessage, 0, len(req.Messages)+1)
		if strings.TrimSpace(req.System) != "" {
			messages = append(messages, openAIChatMessage{Role: "system", Content: req.System})
		}
		for _, m := range req.Messages {
			role := strings.TrimSpace(m.Role)
			if role == "" {
				return nil, fmt.Errorf("%w: message role is required", ErrInvalidRequest)
			}
			messages = append(messages, openAIChatMessage{Role: role, Content: m.Content, Name: m.Name})
		}
		return messages, nil
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("%w: prompt or messages are required", ErrInvalidRequest)
	}
	messages := make([]openAIChatMessage, 0, 2)
	if strings.TrimSpace(req.System) != "" {
		messages = append(messages, openAIChatMessage{Role: "system", Content: strings.TrimSpace(req.System)})
	}
	messages = append(messages, openAIChatMessage{Role: "user", Content: prompt})
	return messages, nil
}

// statusError maps non-2xx HTTP statuses onto typed sentinel errors.
func (p *OpenAIProvider) statusError(status int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > errSnippetLen {
		snippet = snippet[:errSnippetLen] + "..."
	}
	switch {
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%w (http %d): %s", ErrRateLimited, status, snippet)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w (http %d): %s", ErrProviderAuth, status, snippet)
	case status == http.StatusBadRequest,
		status == http.StatusNotFound,
		status == http.StatusUnprocessableEntity,
		status == http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w (http %d): %s", ErrInvalidRequest, status, snippet)
	default:
		// 5xx, 408 request timeout, and anything unknown: retryable.
		return fmt.Errorf("%w (http %d): %s", ErrProviderUnavailable, status, snippet)
	}
}

// wrapTransportError classifies client.Do failures. Both the sentinel and the
// underlying error stay unwrappable via errors.Is.
func (p *OpenAIProvider) wrapTransportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("models: provider %q request aborted: %w", p.name, ctxErr)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("models: provider %q request timed out: %w: %w", p.name, context.DeadlineExceeded, err)
	}
	return fmt.Errorf("models: provider %q request failed: %w: %w", p.name, ErrProviderUnavailable, err)
}
