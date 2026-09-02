package models

import (
	"context"
	"strings"
	"sync"
	"time"
)

// FailoverProvider implements Provider by trying a primary provider and then
// a fixed fallback chain in order. Only transient failures (rate limits,
// unavailability, network timeouts — see IsTransient) trigger failover;
// invalid requests, auth failures and cancellation are surfaced immediately.
//
// A simple exponential backoff (bounded) separates attempts so tight
// failover does not hammer a struggling provider. FailoverProvider is safe
// for concurrent use.
type FailoverProvider struct {
	mu        sync.Mutex
	primary   Provider
	fallbacks []Provider
	baseDelay time.Duration
}

// NewFailoverProvider builds a failover chain. At least one provider is
// required. Duplicate nil entries are ignored.
func NewFailoverProvider(primary Provider, fallbacks ...Provider) (*FailoverProvider, error) {
	if primary == nil {
		return nil, ErrNoProvider
	}
	chain := make([]Provider, 0, len(fallbacks)+1)
	for _, p := range fallbacks {
		if p != nil {
			chain = append(chain, p)
		}
	}
	return &FailoverProvider{primary: primary, fallbacks: chain, baseDelay: 50 * time.Millisecond}, nil
}

// Name identifies the chain for logs and step records.
func (f *FailoverProvider) Name() string {
	if f == nil {
		return ""
	}
	names := make([]string, 0, len(f.fallbacks)+1)
	names = append(names, f.primary.Name())
	for _, p := range f.fallbacks {
		names = append(names, p.Name())
	}
	return strings.Join(names, "->")
}

// Complete tries the chain in order and returns the first success. When every
// attempt fails, the LAST error is returned so callers see the most specific
// cause.
func (f *FailoverProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	if f == nil || f.primary == nil {
		return nil, ErrNoProvider
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	resp, err := f.primary.Complete(ctx, req)
	if err == nil || !IsTransient(err) {
		return resp, err
	}

	delay := f.baseDelay
	lastErr := err
	for _, p := range f.fallbacks {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
		resp, err = p.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !IsTransient(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

// compile-time check
var _ Provider = (*FailoverProvider)(nil)
