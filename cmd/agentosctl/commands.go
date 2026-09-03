package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"agentos/internal/sdk"
)

// Exit codes used by every command (documented in the main usage text).
const (
	exitOK    = 0
	exitError = 1 // API/network/operation failure
	exitUsage = 2 // unknown command/subcommand or bad flags
)

// cliContext carries everything a command needs: io streams, resolved config
// and whether --json was requested.
type cliContext struct {
	stdout io.Writer
	stderr io.Writer
	cfg    Config
	json   bool
}

// commandFunc executes one top-level command with its remaining args.
type commandFunc func(ctx *cliContext, args []string) int

// command is one entry of the CLI dispatch table.
type command struct {
	name    string
	summary string
	run     commandFunc
}

// commands is the dispatch table (order defines the usage listing).
var commands = []command{
	{"login", "authenticate with email+password and store the token", cmdLogin},
	{"register", "create a new organization + owner account", cmdRegister},
	{"whoami", "show the identity of the stored token", cmdWhoami},
	{"agents", "list, create, inspect and run agents", cmdAgents},
	{"runs", "inspect runs; watch one until it finishes", cmdRuns},
	{"workflows", "list, create and execute workflows", cmdWorkflows},
	{"knowledge", "list/add documents and search the RAG index", cmdKnowledge},
	{"usage", "usage cost reports", cmdUsage},
	{"tools", "list the public tool registry", cmdTools},
}

// newFlagSet builds a FlagSet that reports usage to stderr and stops at the
// first positional argument.
func newFlagSet(ctx *cliContext, name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(ctx.stderr)
	return fs
}

// clientFor builds an SDK client from the resolved config. Precedence (set
// in effectiveConfig): env vars > config file > sdk defaults.
func clientFor(ctx *cliContext) *sdk.Client {
	opts := []sdk.Option{sdk.WithBaseURL(ctx.cfg.URL)}
	switch {
	case ctx.cfg.Token != "":
		opts = append(opts, sdk.WithToken(ctx.cfg.Token))
	case ctx.cfg.APIKey != "":
		opts = append(opts, sdk.WithAPIKey(ctx.cfg.APIKey))
	}
	return sdk.New(opts...)
}

// fail prints an error using the shared 401/403/404/422 treatment.
func fail(ctx *cliContext, err error) int {
	fmt.Fprintln(ctx.stderr, describeAPIError(err))
	return exitError
}

// usageFail prints a usage hint and returns the usage exit code.
func usageFail(ctx *cliContext, format string, args ...any) int {
	fmt.Fprintf(ctx.stderr, "error: "+format+"\n", args...)
	return exitUsage
}

// firstNonEmpty returns the first non-empty string (flag/positional fallback).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
