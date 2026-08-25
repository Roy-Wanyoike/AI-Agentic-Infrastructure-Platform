package models

import (
	"context"
	"errors"
	"strings"
)

type CompletionRequest struct {
	Prompt string
	System string
}

type CompletionResponse struct {
	Text string
	Model string
}

type Provider interface {
	Name() string
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
}

type StaticProvider struct {
	name    string
	text    string
	modelID string
}

func NewStaticProvider(name, text string) *StaticProvider {
	return &StaticProvider{name: strings.TrimSpace(name), text: text, modelID: strings.TrimSpace(name)}
}

func (p *StaticProvider) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

func (p *StaticProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	if p == nil {
		return nil, errors.New("provider is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	_ = ctx
	responseText := p.text
	if strings.TrimSpace(responseText) == "" {
		responseText = "ok"
	}
	return &CompletionResponse{Text: responseText, Model: p.modelID}, nil
}
