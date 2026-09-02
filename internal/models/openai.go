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
	name    string
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
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
		name:    strings.TrimSpace(name),
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: trimmed,
		model:   strings.TrimSpace(model),
		client:  httpClient,
	}
}

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
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
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

// Complete implements Provider by POSTing {baseURL}/chat/completions.
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

	messages, err := buildOpenAIMessages(req)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(openAIChatRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode request: %v", ErrInvalidRequest, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build request: %v", ErrInvalidRequest, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, p.wrapTransportError(ctx, err)
	}
	defer httpResp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(httpResp.Body, maxOpenAIResponseBytes))
	if readErr != nil {
		return nil, fmt.Errorf("%w: reading response body failed: %v", ErrInvalidResponse, readErr)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return nil, p.statusError(httpResp.StatusCode, body)
	}

	var parsed openAIChatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: malformed JSON from provider: %v", ErrInvalidResponse, err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("%w: provider returned no choices", ErrInvalidResponse)
	}

	usage := Usage{
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		TotalTokens:      parsed.Usage.TotalTokens,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return &CompletionResponse{
		Text:  parsed.Choices[0].Message.Content,
		Model: parsed.Model,
		Usage: usage,
	}, nil
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
