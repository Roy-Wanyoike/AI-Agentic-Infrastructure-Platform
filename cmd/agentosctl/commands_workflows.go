package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"agentos/internal/sdk"
)

// cmdWorkflows dispatches the workflows subcommands.
func cmdWorkflows(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "workflows requires a subcommand: list, get, create, execute")
	}
	switch args[0] {
	case "list":
		return workflowsList(ctx, args[1:])
	case "get":
		return workflowsGet(ctx, args[1:])
	case "create":
		return workflowsCreate(ctx, args[1:])
	case "execute":
		return workflowsExecute(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, workflowsUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown workflows subcommand %q (want list, get, create, execute)", args[0])
	}
}

const workflowsUsage = `usage: agentosctl workflows <subcommand> [flags]

  workflows list                    list the caller's workflows
      flags: [-json]
  workflows get WORKFLOW_ID         show one workflow with its DSL/versions
      flags: [-json]
  workflows create                  create a draft workflow from a DSL file
      flags: -name NAME -dsl-file FILE [-description TEXT] [-json]
  workflows execute WORKFLOW_ID [INPUT]
                                    expand the DAG into agent runs
      flags: [-input TEXT] [-json]
`

// workflowsList implements `workflows list` (GET /v1/workflows).
func workflowsList(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "workflows list")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	list, err := client.ListWorkflows(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, list)
	}
	rows := make([][]string, 0, len(list.Workflows))
	for _, wf := range list.Workflows {
		rows = append(rows, []string{wf.ID, wf.Name, wf.Status, fmt.Sprintf("%d", wf.CurrentVersion)})
	}
	printTable(ctx.stdout, []string{"ID", "NAME", "STATUS", "VERSION"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d workflow(s)\n", len(list.Workflows))
	return exitOK
}

// workflowsGet implements `workflows get ID` (GET /v1/workflows/{id}).
func workflowsGet(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "workflows get")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	id := popFront(&args)
	if strings.TrimSpace(id) == "" {
		return usageFail(ctx, "workflows get requires a workflow id\nusage: agentosctl workflows get WORKFLOW_ID")
	}
	client := clientFor(ctx)
	wf, err := client.GetWorkflow(ctxRun(ctx), id)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, wf)
	}
	printDetail(ctx.stdout, map[string]string{
		"id":      wf.ID,
		"name":    wf.Name,
		"status":  wf.Status,
		"version": fmt.Sprintf("%d", wf.CurrentVersion),
		"nodes":   fmt.Sprintf("%d node(s), %d edge(s)", len(wf.DSL.Nodes), len(wf.DSL.Edges)),
	})
	return exitOK
}

// workflowsCreate implements `workflows create` (POST /v1/workflows/create).
// The DSL is read from a JSON file: {"nodes":[…],"edges":[…]}.
func workflowsCreate(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "workflows create")
	nameFlag := fs.String("name", "", "workflow name (required)")
	descFlag := fs.String("description", "", "workflow description")
	dslFile := fs.String("dsl-file", "", "path to a DSL JSON file (required)")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*nameFlag) == "" || strings.TrimSpace(*dslFile) == "" {
		return usageFail(ctx, "workflows create requires -name and -dsl-file\nusage: agentosctl workflows create -name NAME -dsl-file FILE")
	}
	raw, err := os.ReadFile(*dslFile)
	if err != nil {
		return fail(ctx, err)
	}
	var dsl sdk.DSL
	if err := json.Unmarshal(raw, &dsl); err != nil {
		return usageFail(ctx, "parse DSL file %s: %v", *dslFile, err)
	}
	client := clientFor(ctx)
	res, err := client.CreateWorkflow(ctxRun(ctx), sdk.CreateWorkflowRequest{
		Name:        *nameFlag,
		Description: *descFlag,
		DSL:         dsl,
	})
	if err != nil {
		// 422 DSL validation arrays print item by item (describeAPIError).
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, res)
	}
	printDetail(ctx.stdout, map[string]string{
		"id":      res.Workflow.ID,
		"name":    res.Workflow.Name,
		"status":  res.Workflow.Status,
		"version": fmt.Sprintf("%d", res.Workflow.CurrentVersion),
	})
	return exitOK
}

// workflowsExecute implements `workflows execute ID [INPUT]`
// (POST /v1/workflows/{id}/execute).
func workflowsExecute(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "workflows execute")
	inputFlag := fs.String("input", "", "execution input (or pass it as a positional argument)")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	id := popFront(&args)
	input := firstNonEmpty(*inputFlag, strings.Join(args, " "))
	if strings.TrimSpace(id) == "" {
		return usageFail(ctx, "workflows execute requires a workflow id\nusage: agentosctl workflows execute WORKFLOW_ID [INPUT]")
	}
	client := clientFor(ctx)
	res, err := client.ExecuteWorkflow(ctxRun(ctx), id, input)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, res)
	}
	printDetail(ctx.stdout, map[string]string{
		"workflow run id": res.WorkflowRunID,
		"status":          res.Status,
		"agent runs":      fmt.Sprintf("%d", len(res.RunIDs)),
	})
	for _, runID := range res.RunIDs {
		fmt.Fprintf(ctx.stdout, "  run %s\n", runID)
	}
	return exitOK
}
