package main

import (
	"context"
	"os"
)

// envValue wraps os.Getenv (kept as a seam for tests).
func envValue(key string) string { return os.Getenv(key) }

// popFront removes and returns the first remaining positional argument.
func popFront(args *[]string) string {
	if len(*args) == 0 {
		return ""
	}
	v := (*args)[0]
	*args = (*args)[1:]
	return v
}

// dayLayout is the compact date format used by table/detail output for the
// billing period columns (RFC3339 timestamps stay in --json output).
const dayLayout = "2006-01-02"

// ctxRun returns the context for command execution. The CLI is one-shot per
// process, so the base context is used: the process lifetime is the command
// lifetime and there is nothing to cancel from (main wires no signal
// handling; process teardown cancels in-flight requests).
func ctxRun(*cliContext) context.Context { return context.Background() }

// mustConfigPath is the display form of ConfigPath (errors can only come from
// a missing home directory; SaveConfig reports them properly when saving).
func mustConfigPath() string {
	path, err := ConfigPath()
	if err != nil {
		return "(config file)"
	}
	return path
}
