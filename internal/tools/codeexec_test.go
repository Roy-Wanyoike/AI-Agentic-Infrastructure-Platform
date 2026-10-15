package tools

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runtimeAvailable reports whether a binary is on PATH (skip helper).
func runtimeAvailable(t *testing.T, binary string) {
	t.Helper()
	if _, err := exec.LookPath(binary); err != nil {
		t.Skipf("%s is not installed in this environment; skipping real-runtime case", binary)
	}
}

func TestCodeExecToolValidationAlwaysApplies(t *testing.T) {
	tool := NewCodeExecTool()
	cases := []struct {
		name  string
		input map[string]any
	}{
		{"missing language", map[string]any{"code": "print(1)"}},
		{"unknown language", map[string]any{"language": "ruby", "code": "puts 1"}},
		{"missing code", map[string]any{"language": "python"}},
		{"empty code", map[string]any{"language": "python", "code": "   "}},
		{"oversized code", map[string]any{"language": "python", "code": strings.Repeat("1", DefaultCodeExecMaxCodeBytes+1)}},
		{"non-string code", map[string]any{"language": "python", "code": 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.ExecuteContext(nil, tc.input); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
}

func TestCodeExecToolLanguageRouting(t *testing.T) {
	// Routing table is deterministic and independent of installed binaries.
	tool := NewCodeExecTool()
	for _, alias := range []string{"python", "Python3", "PY", "javascript", "JS", "node", "go", "golang"} {
		if codeLanguageAliases[strings.ToLower(alias)] == "" {
			t.Fatalf("alias %q missing from routing table", alias)
		}
	}
	if codeLanguageAliases["ruby"] != "" {
		t.Fatal("unexpected alias")
	}
	// WithRuntime pins a fake runtime under a canonical key regardless of
	// LookPath (used by the deterministic tests below).
	tool.WithRuntime("python", "/bin/sh", "-c", "echo pinned")
	if spec := tool.runtimes["python"]; spec.binary != "/bin/sh" {
		t.Fatalf("WithRuntime override failed: %#v", spec)
	}
}

func TestCodeExecToolFakeRuntimeSuccessAndCaps(t *testing.T) {
	tool := NewCodeExecTool().WithOutputLimit(64)
	tool.WithRuntime("python", "/bin/sh", "-c", "echo hello-stdout; echo hello-stderr 1>&2")

	result, err := tool.ExecuteContext(nil, map[string]any{"language": "python", "code": "irrelevant"})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	if stdout, _ := result["stdout"].(string); !strings.Contains(stdout, "hello-stdout") {
		t.Fatalf("unexpected stdout: %#v", result["stdout"])
	}
	if stderr, _ := result["stderr"].(string); !strings.Contains(stderr, "hello-stderr") {
		t.Fatalf("unexpected stderr: %#v", result["stderr"])
	}
	if result["exit_code"] != 0 {
		t.Fatalf("expected exit 0, got %#v", result["exit_code"])
	}

	// Output cap: a chatty child is truncated at the limit and flagged.
	tool2 := NewCodeExecTool().WithOutputLimit(128)
	tool2.WithRuntime("python", "/bin/sh", "-c", "yes x | head -c 10000")
	result, err = tool2.ExecuteContext(nil, map[string]any{"language": "python", "code": "x"})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	if stdout, _ := result["stdout"].(string); len(stdout) != 128 {
		t.Fatalf("expected stdout capped at 128 bytes, got %d", len(stdout))
	}
	if result["truncated"] != true {
		t.Fatalf("expected truncated=true, got %#v", result["truncated"])
	}
}

func TestCodeExecToolFakeRuntimeTimeoutAndProcessGroupKill(t *testing.T) {
	tool := NewCodeExecTool()
	// A stubborn descendant survives a plain process kill; the group kill in
	// cmd.Cancel must reap it (no orphans — house rule).
	tool.WithRuntime("python", "/bin/sh", "-c", "sleep 30 & wait")
	start := time.Now()
	_, err := tool.ExecuteContext(nil, map[string]any{
		"language": "python", "code": "x", "timeout_ms": float64(300),
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrCodeExecTimeout) {
		t.Fatalf("expected ErrCodeExecTimeout, got %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("timeout did not bound execution (took %s)", elapsed)
	}
}

func TestCodeExecToolNonZeroExitSurfacesStderr(t *testing.T) {
	tool := NewCodeExecTool()
	tool.WithRuntime("python", "/bin/sh", "-c", "echo boom 1>&2; exit 3")
	_, err := tool.ExecuteContext(nil, map[string]any{"language": "python", "code": "x"})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if !strings.Contains(err.Error(), "exited with code 3") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should carry exit code and stderr tail, got: %v", err)
	}
}

func TestCodeExecToolMissingRuntimeFailsTyped(t *testing.T) {
	tool := NewCodeExecTool()
	tool.WithRuntime("go", "/nonexistent-agentos-binary-xyz", "{file}")
	_, err := tool.ExecuteContext(nil, map[string]any{"language": "go", "code": "package main"})
	if !errors.Is(err, ErrRuntimeMissing) {
		t.Fatalf("expected ErrRuntimeMissing, got %v", err)
	}
}

func TestCodeExecToolEnvScrubbing(t *testing.T) {
	t.Setenv("AGENTOS_SEARCH_API_KEY", "sk-super-secret")
	t.Setenv("HTTP_PROXY", "http://proxy:8080")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "welp")
	env := scrubCodeExecEnv()
	blob := strings.Join(env, "\n")
	for _, forbidden := range []string{"AGENTOS_SEARCH_API_KEY", "sk-super-secret", "HTTP_PROXY", "AWS_SECRET_ACCESS_KEY", "welp"} {
		if strings.Contains(blob, forbidden) {
			t.Fatalf("scrubbed env leaked %q: %s", forbidden, blob)
		}
	}
	if !strings.Contains(blob, "PYTHONDONTWRITEBYTECODE=1") {
		t.Fatalf("forced python quiet vars missing: %s", blob)
	}
	// PATH is allowlisted so the interpreters keep resolving.
	if !strings.Contains(blob, "PATH=") {
		t.Fatal("PATH should be allowlisted")
	}
}

func TestCodeExecToolTempCleanup(t *testing.T) {
	// Snippet files live in their own temp dir and must be gone after the
	// call (success and failure paths). We detect leftovers by listing dirs
	// before and after with the same prefix.
	prefix := "agentos-codeexec-"
	tool := NewCodeExecTool()
	tool.WithRuntime("python", "/bin/sh", "-c", "cat {file}")
	tmp := os.TempDir()
	_, err := tool.ExecuteContext(nil, map[string]any{"language": "python", "code": "MARKER-agentos-cleanup"})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	// Give the deferred RemoveAll a beat, then assert no directory still
	// holds our marker content (files are 0600, dirs removed wholesale).
	time.Sleep(20 * time.Millisecond)
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		data, _ := os.ReadFile(tmp + "/" + e.Name() + "/main.py")
		if strings.Contains(string(data), "MARKER-agentos-cleanup") {
			t.Fatalf("snippet file leaked in %s", e.Name())
		}
	}
}

// --- real-runtime cases (skipped when the binary is absent) ---

func TestCodeExecToolPythonReal(t *testing.T) {
	runtimeAvailable(t, "python3")
	tool := NewCodeExecTool()
	result, err := tool.ExecuteContext(nil, map[string]any{
		"language": "python",
		"code":     "import sys\nsys.stdout.write('py-ok')\n",
	})
	if err != nil {
		t.Fatalf("python snippet failed: %v", err)
	}
	if stdout, _ := result["stdout"].(string); strings.TrimSpace(stdout) != "py-ok" {
		t.Fatalf("unexpected python stdout %q", stdout)
	}
}

func TestCodeExecToolPythonTimeoutReal(t *testing.T) {
	runtimeAvailable(t, "python3")
	tool := NewCodeExecTool()
	_, err := tool.ExecuteContext(nil, map[string]any{
		"language":   "python",
		"code":       "import time\ntime.sleep(30)\n",
		"timeout_ms": float64(300),
	})
	if !errors.Is(err, ErrCodeExecTimeout) {
		t.Fatalf("expected ErrCodeExecTimeout, got %v", err)
	}
}

func TestCodeExecToolJavascriptReal(t *testing.T) {
	runtimeAvailable(t, "node")
	tool := NewCodeExecTool()
	result, err := tool.ExecuteContext(nil, map[string]any{
		"language": "javascript",
		"code":     "process.stdout.write('js-' + (6 * 7));",
	})
	if err != nil {
		t.Fatalf("js snippet failed: %v", err)
	}
	if stdout, _ := result["stdout"].(string); strings.TrimSpace(stdout) != "js-42" {
		t.Fatalf("unexpected js stdout %q", stdout)
	}
}

func TestCodeExecToolGoReal(t *testing.T) {
	runtimeAvailable(t, "go")
	tool := NewCodeExecTool()
	result, err := tool.ExecuteContext(nil, map[string]any{
		"language": "go",
		"code":     "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Print(\"go-ok\") }\n",
	})
	if err != nil {
		t.Fatalf("go snippet failed: %v", err)
	}
	if stdout, _ := result["stdout"].(string); strings.TrimSpace(stdout) != "go-ok" {
		t.Fatalf("unexpected go stdout %q", stdout)
	}
}
