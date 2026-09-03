// Package models defines the vendor-agnostic model provider abstraction used
// by the agent runtime: a completion request/response pair, the Provider
// interface, an OpenAI-compatible adapter, and a failover router.
package models

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Message is one turn of a conversation. Role is one of "system", "user",
// "assistant" or "tool"; Name carries the tool name for role=="tool"
// observations.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// Usage reports token consumption for a completion as reported by the
// provider (0 when the provider does not report usage).
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// CompletionRequest is the vendor-agnostic completion input. Callers may use
// either the simple Prompt/System form or the full Messages conversation;
// providers must support both.
type CompletionRequest struct {
	Prompt string
	System string
	// Messages, when non-empty, takes precedence over Prompt/System.
	Messages []Message
	// MaxTokens caps generation length; 0 means "provider default".
	MaxTokens int
	// Temperature is the sampling temperature; 0 means "provider default".
	Temperature float64
	// Model optionally overrides the provider's configured model for this
	// request (e.g. the model pinned by an agent version).
	Model string
}

// CompletionResponse is the vendor-agnostic completion output.
type CompletionResponse struct {
	Text  string
	Model string
	Usage Usage
}

// Provider is the minimal contract every model backend must satisfy.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
}

// Typed provider errors. Callers must use errors.Is to classify failures.
var (
	// ErrInvalidRequest indicates the request itself is malformed (bad
	// arguments, unsupported parameters).
	ErrInvalidRequest = errors.New("models: invalid request")
	// ErrProviderUnavailable indicates a transport-level or server-side
	// failure that may succeed on retry (5xx, connection errors, timeouts).
	ErrProviderUnavailable = errors.New("models: provider unavailable")
	// ErrRateLimited indicates the provider asked us to back off (HTTP 429).
	ErrRateLimited = errors.New("models: rate limited by provider")
	// ErrProviderAuth indicates credentials are missing or rejected
	// (HTTP 401/403). Never retried automatically.
	ErrProviderAuth = errors.New("models: provider authentication failed")
	// ErrInvalidResponse indicates the provider answered with a malformed or
	// protocol-violating payload.
	ErrInvalidResponse = errors.New("models: invalid provider response")
	// ErrNoProvider is returned by routers when no provider can serve a
	// request.
	ErrNoProvider = errors.New("models: no provider available")
)

// IsTransient reports whether err is the kind of failure a caller may retry
// with the same request: rate limiting, provider unavailability and network
// timeouts. Authentication failures, malformed responses, invalid requests
// and caller cancellation are NOT transient.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrProviderUnavailable) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// StaticProvider is a deterministic, dependency-free Provider for
// development and tests. When constructed with a fixed text it always returns
// that text; when constructed with an empty text it echoes the request back
// (echo mode), which keeps tests honest about what they sent.
type StaticProvider struct {
	name    string
	text    string
	modelID string
}

// NewStaticProvider builds a StaticProvider. A non-empty text pins the
// response; an empty text enables deterministic echo mode.
func NewStaticProvider(name, text string) *StaticProvider {
	trimmedName := strings.TrimSpace(name)
	return &StaticProvider{name: trimmedName, text: text, modelID: trimmedName}
}

// Name implements Provider.
func (p *StaticProvider) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

// Complete implements Provider deterministically, honoring ctx cancellation.
func (p *StaticProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	if p == nil {
		return nil, errors.New("models: provider is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	responseText := strings.TrimSpace(p.text)
	promptTokens := 0
	if responseText == "" {
		// Echo mode: repeat the last user turn back to the caller.
		echo := req.Prompt
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				echo = req.Messages[i].Content
				break
			}
		}
		echo = strings.TrimSpace(echo)
		if echo == "" {
			return nil, fmt.Errorf("%w: prompt is required", ErrInvalidRequest)
		}
		responseText = "echo: " + echo
	}
	if strings.TrimSpace(req.Prompt) == "" && len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: prompt is required", ErrInvalidRequest)
	}

	promptTokens = estimateTokens(req.Prompt) + estimateTokens(req.System)
	for _, m := range req.Messages {
		promptTokens += estimateTokens(m.Content)
	}
	usage := Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: estimateTokens(responseText),
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	model := p.modelID
	if strings.TrimSpace(req.Model) != "" {
		model = strings.TrimSpace(req.Model)
	}
	return &CompletionResponse{Text: responseText, Model: model, Usage: usage}, nil
}

// estimateTokens is a rough deterministic token estimate (~4 chars/token).
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// compile-time interface checks
var (
	_ Provider = (*StaticProvider)(nil)
	_ Provider = (*OpenAIProvider)(nil)
)
