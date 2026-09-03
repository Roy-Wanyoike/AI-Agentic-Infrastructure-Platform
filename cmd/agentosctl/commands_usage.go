package main

import (
	"fmt"
	"strings"

	"agentos/internal/sdk"
)

// cmdUsage dispatches the usage subcommands.
func cmdUsage(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "usage requires a subcommand: costs")
	}
	switch args[0] {
	case "costs":
		return usageCosts(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, usageUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown usage subcommand %q (want costs)", args[0])
	}
}

const usageUsage = `usage: agentosctl usage costs [flags]

  usage costs                       aggregate run costs for the caller's org
      flags: [-from RFC3339|YYYY-MM-DD] [-to RFC3339|YYYY-MM-DD]
             [-group-by day|agent|model] [-json]
`

// usageCosts implements `usage costs` (GET /v1/usage/costs).
func usageCosts(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "usage costs")
	fromFlag := fs.String("from", "", "window start (RFC3339 or YYYY-MM-DD; default: to minus 30 days)")
	toFlag := fs.String("to", "", "window end (default: now)")
	groupFlag := fs.String("group-by", "day", "aggregation: day | agent | model")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	switch strings.TrimSpace(*groupFlag) {
	case "day", "agent", "model":
	default:
		return usageFail(ctx, "-group-by must be one of day, agent, model")
	}
	client := clientFor(ctx)
	res, err := client.Costs(ctxRun(ctx), sdk.CostsQuery{
		From:    *fromFlag,
		To:      *toFlag,
		GroupBy: *groupFlag,
	})
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, res)
	}
	rows := make([][]string, 0, len(res.Series))
	for _, b := range res.Series {
		label := b.Bucket
		if b.AgentID != "" {
			label = b.AgentID
		}
		if b.Model != "" {
			label = b.Model
		}
		rows = append(rows, []string{label, fmt.Sprintf("%.2f", b.CostCents), fmt.Sprintf("%d", b.Runs)})
	}
	printTable(ctx.stdout, []string{"GROUP", "COST CENTS", "RUNS"}, rows)
	fmt.Fprintf(ctx.stdout, "\ntotal: %.2f cents across %d bucket(s)\n", res.TotalCostCents, len(res.Series))
	return exitOK
}

// cmdTools implements the tools command (GET /v1/tools).
func cmdTools(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "tools")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	list, err := client.ListTools(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, list)
	}
	rows := make([][]string, 0, len(list.Tools))
	for _, t := range list.Tools {
		schema := ""
		if len(t.InputSchema) > 0 {
			schema = fmt.Sprintf("%d propert(ies)", len(t.InputSchema))
		}
		rows = append(rows, []string{t.Name, t.Description, schema})
	}
	printTable(ctx.stdout, []string{"NAME", "DESCRIPTION", "SCHEMA"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d tool(s)\n", len(list.Tools))
	return exitOK
}
