// Command sandbox-exec is the child-side helper for internal/sandbox (issue
// #27). The parent Runner spawns one short-lived instance per tool call,
// writes a single JSON request line to stdin, and reads a single JSON
// response line ("result|error") from stdout — see internal/sandbox/protocol.go.
//
// Lifecycle: parse flags -> apply rlimits to itself (best-effort, Linux only)
// -> read one request -> execute the named tool -> write one response -> exit.
//
// Tools available inside the child are the platform's built-ins registered
// from internal/tools (calculator, http_request) plus a few explicitly
// documented sandbox built-ins used for observability and testing:
//
//	env_echo      -> {"env": [...]} the child's exact environment (lets
//	                 tests prove allowlist-only env scrubbing)
//	sandbox_echo  -> {"data": strings.Repeat(fill, bytes)} (bounded)
//	sandbox_sleep -> sleeps input.ms milliseconds (bounded at 120s); the
//	                 sleep-style job used to exercise the parent timeout kill
//	sandbox_spin  -> busy-loops input.ms milliseconds burning CPU; used to
//	                 prove the Linux RLIMIT_CPU enforcement end-to-end
//
// Flags:
//
//	-cpu-seconds/-mem-bytes/-fsize-bytes/-nofile  rlimit hints written by the
//	    parent Runner from its Limits (applied by this process to itself via
//	    syscall.Setrlimit; Linux only, best-effort, never raised above what an
//	    unprivileged process may set).
//	-allow-private  registers the SSRF-protection-disabled http_request
//	    variant. For tests and explicitly trusted environments only — the
//	    production Runner never passes this.
//	-exit-early     exit immediately without reading stdin. Test-only hook to
//	    exercise the parent's missing-response/protocol-error path.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"agentos/internal/sandbox"
	"agentos/internal/tools"
)

// bounded sandbox built-ins
const (
	maxEchoBytes  = 64 << 20
	maxSleepMs    = 120_000
	maxRequestCap = sandbox.MaxRequestBytes
)

// spinSink keeps the CPU-burn loop observable to the compiler so the rlimit
// CPU test cannot be optimized into a no-op.
var spinSink uint64

func main() {
	cpuSeconds := flag.Int64("cpu-seconds", sandbox.DefaultCPUSeconds, "RLIMIT_CPU hint (soft=hard), seconds")
	memBytes := flag.Int64("mem-bytes", sandbox.DefaultMemBytes, "RLIMIT_AS hint (soft=hard), bytes")
	fsizeBytes := flag.Int64("fsize-bytes", sandbox.DefaultFileSizeBytes, "RLIMIT_FSIZE hint (soft=hard), bytes")
	nofile := flag.Uint64("nofile", sandbox.DefaultNoFile, "RLIMIT_NOFILE hint (soft=hard)")
	allowPrivate := flag.Bool("allow-private", false, "register the SSRF-disabled http_request variant (tests/trusted environments only)")
	exitEarly := flag.Bool("exit-early", false, "exit immediately without reading stdin (test-only)")
	flag.Parse()

	if *exitEarly {
		return
	}

	// Rlimits first, before any request is read: the child's own budget is
	// in place for the entire tool execution. Best-effort by design; a
	// failure is reported on stderr (captured by the parent for diagnostics)
	// and never aborts the run — the parent-enforced timeout/output caps
	// remain the hard boundary.
	if err := applyRlimits(*cpuSeconds, *memBytes, *fsizeBytes, *nofile); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-exec: rlimit application incomplete: %v\n", err)
	}

	line, readErr := readRequestLine()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		fatal("reading request: %v", readErr)
		return
	}
	line = strings.TrimSpace(line)
	if line == "" {
		if errors.Is(readErr, io.EOF) {
			// Parent closed stdin without a request: exit cleanly.
			return
		}
		writeErrorAndExit("empty request line")
		return
	}
	if errors.Is(readErr, io.EOF) {
		// Hit the request size cap without a terminating newline.
		writeErrorAndExit(fmt.Sprintf("request exceeds %d bytes", maxRequestCap))
		return
	}

	req, err := sandbox.DecodeRequest([]byte(line))
	if err != nil {
		writeErrorAndExit(err.Error())
		return
	}

	result, toolErr := executeJob(req, *allowPrivate)
	resp := sandbox.Response{OK: toolErr == nil, Result: result}
	if toolErr != nil {
		resp.Error = toolErr.Error()
	}
	if err := sandbox.WriteResponse(os.Stdout, resp); err != nil {
		// Parent is gone or killed us mid-write; nothing else to do.
		os.Exit(1)
	}
}

// readRequestLine reads the single request line, bounded by maxRequestCap.
func readRequestLine() (string, error) {
	reader := bufio.NewReader(io.LimitReader(os.Stdin, maxRequestCap))
	return reader.ReadString('\n')
}

// executeJob dispatches the request: sandbox built-ins first, then the
// internal/tools registry (the same built-in tool surface the worker
// registers).
func executeJob(req sandbox.Request, allowPrivate bool) (map[string]any, error) {
	ctx := context.Background()
	switch req.Tool {
	case "env_echo":
		env := os.Environ()
		sort.Strings(env)
		return map[string]any{"env": env}, nil
	case "sandbox_echo":
		return sandboxEcho(req.Input)
	case "sandbox_sleep":
		return sandboxSleep(req.Input)
	case "sandbox_spin":
		return sandboxSpin(req.Input)
	}

	registry := buildToolRegistry(allowPrivate)
	tool, ok := registry.Get(req.Tool)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q in sandbox", req.Tool)
	}
	if aware, ok := tool.(tools.ContextAware); ok {
		return aware.ExecuteContext(ctx, req.Input)
	}
	return tool.Execute(req.Input)
}

// buildToolRegistry mirrors the worker's built-in tool set.
func buildToolRegistry(allowPrivate bool) *tools.Registry {
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())
	if allowPrivate {
		registry.Register(tools.NewHTTPRequestToolAllowPrivate())
	} else {
		registry.Register(tools.NewHTTPRequestTool())
	}
	return registry
}

// sandboxEcho returns a string of the requested size (bounded) so parents can
// exercise output-cap enforcement deterministically.
func sandboxEcho(input map[string]any) (map[string]any, error) {
	n := 0
	switch v := input["bytes"].(type) {
	case float64:
		n = int(v)
	case nil:
		n = 0
	default:
		return nil, fmt.Errorf("bytes must be a number, got %T", input["bytes"])
	}
	if n < 0 || n > maxEchoBytes {
		return nil, fmt.Errorf("bytes must be within [0, %d]", maxEchoBytes)
	}
	fill := "x"
	if f, ok := input["fill"].(string); ok && f != "" {
		fill = f
	}
	return map[string]any{"data": strings.Repeat(fill, n)}, nil
}

// sandboxSleep sleeps the requested milliseconds (bounded): the sleep-style
// job for the parent's hard-timeout kill path.
func sandboxSleep(input map[string]any) (map[string]any, error) {
	ms := sleepMsArg(input)
	start := time.Now()
	time.Sleep(time.Duration(ms) * time.Millisecond)
	return map[string]any{"slept_ms": ms, "actual_ms": time.Since(start).Milliseconds()}, nil
}

// sandboxSpin busy-loops burning CPU for the requested milliseconds (bounded):
// the job that proves RLIMIT_CPU kills runaway children on Linux.
func sandboxSpin(input map[string]any) (map[string]any, error) {
	ms := sleepMsArg(input)
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	for time.Now().Before(deadline) {
		for i := 0; i < 100_000; i++ {
			spinSink += uint64(i)
		}
	}
	return map[string]any{"spun_ms": ms, "sink": spinSink}, nil
}

func sleepMsArg(input map[string]any) int64 {
	var ms int64
	switch v := input["ms"].(type) {
	case float64:
		ms = int64(v)
	case int64:
		ms = v
	case nil:
		ms = 0
	default:
		ms = 0
	}
	if ms < 0 {
		ms = 0
	}
	if ms > maxSleepMs {
		ms = maxSleepMs
	}
	return ms
}

// writeErrorAndExit reports a request-level failure as a protocol error
// response (exit stays 0: the child was healthy, the request was not).
func writeErrorAndExit(msg string) {
	resp := sandbox.Response{OK: false, Error: msg}
	if err := sandbox.WriteResponse(os.Stdout, resp); err != nil {
		os.Exit(1)
	}
}

// fatal reports an internal child failure on stderr (the parent embeds a
// bounded tail in its protocol error) and exits non-zero.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sandbox-exec: "+format+"\n", args...)
	os.Exit(1)
}
