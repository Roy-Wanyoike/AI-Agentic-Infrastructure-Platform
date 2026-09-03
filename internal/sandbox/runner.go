package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// runner.go is the parent side of the sandbox: it spawns one cmd/sandbox-exec
// child per tool call and guarantees, before returning, that
//   - the child (and, on unix, its whole process group) has been killed if it
//     outlived the response or the deadline,
//   - the direct child has been reaped (cmd.Wait),
//   - the runner's own reader goroutine has exited.
//
// Timeout, output cap and env scrubbing are parent-enforced on every OS; see
// doc.go for the enforced-vs-advisory breakdown per platform.

// Defaults for a zero-configuration Runner. They bound a single tool call:
// the wall-clock timeout is the hard stop, the output cap bounds how much of
// the child's stdout the parent will ever read, and the limits are forwarded
// to the child as rlimit hints (best-effort, Linux-only; see doc.go).
const (
	HelperBinaryName = "sandbox-exec"

	DefaultTimeout        = 30 * time.Second
	DefaultMaxOutputBytes = 1 << 20 // 1 MiB of child stdout, hard parent-side cap

	// Child rlimit hints. RLIMIT_AS is deliberately generous: it counts the
	// child's whole virtual address space and a Go runtime reserves arena
	// address space well beyond its heap usage, so a tight limit would kill
	// healthy children. Tune via WithLimits if needed.
	DefaultCPUSeconds    = int64(30)
	DefaultMemBytes      = int64(1) << 30 // 1 GiB address space
	DefaultFileSizeBytes = int64(16) << 20
	DefaultNoFile        = uint64(64)

	// waitDelay bounds os/exec's own post-kill pipe draining so a stubborn
	// grandchild holding the pipes cannot stall Wait; the process group is
	// already SIGKILLed by then.
	waitDelay = 3 * time.Second

	// maxStderrTail caps how much child stderr is embedded in protocol
	// errors (diagnostics only; the child is not trusted, so its stderr is
	// never returned as tool output).
	maxStderrTail = 512
)

// Limits are the resource-limit hints forwarded to the child as command-line
// flags (the child applies them to itself via syscall.Setrlimit on Linux;
// non-Linux children ignore them). All fields are clamped by the child to
// what an unprivileged process may lower.
type Limits struct {
	CPUSeconds    int64  // RLIMIT_CPU (soft=hard), seconds
	MemBytes      int64  // RLIMIT_AS (soft=hard), bytes of address space
	FileSizeBytes int64  // RLIMIT_FSIZE (soft=hard), bytes written per file
	NoFile        uint64 // RLIMIT_NOFILE (soft=hard), open file descriptors
}

// Typed errors. Use errors.Is to classify sandbox failures; all other errors
// are internal/protocol details wrapped underneath.
var (
	// ErrSandboxUnavailable means the helper binary could not be resolved or
	// started (missing binary, bad path, exec format). The sandbox never
	// silently falls back to in-process execution: the caller decides.
	ErrSandboxUnavailable = errors.New("sandbox: helper binary unavailable")
	// ErrTimeout means the execution exceeded the Runner's wall-clock budget
	// and the child process group was killed.
	ErrTimeout = errors.New("sandbox: execution timed out")
	// ErrOutputCapExceeded means the child produced more stdout than the
	// configured cap; the output is truncated (cut at the cap, never trusted)
	// and the child is killed.
	ErrOutputCapExceeded = errors.New("sandbox: output cap exceeded")
	// ErrProtocol means the child violated the request/response protocol
	// (garbage, no response, crash) or the request could not be encoded.
	ErrProtocol = errors.New("sandbox: protocol violation")
	// ErrToolFailed means the tool executed inside the child and reported a
	// failure (the child itself was healthy).
	ErrToolFailed = errors.New("sandbox: tool failed")
)

// Runner executes tool jobs in sandboxed child processes. Construct with
// NewRunner; a Runner is safe for concurrent use (every ExecuteTool call is
// an independent child process).
type Runner struct {
	binary         string
	childArgs      []string
	timeout        time.Duration
	maxOutputBytes int
	envAllowlist   []string
	limits         Limits
}

// Option configures a Runner.
type Option func(*Runner)

// WithBinary sets the path to the cmd/sandbox-exec helper binary. The path
// is validated (and bare names resolved via PATH) at construction time so
// misconfiguration fails fast instead of on the first tool call.
func WithBinary(path string) Option {
	return func(r *Runner) { r.binary = path }
}

// WithChildArgs appends extra command-line arguments for the child (after the
// rlimit flags the Runner derives from Limits, so later flags win). Intended
// for explicitly documented helper flags.
func WithChildArgs(args ...string) Option {
	return func(r *Runner) { r.childArgs = append(r.childArgs, args...) }
}

// WithTimeout overrides the hard per-execution wall-clock budget. Values <= 0
// keep the default.
func WithTimeout(d time.Duration) Option {
	return func(r *Runner) {
		if d > 0 {
			r.timeout = d
		}
	}
}

// WithMaxOutputBytes overrides the parent-side stdout cap. Values <= 0 keep
// the default.
func WithMaxOutputBytes(n int) Option {
	return func(r *Runner) {
		if n > 0 {
			r.maxOutputBytes = n
		}
	}
}

// WithEnvAllowlist sets the ONLY environment variable names the child
// inherits (resolved from the parent's environment at spawn time). An empty
// allowlist (the default) gives the child an empty environment.
func WithEnvAllowlist(keys ...string) Option {
	return func(r *Runner) { r.envAllowlist = append(r.envAllowlist, keys...) }
}

// WithLimits overrides the rlimit hints forwarded to the child. Zero/negative
// fields keep the default for that field.
func WithLimits(l Limits) Option {
	return func(r *Runner) {
		if l.CPUSeconds > 0 {
			r.limits.CPUSeconds = l.CPUSeconds
		}
		if l.MemBytes > 0 {
			r.limits.MemBytes = l.MemBytes
		}
		if l.FileSizeBytes > 0 {
			r.limits.FileSizeBytes = l.FileSizeBytes
		}
		if l.NoFile > 0 {
			r.limits.NoFile = l.NoFile
		}
	}
}

// NewRunner builds a Runner. Without WithBinary the helper is resolved from
// PATH (HelperBinaryName); construction fails fast with
// ErrSandboxUnavailable when it cannot be found, so an operator who asked for
// sandboxed execution never gets a silent in-process fallback.
func NewRunner(opts ...Option) (*Runner, error) {
	r := &Runner{
		timeout:        DefaultTimeout,
		maxOutputBytes: DefaultMaxOutputBytes,
		limits: Limits{
			CPUSeconds:    DefaultCPUSeconds,
			MemBytes:      DefaultMemBytes,
			FileSizeBytes: DefaultFileSizeBytes,
			NoFile:        DefaultNoFile,
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	if r.binary == "" {
		r.binary = HelperBinaryName
	}
	path, err := exec.LookPath(r.binary)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrSandboxUnavailable, r.binary, err)
	}
	r.binary = path
	return r, nil
}

// ExecuteTool runs one tool job in a fresh child process and returns the
// tool's result map, or a typed error (see the Err* sentinels). The child is
// always killed-if-needed and reaped before this call returns, and no
// runner-owned goroutine outlives it.
func (r *Runner) ExecuteTool(ctx context.Context, tool string, input map[string]any) (map[string]any, error) {
	if r == nil || r.binary == "" {
		return nil, fmt.Errorf("%w: runner is not initialized", ErrSandboxUnavailable)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	execCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	line, err := EncodeRequest(Request{Tool: tool, Input: input})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProtocol, err)
	}

	cmd := exec.CommandContext(execCtx, r.binary, r.buildArgs()...)
	// Own process group (unix): enables the group SIGKILL that also reaps
	// tool-spawned grandchildren. No-op struct edit on Windows.
	cmd.SysProcAttr = setProcessGroup(&syscall.SysProcAttr{})
	cmd.Env = scrubEnv(os.Environ(), r.envAllowlist)
	// Captured for protocol-error diagnostics only: the child is not trusted,
	// so its stderr is never surfaced as tool output, just a bounded tail.
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: stdin pipe: %v", ErrSandboxUnavailable, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: stdout pipe: %v", ErrSandboxUnavailable, err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("%w: starting %s: %v", ErrSandboxUnavailable, r.binary, err)
	}

	// Write the single request line, then close stdin so the child sees EOF
	// after its one request. A dead child surfaces as EPIPE here, which is
	// treated by the response path below (no response -> protocol error).
	_, werr := stdin.Write(line)
	_ = stdin.Close()

	// Reader goroutine: reads at most one capped response line. It exits on
	// its own once the child dies (EOF) or once Wait closes the pipe; we join
	// it before returning so nothing outlives the call.
	type readResult struct {
		data      []byte
		truncated bool
		err       error
	}
	readCh := make(chan readResult, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		data, truncated, rerr := readCappedLine(stdout, r.maxOutputBytes)
		readCh <- readResult{data: data, truncated: truncated, err: rerr}
	}()

	var rr readResult
	timedOut := false
	select {
	case rr = <-readCh:
	case <-execCtx.Done():
		timedOut = true
	}

	// Both exits converge here: make sure the child (and on unix its whole
	// process group) is dead, then reap it. Killing an already-exited process
	// is a harmless no-op.
	killProcessGroup(cmd)
	waitErr := cmd.Wait()

	// Join the reader goroutine: after the kill the pipe hits EOF promptly.
	joinErr := waitWithTimeout(readDone, waitDelay)

	if timedOut {
		// Distinguish "caller's context expired" from "our budget expired".
		if perr := ctx.Err(); perr != nil {
			return nil, perr
		}
		return nil, fmt.Errorf("%w: tool %q exceeded %s and its process group was killed",
			ErrTimeout, tool, r.timeout)
	}
	if joinErr != nil {
		return nil, fmt.Errorf("%w: reader goroutine did not exit: %v", ErrProtocol, joinErr)
	}
	// If we exited the select via the deadline but the reader finished first,
	// pick up its buffered result instead of treating it as a timeout.
	select {
	case rr2 := <-readCh:
		if rr.data == nil && rr.err == nil {
			rr = rr2
		}
	default:
	}

	if werr != nil {
		return nil, protocolError(tool, waitErr, stderr, werr.Error())
	}
	if rr.err != nil {
		return nil, protocolError(tool, waitErr, stderr, "reading child stdout: "+rr.err.Error())
	}
	if rr.truncated {
		// Truncate + flag: the output is cut at the cap and reported as a
		// typed failure instead of being trusted as tool output.
		return nil, fmt.Errorf("%w: child stdout exceeded %d bytes; output truncated and child killed",
			ErrOutputCapExceeded, r.maxOutputBytes)
	}
	if len(bytes.TrimSpace(rr.data)) == 0 {
		return nil, protocolError(tool, waitErr, stderr, "no response from child")
	}

	resp, derr := DecodeResponse(rr.data)
	if derr != nil {
		return nil, protocolError(tool, waitErr, stderr, derr.Error())
	}
	if !resp.OK {
		return nil, fmt.Errorf("%w: %s", ErrToolFailed, resp.Error)
	}
	if resp.Result == nil {
		resp.Result = map[string]any{}
	}
	return resp.Result, nil
}

// buildArgs derives the child argv: rlimit hints first (documented, stable
// order), then the caller's extra args so explicit flags can override.
func (r *Runner) buildArgs() []string {
	args := []string{
		"-cpu-seconds", fmt.Sprintf("%d", r.limits.CPUSeconds),
		"-mem-bytes", fmt.Sprintf("%d", r.limits.MemBytes),
		"-fsize-bytes", fmt.Sprintf("%d", r.limits.FileSizeBytes),
		"-nofile", fmt.Sprintf("%d", r.limits.NoFile),
	}
	return append(args, r.childArgs...)
}

// protocolError builds a consistent ErrProtocol failure carrying the wait
// result and a bounded stderr tail for diagnostics.
func protocolError(tool string, waitErr error, stderr *bytes.Buffer, detail string) error {
	msg := fmt.Sprintf("tool %q: %s", tool, detail)
	if waitErr != nil {
		msg += fmt.Sprintf("; child exit: %v", waitErr)
	}
	if stderr != nil && stderr.Len() > 0 {
		tail := stderr.String()
		tail = strings.TrimSpace(tail)
		if len(tail) > maxStderrTail {
			tail = tail[:maxStderrTail] + "...[truncated]"
		}
		msg += "; child stderr: " + tail
	}
	return fmt.Errorf("%w: %s", ErrProtocol, msg)
}

// readCappedLine reads one newline-terminated line, accumulating at most cap
// bytes. If the line exceeds the cap it stops reading (the caller then kills
// the child) and reports truncated=true with the capped prefix. An
// EOF-terminated final line without newline is accepted.
func readCappedLine(reader interface{ Read([]byte) (int, error) }, cap int) ([]byte, bool, error) {
	const chunk = 32 << 10
	buf := &bytes.Buffer{}
	frag := make([]byte, chunk)
	for {
		n, err := reader.Read(frag)
		if n > 0 {
			if i := bytes.IndexByte(frag[:n], '\n'); i >= 0 {
				buf.Write(frag[:i])
				if buf.Len() > cap {
					return buf.Bytes()[:cap], true, nil
				}
				return buf.Bytes(), false, nil
			}
			buf.Write(frag[:n])
			if buf.Len() > cap {
				return buf.Bytes()[:cap], true, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf.Bytes(), false, nil
			}
			return buf.Bytes(), false, err
		}
	}
}

// waitWithTimeout waits for ch with a deadline so a pathological pipe state
// cannot hang ExecuteTool (the error path is a typed protocol violation).
func waitWithTimeout(ch <-chan struct{}, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return nil
	case <-timer.C:
		return fmt.Errorf("timed out after %s", d)
	}
}

// scrubEnv builds the child environment from the explicit allowlist only:
// never inherit the parent environment wholesale. An empty allowlist yields
// an empty (non-nil) environment.
func scrubEnv(parent []string, allowlist []string) []string {
	env := []string{}
	if len(allowlist) == 0 {
		return env
	}
	allowed := make(map[string]bool, len(allowlist))
	for _, key := range allowlist {
		allowed[key] = true
	}
	for _, kv := range parent {
		if key, _, ok := strings.Cut(kv, "="); ok && allowed[key] {
			env = append(env, kv)
		}
	}
	return env
}
