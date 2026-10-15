package tools

// codeexec.go — the code execution tool (issue #111). It routes snippets by
// language and runs each one in a FRESH child process of its own process
// group, so a call can never leak processes, state or environment into the
// worker (this is the tool's own process isolation; the registry-level
// sandbox seam, tools.WithSandboxRunner, remains available for deployments
// that want the full cmd/sandbox-exec isolation — the child must then
// register this tool too).
//
// Isolation and resource model, per issue #111:
//
//   - Language routing: python -> python3 <file>, javascript -> node <file>,
//     go -> go run <file>. Runtimes are resolved at execution time
//     (exec.LookPath); an absent binary is a typed ErrRuntimeMissing the
//     caller renders as a clean "runtime unavailable" step. Tests can pin
//     fake runtimes via WithRuntime to exercise every logic path
//     deterministically.
//   - Network DISABLED (best-effort): the child environment is SCRUBBED to a
//     small allowlist (no credentials, no proxy settings, no service URLs)
//     and Python/Node are pinned to quiet, non-caching behavior. There is no
//     portable user-space way to hard-block sockets for a child process —
//     deployments that require a hard network kill switch must run the
//     platform sandbox (AGENTOS_TOOL_SANDBOX=exec) or a network namespace;
//     this is documented honestly in the tool schema as well.
//   - Output caps: combined stdout/stderr are truncated at the configured
//     limit (default 64 KiB) with a "truncated" flag — the child is left
//     running to finish but nothing beyond the cap is captured.
//   - Timeout: default 15s, hard max 60s. The deadline cancels the command's
//     context; the Cancel hook then SIGKILLs the whole process group, so
//     grandchildren die with the child (no orphans).
//   - Temp file cleanup: every execution writes the snippet into its own
//     temp directory and removes the whole directory when the call returns,
//     including on the error paths.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// CodeExecToolName is the registry name of the code execution tool.
const CodeExecToolName = "code_execute"

// Code execution defaults/hard caps (issue #111).
const (
	// DefaultCodeExecTimeout bounds one snippet execution.
	DefaultCodeExecTimeout = 15 * time.Second
	// MaxCodeExecTimeout is the hard timeout ceiling.
	MaxCodeExecTimeout = 60 * time.Second
	// DefaultCodeExecMaxCodeBytes bounds the snippet source size.
	DefaultCodeExecMaxCodeBytes = 128 << 10
	// DefaultCodeExecMaxOutputBytes bounds captured stdout+stderr.
	DefaultCodeExecMaxOutputBytes = 64 << 10
	// codeExecKillGrace is the WaitDelay grace between the context deadline
	// and the final hard kill of a stuck child.
	codeExecKillGrace = 2 * time.Second
)

// Code execution errors.
var (
	// ErrRuntimeMissing is returned when the language's binary is not
	// installed in the execution environment.
	ErrRuntimeMissing = errors.New("tools: language runtime is not available")
	// ErrCodeExecTimeout is returned when the snippet exceeds its deadline.
	ErrCodeExecTimeout = errors.New("tools: code execution timed out")
)

// codeRuntime describes how to run one language: binary plus arguments, with
// the literal "{file}" placeholder standing for the snippet file path.
type codeRuntime struct {
	binary string
	args   []string
	file   string // file name written into the temp dir (with extension)
}

// codeLanguageAliases maps accepted "language" spellings onto canonical keys.
var codeLanguageAliases = map[string]string{
	"python": "python", "python3": "python", "py": "python",
	"javascript": "javascript", "js": "javascript", "node": "javascript",
	"go": "go", "golang": "go",
}

// defaultCodeRuntimes resolves lazily at execution time via LookPath.
var defaultCodeRuntimes = map[string]codeRuntime{
	"python":     {binary: "python3", args: []string{"{file}"}, file: "main.py"},
	"javascript": {binary: "node", args: []string{"{file}"}, file: "main.js"},
	"go":         {binary: "go", args: []string{"run", "{file}"}, file: "main.go"},
}

// codeExecEnvAllowlist is the scrubbed child environment. Everything else —
// credentials, API keys, proxy settings, service URLs — is stripped.
var codeExecEnvAllowlist = []string{
	"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR",
	// Go needs a writable build cache; only pass-through when the operator
	// configured it (the value itself is a path, never a credential).
	"GOPATH", "GOCACHE", "GOMODCACHE", "GOTOOLCHAIN",
}

// CodeExecTool executes code snippets per language in isolated children.
type CodeExecTool struct {
	runtimes       map[string]codeRuntime
	maxCodeBytes   int
	maxOutputBytes int
	defaultTimeout time.Duration
	maxTimeout     time.Duration
	gate           *ToolPolicyGate
}

// NewCodeExecTool returns the tool with default limits and the built-in
// language routing (python/javascript/go).
func NewCodeExecTool() *CodeExecTool {
	runtimes := make(map[string]codeRuntime, len(defaultCodeRuntimes))
	for k, v := range defaultCodeRuntimes {
		runtimes[k] = v
	}
	return &CodeExecTool{
		runtimes:       runtimes,
		maxCodeBytes:   DefaultCodeExecMaxCodeBytes,
		maxOutputBytes: DefaultCodeExecMaxOutputBytes,
		defaultTimeout: DefaultCodeExecTimeout,
		maxTimeout:     MaxCodeExecTimeout,
	}
}

// WithRuntime overrides (or adds) the runtime for a canonical language key
// ("python", "javascript", "go"). The "{file}" placeholder in args is
// replaced with the snippet path. Intended for tests and for pinning
// containerized interpreters in production.
func (t *CodeExecTool) WithRuntime(language, binary string, args ...string) *CodeExecTool {
	key := codeLanguageAliases[strings.ToLower(strings.TrimSpace(language))]
	if key == "" {
		key = strings.ToLower(strings.TrimSpace(language))
	}
	file := defaultCodeRuntimes[key].file
	if file == "" {
		file = "main.txt"
	}
	t.runtimes[key] = codeRuntime{binary: binary, args: args, file: file}
	return t
}

// WithOutputLimit overrides the stdout/stderr cap.
func (t *CodeExecTool) WithOutputLimit(limit int) *CodeExecTool {
	if limit > 0 {
		t.maxOutputBytes = limit
	}
	return t
}

// WithPolicyGate attaches the per-tool policy gate (see policy.go).
func (t *CodeExecTool) WithPolicyGate(gate *ToolPolicyGate) *CodeExecTool {
	t.gate = gate
	return t
}

// Name implements Tool.
func (t *CodeExecTool) Name() string { return CodeExecToolName }

// DescribeTool publishes catalog metadata (issue #111).
func (t *CodeExecTool) DescribeTool() ToolInfo {
	return ToolInfo{
		Name: t.Name(),
		Description: "Executes a short code snippet in an isolated child process " +
			"(python3, node or go run). Network access is disabled best-effort via a " +
			"scrubbed environment; stdout/stderr are capped and truncated beyond " +
			"the limit; the whole process group is killed on timeout.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"language": map[string]any{
					"type":        "string",
					"enum":        []string{"python", "javascript", "go"},
					"description": "Snippet language (python | javascript | go).",
				},
				"code": map[string]any{
					"type":        "string",
					"description": "Snippet source, max 128 KiB. For go, a complete package main file.",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Execution deadline in milliseconds (defaults to 15000, capped at 60000).",
				},
				"estimated_cost_cents": map[string]any{
					"type":        "integer",
					"description": "Optional estimated cost surfaced to the policy engine (max_cost_cents condition).",
				},
			},
			"required": []string{"language", "code"},
		},
	}
}

// Execute implements Tool using a background context.
func (t *CodeExecTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware: the deadline drives both the
// command context and the hard process-group kill.
func (t *CodeExecTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t == nil || t.runtimes == nil {
		return nil, errors.New("tools: code execution tool is not initialized")
	}
	if err := t.gate.Allow(ctx, t.Name(), toolEstimatedCostCents(input["estimated_cost_cents"])); err != nil {
		return nil, err
	}

	language, err := stringArg(input["language"])
	if err != nil {
		return nil, fmt.Errorf("%w: language: %v", ErrInvalidToolInput, err)
	}
	runtimeKey := codeLanguageAliases[strings.ToLower(strings.TrimSpace(language))]
	if runtimeKey == "" {
		return nil, fmt.Errorf("%w: language %q is not supported (python|javascript|go)", ErrInvalidToolInput, language)
	}
	spec, ok := t.runtimes[runtimeKey]
	if !ok || spec.binary == "" {
		return nil, fmt.Errorf("%w: language %q has no runtime configured", ErrRuntimeMissing, runtimeKey)
	}
	if _, err := exec.LookPath(spec.binary); err != nil {
		return nil, fmt.Errorf("%w: %s is not installed in this environment", ErrRuntimeMissing, spec.binary)
	}

	code, err := stringArg(input["code"])
	if err != nil {
		return nil, fmt.Errorf("%w: code: %v", ErrInvalidToolInput, err)
	}
	if len(code) > t.maxCodeBytes {
		return nil, fmt.Errorf("%w: code exceeds %d bytes", ErrInvalidToolInput, t.maxCodeBytes)
	}

	timeout := timeoutArg(input["timeout_ms"], t.defaultTimeout, t.maxTimeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "agentos-codeexec-")
	if err != nil {
		return nil, fmt.Errorf("creating execution temp dir failed: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }() // snippet + any build artifacts die here

	snippetPath := filepath.Join(dir, spec.file)
	if err := os.WriteFile(snippetPath, []byte(code), 0o600); err != nil {
		return nil, fmt.Errorf("writing snippet failed: %v", err)
	}

	args := make([]string, len(spec.args))
	for i, a := range spec.args {
		args[i] = strings.ReplaceAll(a, "{file}", snippetPath)
	}
	cmd := exec.CommandContext(runCtx, spec.binary, args...)
	cmd.Dir = dir
	cmd.Env = scrubCodeExecEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	setProcessGroup(cmd.SysProcAttr)
	// On deadline: kill the whole process group, then give Wait a short grace
	// to reap before the final hard kill. No orphan processes survive.
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}
	cmd.WaitDelay = codeExecKillGrace

	stdout := &cappedBuffer{limit: t.maxOutputBytes}
	stderr := &cappedBuffer{limit: t.maxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	if runCtx.Err() != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w after %s (language %s)", ErrCodeExecTimeout, duration.Round(time.Millisecond), runtimeKey)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return nil, fmt.Errorf("snippet exited with code %d (stderr: %s)",
				exitErr.ExitCode(), tailString(stderr.String(), 512))
		}
		return nil, fmt.Errorf("starting %s failed: %v", spec.binary, runErr)
	}

	return map[string]any{
		"language":    runtimeKey,
		"stdout":      stdout.String(),
		"stderr":      stderr.String(),
		"exit_code":   0,
		"truncated":   stdout.truncated || stderr.truncated,
		"duration_ms": duration.Milliseconds(),
	}, nil
}

// scrubCodeExecEnv builds the stripped child environment: the allowlisted
// passthrough variables plus forced quiet-mode settings for the interpreters.
func scrubCodeExecEnv() []string {
	env := make([]string, 0, len(codeExecEnvAllowlist)+4)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		for _, allowed := range codeExecEnvAllowlist {
			if key == allowed {
				env = append(env, entry)
				break
			}
		}
	}
	return append(env,
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONUNBUFFERED=1",
		"PYTHONNOUSERSITE=1",
	)
}

// cappedBuffer accumulates at most limit bytes and flags truncation; writes
// beyond the cap are discarded (counted, not stored) so the pipe never
// backpressures the child into a deadlock.
type cappedBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

// Write implements io.Writer with the cap.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.limit <= 0 || len(c.buf.Bytes()) >= c.limit {
		c.truncated = c.truncated || len(p) > 0
		return len(p), nil
	}
	room := c.limit - c.buf.Len()
	if len(p) <= room {
		return c.buf.Write(p)
	}
	_, _ = c.buf.Write(p[:room])
	c.truncated = true
	return len(p), nil
}

// String returns the captured bytes.
func (c *cappedBuffer) String() string {
	return c.buf.String()
}

// tailString keeps the last n bytes of s (stderr excerpts in error messages).
func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
