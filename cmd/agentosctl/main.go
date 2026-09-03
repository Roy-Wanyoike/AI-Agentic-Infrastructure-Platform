// Command agentosctl is the AgentOS command-line client.
//
// Configuration precedence: environment variables (AGENTOS_URL,
// AGENTOS_TOKEN, AGENTOS_API_KEY) over the profile stored in
// ~/.agentos/config.json (written by `agentosctl login`) over built-in
// defaults (http://localhost:8080).
//
// Usage:
//
//	agentosctl [-json] <command> [flags]
//
// Commands: login, register, whoami, agents, runs, workflows, knowledge,
// usage, tools. Run `agentosctl <command> -h` for per-command flags. --json
// is accepted before or after the command and emits machine-readable output
// for every command.
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one CLI invocation and returns the process exit code. It is
// the seam used by the unit tests (no network, in-memory streams).
func run(args []string, stdout, stderr io.Writer) int {
	// Global flags before the command name (-json/-h). Every subcommand also
	// accepts -json after the command so `agentosctl agents list --json`
	// works with plain flag semantics.
	jsonOut := false
	for len(args) > 0 {
		switch args[0] {
		case "-json", "--json":
			jsonOut = true
			args = args[1:]
			continue
		case "-h", "--help", "help":
			printMainUsage(stdout)
			return exitOK
		}
		break
	}
	if len(args) == 0 {
		printMainUsage(stderr)
		return exitUsage
	}
	name, rest := args[0], args[1:]

	var cmd *command
	for i := range commands {
		if commands[i].name == name {
			cmd = &commands[i]
			break
		}
	}
	if cmd == nil {
		fmt.Fprintf(stderr, "error: unknown command %q\n\n", name)
		printMainUsage(stderr)
		return exitUsage
	}

	cfg, err := effectiveConfig()
	if err != nil {
		fmt.Fprintf(stderr, "error: load config: %v\n", err)
		return exitError
	}
	ctx := &cliContext{stdout: stdout, stderr: stderr, cfg: cfg, json: jsonOut}
	return cmd.run(ctx, rest)
}

// printMainUsage renders the top-level help text.
func printMainUsage(w io.Writer) {
	fmt.Fprint(w, `agentosctl — AgentOS command-line client

usage: agentosctl [-json] <command> [flags]

commands:
`)
	for _, cmd := range commands {
		fmt.Fprintf(w, "  %-10s %s\n", cmd.name, cmd.summary)
	}
	fmt.Fprint(w, `
global flags:
  -json            emit JSON output (also accepted after the command)
  -h, --help       show help

configuration:
  `+EnvURL+`      API base URL (default http://localhost:8080)
  `+EnvToken+`    bearer token (overrides the stored login token)
  `+EnvAPIKey+`  API key sent as X-API-Key
  profile file:    `+mustConfigPath()+` (written by `+"`agentosctl login`"+`)

examples:
  agentosctl login -email dev@example.com -password secret
  agentosctl agents list
  agentosctl agents create -name helper -instructions "be brief" -model gpt-4o-mini
  agentosctl agents run AGENT_ID "what is 2+2?"
  agentosctl runs watch RUN_ID
  agentosctl usage costs -group-by agent
`)
}

// listSubcommands is a small helper used by tests to assert the dispatch
// table wiring.
func listSubcommands() []string {
	names := make([]string, 0, len(commands))
	for _, c := range commands {
		names = append(names, c.name)
	}
	return names
}
