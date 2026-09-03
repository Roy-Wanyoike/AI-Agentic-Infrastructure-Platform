package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain builds the cmd/sandbox-exec helper once per test binary: the
// parent-side Runner needs a real child process to talk to.
var helperPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "sandbox-helper-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testmain:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	bin := filepath.Join(tmp, HelperBinaryName)
	build := exec.Command("go", "build", "-o", bin, "../../cmd/sandbox-exec")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building %s helper failed: %v\n%s", HelperBinaryName, err, out)
		os.Exit(1)
	}
	helperPath = bin
	os.Exit(m.Run())
}

func newTestRunner(t *testing.T, opts ...Option) *Runner {
	t.Helper()
	r, err := NewRunner(append([]Option{WithBinary(helperPath)}, opts...)...)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// assertNumber compares a JSON-decoded numeric result (always float64).
func assertNumber(t *testing.T, got any, want float64) {
	t.Helper()
	switch v := got.(type) {
	case float64:
		if v != want {
			t.Fatalf("numeric result: got %v, want %v", v, want)
		}
	default:
		t.Fatalf("numeric result: got %#v (%T), want number %v", got, got, want)
	}
}

func TestExecuteToolCalculatorHappyPath(t *testing.T) {
	runner := newTestRunner(t)
	res, err := runner.ExecuteTool(context.Background(), "calculator", map[string]any{"expression": "12*3+4"})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	assertNumber(t, res["result"], 40)
	if res["expression"] != "12*3+4" {
		t.Fatalf("expression echo: got %#v", res["expression"])
	}

	res, err = runner.ExecuteTool(context.Background(), "calculator", map[string]any{"expression": "100/8"})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	assertNumber(t, res["result"], 12.5)
}

func TestExecuteToolEnvScrubbingAllowlistOnly(t *testing.T) {
	t.Setenv("AGENTOS_SANDBOX_TEST_MARKER", "hello")
	// A variable that is certainly in the parent env on every OS but NOT in
	// the allowlist: the child must never see it.
	t.Setenv("AGENTOS_SANDBOX_NOT_ALLOWED", "secret")

	runner := newTestRunner(t, WithEnvAllowlist("AGENTOS_SANDBOX_TEST_MARKER"))
	res, err := runner.ExecuteTool(context.Background(), "env_echo", nil)
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	env, ok := res["env"].([]any)
	if !ok {
		t.Fatalf("env_echo result shape: %#v", res)
	}
	if len(env) != 1 || env[0] != "AGENTOS_SANDBOX_TEST_MARKER=hello" {
		t.Fatalf("child env = %#v, want exactly [AGENTOS_SANDBOX_TEST_MARKER=hello]", env)
	}
}

func TestExecuteToolEnvScrubbingDefaultIsEmpty(t *testing.T) {
	runner := newTestRunner(t) // no allowlist -> empty child env
	res, err := runner.ExecuteTool(context.Background(), "env_echo", nil)
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if env, _ := res["env"].([]any); len(env) != 0 {
		t.Fatalf("default child env = %#v, want empty (never inherited)", env)
	}
}

func TestExecuteToolTimeoutKillsSleep(t *testing.T) {
	runner := newTestRunner(t, WithTimeout(500*time.Millisecond))
	start := time.Now()
	_, err := runner.ExecuteTool(context.Background(), "sandbox_sleep", map[string]any{"ms": 10000})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout kill took %s, expected a prompt process-group kill", elapsed)
	}
}

func TestExecuteToolHonorsCallerCancellation(t *testing.T) {
	runner := newTestRunner(t, WithTimeout(30*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err := runner.ExecuteTool(ctx, "sandbox_sleep", map[string]any{"ms": 10000})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled propagated, got %v", err)
	}
}

func TestExecuteToolOutputCapTruncates(t *testing.T) {
	runner := newTestRunner(t, WithMaxOutputBytes(64))
	start := time.Now()
	_, err := runner.ExecuteTool(context.Background(), "sandbox_echo", map[string]any{"bytes": 4096})
	if !errors.Is(err, ErrOutputCapExceeded) {
		t.Fatalf("want ErrOutputCapExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "64 bytes") {
		t.Fatalf("error should report the cap, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cap kill took %s, expected prompt termination", elapsed)
	}

	// Boundary: output under the cap passes through intact.
	okRunner := newTestRunner(t, WithMaxOutputBytes(1024))
	res, err := okRunner.ExecuteTool(context.Background(), "sandbox_echo", map[string]any{"bytes": 256})
	if err != nil {
		t.Fatalf("under-cap execute: %v", err)
	}
	if data, _ := res["data"].(string); len(data) != 256 {
		t.Fatalf("under-cap echo: got %d bytes, want 256", len(data))
	}
}

func TestExecuteToolErrorPropagation(t *testing.T) {
	runner := newTestRunner(t)

	// Tool-level failure inside a healthy child: message must survive the
	// protocol intact.
	_, err := runner.ExecuteTool(context.Background(), "calculator", map[string]any{"expression": "1/0"})
	if !errors.Is(err, ErrToolFailed) {
		t.Fatalf("want ErrToolFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("tool error text lost in protocol, got: %v", err)
	}

	// Unknown tool: the child reports it, still a tool-level failure.
	_, err = runner.ExecuteTool(context.Background(), "no-such-tool", nil)
	if !errors.Is(err, ErrToolFailed) || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("unknown-tool error propagation broken, got %v", err)
	}
}

func TestExecuteToolProtocolViolationWhenChildDies(t *testing.T) {
	// -exit-early makes the healthy helper exit 0 without ever writing a
	// response: the parent must classify this as a protocol violation.
	runner := newTestRunner(t, WithChildArgs("-exit-early"))
	_, err := runner.ExecuteTool(context.Background(), "calculator", map[string]any{"expression": "1+1"})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("want ErrProtocol, got %v", err)
	}
}

func TestNewRunnerFailsFastWhenBinaryMissing(t *testing.T) {
	if _, err := NewRunner(WithBinary("definitely-not-a-binary-xyz")); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("want ErrSandboxUnavailable at construction, got %v", err)
	}
}

func TestRunnerFromEnv(t *testing.T) {
	t.Run("default off", func(t *testing.T) {
		t.Setenv(EnvSandboxMode, "")
		if runner, ok := RunnerFromEnv(quietLogger()); runner != nil || ok {
			t.Fatalf("default must stay in-process, got (%v, %v)", runner, ok)
		}
	})
	t.Run("explicit off", func(t *testing.T) {
		t.Setenv(EnvSandboxMode, "off")
		if runner, ok := RunnerFromEnv(quietLogger()); runner != nil || ok {
			t.Fatalf("off must stay in-process, got (%v, %v)", runner, ok)
		}
	})
	t.Run("unknown value rejected", func(t *testing.T) {
		t.Setenv(EnvSandboxMode, "yes-please")
		if runner, ok := RunnerFromEnv(quietLogger()); runner != nil || ok {
			t.Fatalf("unknown value must not enable the sandbox, got (%v, %v)", runner, ok)
		}
	})
	t.Run("exec with missing binary stays off", func(t *testing.T) {
		t.Setenv(EnvSandboxMode, "exec")
		t.Setenv(EnvSandboxBinary, "definitely-not-a-binary-xyz")
		if runner, ok := RunnerFromEnv(quietLogger()); runner != nil || ok {
			t.Fatalf("missing helper must degrade to off with a warning, got (%v, %v)", runner, ok)
		}
	})
	t.Run("exec with binary + timeout override works", func(t *testing.T) {
		t.Setenv(EnvSandboxMode, "exec")
		t.Setenv(EnvSandboxBinary, helperPath)
		t.Setenv(EnvSandboxTime, "5s")
		runner, ok := RunnerFromEnv(quietLogger())
		if !ok || runner == nil {
			t.Fatal("expected a runner for mode=exec with a valid helper")
		}
		if runner.timeout != 5*time.Second {
			t.Fatalf("timeout override: got %s, want 5s", runner.timeout)
		}
		res, err := runner.ExecuteTool(context.Background(), "calculator", map[string]any{"expression": "2*3"})
		if err != nil {
			t.Fatalf("ExecuteTool via env runner: %v", err)
		}
		assertNumber(t, res["result"], 6)
	})
	t.Run("invalid timeout keeps default", func(t *testing.T) {
		t.Setenv(EnvSandboxMode, "exec")
		t.Setenv(EnvSandboxBinary, helperPath)
		t.Setenv(EnvSandboxTime, "soon")
		runner, ok := RunnerFromEnv(quietLogger())
		if !ok || runner == nil {
			t.Fatal("expected a runner despite invalid timeout env")
		}
		if runner.timeout != DefaultTimeout {
			t.Fatalf("invalid timeout must keep default, got %s", runner.timeout)
		}
	})
}

func TestScrubEnv(t *testing.T) {
	parent := []string{"A=1", "B=2", "C=3", "BROKEN"}
	if got := scrubEnv(parent, nil); len(got) != 0 {
		t.Fatalf("empty allowlist must produce an empty env, got %v", got)
	}
	got := scrubEnv(parent, []string{"B", "MISSING", "B"})
	if len(got) != 1 || got[0] != "B=2" {
		t.Fatalf("allowlist env = %v, want [B=2]", got)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	runner := newTestRunner(t)
	baseline := runtime.NumGoroutine()

	// Exercise every exit path: success, timeout kill, cap kill, protocol
	// failure. None of them may leave a runner-owned goroutine behind.
	happy := newTestRunner(t)
	if _, err := happy.ExecuteTool(context.Background(), "calculator", map[string]any{"expression": "1+1"}); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	timed := newTestRunner(t, WithTimeout(300*time.Millisecond))
	if _, err := timed.ExecuteTool(context.Background(), "sandbox_sleep", map[string]any{"ms": 8000}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout path: %v", err)
	}
	capped := newTestRunner(t, WithMaxOutputBytes(64))
	if _, err := capped.ExecuteTool(context.Background(), "sandbox_echo", map[string]any{"bytes": 4096}); !errors.Is(err, ErrOutputCapExceeded) {
		t.Fatalf("cap path: %v", err)
	}
	crasher := newTestRunner(t, WithChildArgs("-exit-early"))
	if _, err := crasher.ExecuteTool(context.Background(), "calculator", nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("protocol path: %v", err)
	}
	_ = runner

	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if now := runtime.NumGoroutine(); now > baseline {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutines leaked: baseline %d, final %d\n%s", baseline, now, buf[:n])
	}
}

func TestChildProcessesReaped(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reaping check scans /proc; Linux-only")
	}
	runner := newTestRunner(t)
	for i := 0; i < 3; i++ {
		if _, err := runner.ExecuteTool(context.Background(), "calculator", map[string]any{"expression": "2+2"}); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	if lingering := findSandboxProcesses(); len(lingering) > 0 {
		t.Fatalf("sandbox child processes not reaped: %v", lingering)
	}
}

// findSandboxProcesses lists live processes whose comm starts with
// "sandbox-exec" (comm is truncated to 15 chars by the kernel, so the -test
// suffix of our helper binary name is cut). Zombies show up here too, which
// is exactly what "reaped" must rule out.
func findSandboxProcesses() []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := fmt.Sprintf("%d", os.Getpid())
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isNumericPID(name) || name == self {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", name, "comm"))
		if err != nil {
			continue // process exited between listing and read
		}
		if strings.HasPrefix(strings.TrimSpace(string(comm)), HelperBinaryName) {
			found = append(found, name)
		}
	}
	return found
}

func isNumericPID(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func TestRlimitSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rlimits are Linux-only; timeout+kill+caps still enforced everywhere")
	}

	// Defaults: the child applies its rlimits at startup and must still work.
	runner := newTestRunner(t)
	res, err := runner.ExecuteTool(context.Background(), "calculator", map[string]any{"expression": "6*7"})
	if err != nil {
		t.Fatalf("calculator under default rlimits: %v", err)
	}
	assertNumber(t, res["result"], 42)

	// RLIMIT_CPU enforcement end-to-end: a CPU-burning job dies well before
	// its 15s self-imposed runtime under a 1s CPU budget (SIGXCPU), which the
	// parent reports as a protocol failure (child died without a response).
	cpuCapped := newTestRunner(t, WithLimits(Limits{CPUSeconds: 1}))
	start := time.Now()
	_, err = cpuCapped.ExecuteTool(context.Background(), "sandbox_spin", map[string]any{"ms": 15000})
	if err == nil {
		t.Fatal("CPU-capped spin job must not succeed")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("CPU cap took %s to stop the child, expected ~1s SIGXCPU", elapsed)
	}
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("want ErrProtocol for the killed child, got %v", err)
	}
	if !strings.Contains(err.Error(), "child exit") {
		t.Fatalf("protocol error should carry the wait status, got: %v", err)
	}
}

func TestConcurrentExecutes(t *testing.T) {
	runner := newTestRunner(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			res, err := runner.ExecuteTool(context.Background(), "calculator", map[string]any{"expression": fmt.Sprintf("%d*2", n)})
			if err != nil {
				errs[n] = err
				return
			}
			if got, _ := res["result"].(float64); got != float64(n*2) {
				errs[n] = fmt.Errorf("n=%d: got %v, want %d", n, res["result"], n*2)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent execute %d: %v", i, err)
		}
	}
}
