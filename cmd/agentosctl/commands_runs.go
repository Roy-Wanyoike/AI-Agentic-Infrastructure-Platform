package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agentos/internal/sdk"
)

// cmdRuns dispatches the runs subcommands.
func cmdRuns(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "runs requires a subcommand: get, list, watch")
	}
	switch args[0] {
	case "get":
		return runsGet(ctx, args[1:])
	case "list":
		return runsList(ctx, args[1:])
	case "watch":
		return runsWatch(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, runsUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown runs subcommand %q (want get, list, watch)", args[0])
	}
}

const runsUsage = `usage: agentosctl runs <subcommand> [flags]

  runs get RUN_ID                   show one run (status, output, cost)
      flags: [-json]
  runs list                         list the caller's runs
      flags: [-json]
  runs watch RUN_ID                 poll the run until COMPLETED/FAILED
      flags: [-interval 2s] [-timeout 10m] [-json]
`

// runsGet implements `runs get ID` (GET /v1/runs/{id}, bare object).
func runsGet(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "runs get")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	id := popFront(&args)
	if strings.TrimSpace(id) == "" {
		return usageFail(ctx, "runs get requires a run id\nusage: agentosctl runs get RUN_ID")
	}
	client := clientFor(ctx)
	run, err := client.GetRun(ctxRun(ctx), id)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, run)
	}
	printDetail(ctx.stdout, runDetailFields(run))
	return exitOK
}

// runDetailFields renders one run for the detail view.
func runDetailFields(run *sdk.Run) map[string]string {
	return map[string]string{
		"id":         run.ID,
		"agent id":   run.AgentID,
		"status":     run.Status,
		"input":      run.Input,
		"output":     run.Output,
		"cost cents": fmt.Sprintf("%.2f", run.TotalCostCents),
		"created":    run.CreatedAt.UTC().Format(time.RFC3339),
		"updated":    run.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// runsList implements `runs list` (GET /v1/runs, {"runs":[…]} envelope).
func runsList(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "runs list")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	list, err := client.ListRuns(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, list)
	}
	rows := make([][]string, 0, len(list.Runs))
	for _, r := range list.Runs {
		rows = append(rows, []string{r.ID, r.AgentID, r.Status, r.UpdatedAt.UTC().Format(time.RFC3339)})
	}
	printTable(ctx.stdout, []string{"ID", "AGENT", "STATUS", "UPDATED"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d run(s)\n", len(list.Runs))
	return exitOK
}

// runsWatch implements `runs watch ID`: poll GET /v1/runs/{id} until the run
// reaches a terminal status (COMPLETED/FAILED), printing status transitions
// as they happen and the final step trace. Exit code 0 = COMPLETED, 1 =
// FAILED or timeout.
func runsWatch(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "runs watch")
	intervalFlag := fs.Duration("interval", 2*time.Second, "poll interval")
	timeoutFlag := fs.Duration("timeout", 10*time.Minute, "give up after this duration")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	id := popFront(&args)
	if strings.TrimSpace(id) == "" {
		return usageFail(ctx, "runs watch requires a run id\nusage: agentosctl runs watch RUN_ID [-interval 2s] [-timeout 10m]")
	}
	if *intervalFlag <= 0 {
		return usageFail(ctx, "-interval must be positive")
	}
	client := clientFor(ctx)
	run, code := watchRun(ctx, client, id, *intervalFlag, *timeoutFlag)
	if run == nil {
		return code
	}
	if ctx.json || *jsonFlag {
		// JSON mode emits the final run plus its step trace.
		steps, err := client.Steps(ctxRun(ctx), id)
		if err != nil {
			steps = &sdk.RunSteps{RunID: id, Steps: []sdk.Step{}}
		}
		return printJSON(ctx.stdout, map[string]any{"run": run, "steps": steps.Steps})
	}
	// Final summary + step trace.
	printDetail(ctx.stdout, runDetailFields(run))
	steps, err := client.Steps(ctxRun(ctx), id)
	if err != nil {
		return code
	}
	if len(steps.Steps) > 0 {
		fmt.Fprintln(ctx.stdout)
		rows := make([][]string, 0, len(steps.Steps))
		for _, s := range steps.Steps {
			rows = append(rows, []string{s.ID, s.StepType, s.Status, fmt.Sprintf("%.2f", s.Cost)})
		}
		printTable(ctx.stdout, []string{"STEP", "TYPE", "STATUS", "COST"}, rows)
	}
	return code
}

// runGetter is the slice of the SDK client watchRun needs; the seam keeps
// the polling loop unit-testable without any transport.
type runGetter interface {
	GetRun(ctx context.Context, id string) (*sdk.Run, error)
}

// watchRun polls the run status, printing transitions. It returns the final
// run (nil on error/timeout) and the process exit code.
func watchRun(ctx *cliContext, client runGetter, id string, interval, timeout time.Duration) (*sdk.Run, int) {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		run, err := client.GetRun(ctxRun(ctx), id)
		if err != nil {
			fail(ctx, err)
			return nil, exitError
		}
		if run.Status != last {
			fmt.Fprintf(ctx.stdout, "status: %s\n", run.Status)
			last = run.Status
		}
		if sdk.TerminalRunStatuses[run.Status] {
			if run.Status == sdk.RunStatusFailed {
				return run, exitError
			}
			return run, exitOK
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(ctx.stderr, "error: run %s did not reach a terminal state within %s (last status: %s)\n", id, timeout, last)
			return nil, exitError
		}
		sleepCtx(ctxRun(ctx), interval)
	}
}

// sleepCtx waits d, aborting early when ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
