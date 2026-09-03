package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agentos/internal/sdk"
)

// stubRunGetter feeds canned runs to watchRun (pure in-memory; no transport).
type stubRunGetter struct {
	runs    []*sdk.Run
	err     error
	calls   int
	history []string
}

func (s *stubRunGetter) GetRun(context.Context, string) (*sdk.Run, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	idx := s.calls - 1
	if idx >= len(s.runs) {
		idx = len(s.runs) - 1
	}
	run := s.runs[idx]
	s.history = append(s.history, run.Status)
	return run, nil
}

func newWatchContext() *cliContext {
	return &cliContext{stdout: new(bytes.Buffer), stderr: new(bytes.Buffer)}
}

func TestWatchRunTransitionsToCompleted(t *testing.T) {
	stub := &stubRunGetter{runs: []*sdk.Run{
		{ID: "r1", Status: sdk.RunStatusQueued},
		{ID: "r1", Status: sdk.RunStatusRunning},
		{ID: "r1", Status: sdk.RunStatusCompleted},
	}}
	ctx := newWatchContext()
	run, code := watchRun(ctx, stub, "r1", time.Millisecond, time.Second)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %q)", code, exitOK, ctx.stderr)
	}
	if run.Status != sdk.RunStatusCompleted {
		t.Errorf("final status = %q", run.Status)
	}
	if stub.calls != 3 {
		t.Errorf("calls = %d, want 3", stub.calls)
	}
	out := ctx.stdout.(*bytes.Buffer).String()
	for _, want := range []string{"status: QUEUED", "status: RUNNING", "status: COMPLETED"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Transitions print once per change, not per poll.
	if strings.Count(out, "status: COMPLETED") != 1 {
		t.Errorf("terminal status should print once: %q", out)
	}
}

func TestWatchRunFailedExitsNonzero(t *testing.T) {
	stub := &stubRunGetter{runs: []*sdk.Run{
		{ID: "r1", Status: sdk.RunStatusFailed, Output: "boom"},
	}}
	ctx := newWatchContext()
	run, code := watchRun(ctx, stub, "r1", time.Millisecond, time.Second)
	if code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
	if run == nil || run.Output != "boom" {
		t.Errorf("final run = %+v", run)
	}
}

func TestWatchRunTimeout(t *testing.T) {
	stub := &stubRunGetter{runs: []*sdk.Run{{ID: "r1", Status: sdk.RunStatusQueued}}}
	ctx := newWatchContext()
	run, code := watchRun(ctx, stub, "r1", time.Millisecond, 5*time.Millisecond)
	if code != exitError || run != nil {
		t.Errorf("timeout: exit = %d, run = %+v", code, run)
	}
	if !strings.Contains(ctx.stderr.(*bytes.Buffer).String(), "did not reach a terminal state") {
		t.Errorf("stderr = %q", ctx.stderr.(*bytes.Buffer).String())
	}
}

func TestWatchRunGetError(t *testing.T) {
	stub := &stubRunGetter{err: &sdk.APIError{StatusCode: 404, Status: "404 Not Found", Message: "run not found"}}
	ctx := newWatchContext()
	run, code := watchRun(ctx, stub, "missing", time.Millisecond, time.Second)
	if code != exitError || run != nil {
		t.Errorf("error path: exit = %d, run = %+v", code, run)
	}
	if !strings.Contains(ctx.stderr.(*bytes.Buffer).String(), "not found (404)") {
		t.Errorf("stderr = %q", ctx.stderr.(*bytes.Buffer).String())
	}
}

func TestWatchRunStillQueuedEventuallyCompletes(t *testing.T) {
	// A run stuck QUEUED for many polls must keep polling until terminal.
	stub := &stubRunGetter{runs: []*sdk.Run{
		{ID: "r1", Status: sdk.RunStatusQueued},
		{ID: "r1", Status: sdk.RunStatusQueued},
		{ID: "r1", Status: sdk.RunStatusQueued},
		{ID: "r1", Status: sdk.RunStatusCompleted, Output: "4"},
	}}
	ctx := newWatchContext()
	run, code := watchRun(ctx, stub, "r1", time.Millisecond, time.Second)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if run.Output != "4" || stub.calls != 4 {
		t.Errorf("calls = %d, run = %+v", stub.calls, run)
	}
}

func TestErrStringPassThrough(t *testing.T) {
	if got := describeAPIError(errors.New("boom")); got != "error: boom" {
		t.Errorf("describe = %q", got)
	}
}
