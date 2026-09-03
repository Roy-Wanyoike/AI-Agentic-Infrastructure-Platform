package tools

import (
	"context"
	"errors"
	"fmt"
)

// sandbox.go adds the process-isolation seam for tool execution (issue #27):
// a Registry's tools can be wrapped so every call is executed by a
// SandboxExecutor instead of in-process — typically sandbox.Runner from
// internal/sandbox, which runs the work in a short-lived cmd/sandbox-exec
// child process with hard timeouts, output caps and env scrubbing.
//
// The seam is opt-in and additive: nothing in the registry changes until
// WithSandboxRunner is called, and the default (off) keeps the zero-infra
// in-process behavior byte-identical.

// SandboxExecutor executes ONE tool call on behalf of the registry. It is
// satisfied by *sandbox.Runner (internal/sandbox); declared here so this
// package does not depend on the sandbox implementation.
type SandboxExecutor interface {
	ExecuteTool(ctx context.Context, tool string, input map[string]any) (map[string]any, error)
}

// sandboxedTool wraps a registered Tool so calls route through the executor
// while every metadata surface (Name, DescribeTool) keeps reflecting the
// inner tool unchanged.
type sandboxedTool struct {
	inner Tool
	exec  SandboxExecutor
}

// Name implements Tool, delegating to the inner tool (the registry key is the
// authoritative tool name and must not change when sandboxing is enabled).
func (s *sandboxedTool) Name() string { return s.inner.Name() }

// Execute implements Tool through the sandbox executor, using a background
// context (matching the plain Tool contract).
func (s *sandboxedTool) Execute(input map[string]any) (map[string]any, error) {
	return s.exec.ExecuteTool(context.Background(), s.inner.Name(), input)
}

// ExecuteContext implements ContextAware so the runtime's per-call timeout
// propagates into the sandbox: the executor derives its own hard kill
// deadline from this context.
func (s *sandboxedTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	return s.exec.ExecuteTool(ctx, s.inner.Name(), input)
}

// DescribeTool forwards the inner tool's catalog metadata (issue #18) so the
// public /v1/tools listing is unchanged by sandboxing.
func (s *sandboxedTool) DescribeTool() ToolInfo {
	if described, ok := s.inner.(Described); ok {
		return described.DescribeTool()
	}
	return ToolInfo{Name: s.inner.Name()}
}

// WithSandboxRunner routes the named tools through exec instead of executing
// them in-process. With no names, only the http tool ("http_request" — the
// network-facing built-in whose workload isolation the sandbox story is for)
// is wrapped; pass explicit names to sandbox other tools (e.g. "calculator").
//
// The default stays in-process: without this call nothing changes. Calling it
// twice for the same tool is a no-op (already-sandboxed tools are not
// double-wrapped), so wiring code can be defensive. Returns an error when
// exec is nil or a named tool is not registered; a nil registry is a no-op.
func WithSandboxRunner(r *Registry, exec SandboxExecutor, names ...string) error {
	if r == nil {
		return nil
	}
	if exec == nil {
		return errors.New("tools: sandbox executor is required")
	}
	if len(names) == 0 {
		names = []string{HTTPToolName}
	}
	for _, name := range names {
		tool, ok := r.Get(name)
		if !ok {
			return fmt.Errorf("tools: cannot sandbox unknown tool %q", name)
		}
		if _, already := tool.(*sandboxedTool); already {
			continue
		}
		r.Register(&sandboxedTool{inner: tool, exec: exec})
	}
	return nil
}
