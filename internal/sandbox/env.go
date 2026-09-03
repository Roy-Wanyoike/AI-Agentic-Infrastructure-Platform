package sandbox

import (
	"log/slog"
	"os"
	"strings"
	"time"
)

// env.go holds the env-driven construction seam, mirroring the
// models.ProviderFromEnv convention: an operator-facing env knob read once at
// wiring time, with loud logging instead of silent behavior changes.

// Environment variables:
//
//	AGENTOS_TOOL_SANDBOX     "exec" enables sandboxed tool execution; unset,
//	                         "off", "false" or "0" keeps tools in-process (the
//	                         zero-infrastructure default). Any other value is
//	                         rejected with a warning and tools stay in-process.
//	AGENTOS_SANDBOX_EXEC_BIN optional explicit path to the cmd/sandbox-exec
//	                         helper; when unset the helper is looked up on PATH.
//	AGENTOS_SANDBOX_TIMEOUT  optional per-execution wall-clock budget, a Go
//	                         duration (e.g. "45s", "2m"). Invalid or non-positive
//	                         values are warned about and the default is kept.
const (
	EnvSandboxMode   = "AGENTOS_TOOL_SANDBOX"
	EnvSandboxBinary = "AGENTOS_SANDBOX_EXEC_BIN"
	EnvSandboxTime   = "AGENTOS_SANDBOX_TIMEOUT"
)

// RunnerFromEnv reads the AGENTOS_TOOL_SANDBOX knob and constructs a Runner.
// It returns (nil, false) when sandboxed execution is disabled or cannot be
// honored (helper binary missing) — in that case the caller keeps the
// default in-process tool execution, with a warning on the "requested but
// unavailable" path so a misconfigured sandbox never masquerades as healthy.
// A nil logger falls back to slog.Default.
func RunnerFromEnv(logr *slog.Logger) (*Runner, bool) {
	if logr == nil {
		logr = slog.Default()
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(EnvSandboxMode)))
	switch mode {
	case "", "off", "false", "0":
		return nil, false // default: in-process, unchanged zero-infra mode
	case "exec":
		// enabled below
	default:
		logr.Warn("unknown AGENTOS_TOOL_SANDBOX value; tools stay in-process",
			"value", os.Getenv(EnvSandboxMode))
		return nil, false
	}

	opts := []Option{}
	if bin := strings.TrimSpace(os.Getenv(EnvSandboxBinary)); bin != "" {
		opts = append(opts, WithBinary(bin))
	}
	if raw := strings.TrimSpace(os.Getenv(EnvSandboxTime)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			logr.Warn("invalid AGENTOS_SANDBOX_TIMEOUT; using default",
				"value", raw, "default", DefaultTimeout.String())
		} else {
			opts = append(opts, WithTimeout(d))
		}
	}

	runner, err := NewRunner(opts...)
	if err != nil {
		logr.Warn("sandboxed tool execution requested but the helper binary is unavailable; tools stay in-process",
			"error", err)
		return nil, false
	}
	logr.Info("sandboxed tool execution enabled",
		"binary", runner.binary, "timeout", runner.timeout.String())
	return runner, true
}
