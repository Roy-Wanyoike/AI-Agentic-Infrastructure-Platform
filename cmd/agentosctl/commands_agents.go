package main

import (
	"fmt"
	"strings"
	"time"

	"agentos/internal/sdk"
)

// cmdAgents dispatches the agents subcommands.
func cmdAgents(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "agents requires a subcommand: list, create, get, run")
	}
	switch args[0] {
	case "list":
		return agentsList(ctx, args[1:])
	case "create":
		return agentsCreate(ctx, args[1:])
	case "get":
		return agentsGet(ctx, args[1:])
	case "run":
		return agentsRun(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, agentsUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown agents subcommand %q (want list, create, get, run)", args[0])
	}
}

const agentsUsage = `usage: agentosctl agents <subcommand> [flags]

  agents list                       list the caller's agents
      flags: [-org ORG_ID] [-json]
  agents create                     create an agent (DRAFT status + version 1)
      flags: -name NAME -instructions TEXT -model MODEL
             [-description TEXT] [-org ORG_ID] [-json]
  agents get AGENT_ID               show one agent
      flags: [-json]
  agents run AGENT_ID [INPUT]       enqueue a run for the agent
      flags: [-input TEXT] [-json]
`

// agentsList implements `agents list` (GET /v1/agents, bare array).
func agentsList(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "agents list")
	orgFlag := fs.String("org", "", "organization id (defaults to the token's tenant)")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	agents, err := client.ListAgents(ctxRun(ctx), *orgFlag)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, agents)
	}
	rows := make([][]string, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, []string{a.ID, a.Name, a.Model, a.Status, a.CreatedAt.UTC().Format(time.RFC3339)})
	}
	printTable(ctx.stdout, []string{"ID", "NAME", "MODEL", "STATUS", "CREATED"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d agent(s)\n", len(agents))
	return exitOK
}

// agentsCreate implements `agents create` (POST /v1/agents/create).
func agentsCreate(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "agents create")
	nameFlag := fs.String("name", "", "agent name (required)")
	descFlag := fs.String("description", "", "agent description")
	instrFlag := fs.String("instructions", "", "agent instructions (required)")
	modelFlag := fs.String("model", "", "model id, e.g. gpt-4o-mini (required)")
	orgFlag := fs.String("org", "", "organization id (defaults to the token's tenant)")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*nameFlag) == "" || strings.TrimSpace(*instrFlag) == "" || strings.TrimSpace(*modelFlag) == "" {
		return usageFail(ctx, "agents create requires -name, -instructions and -model")
	}
	client := clientFor(ctx)
	agent, err := client.CreateAgent(ctxRun(ctx), sdk.CreateAgentRequest{
		OrganizationID: *orgFlag,
		Name:           *nameFlag,
		Description:    *descFlag,
		Instructions:   *instrFlag,
		Model:          *modelFlag,
	})
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, agent)
	}
	printDetail(ctx.stdout, map[string]string{
		"id":          agent.ID,
		"name":        agent.Name,
		"model":       agent.Model,
		"status":      agent.Status,
		"version":     agent.CurrentVersionID,
		"description": agent.Description,
	})
	return exitOK
}

// agentsGet implements `agents get ID` (GET /v1/agents/{id}).
func agentsGet(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "agents get")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	id := popFront(&args)
	if strings.TrimSpace(id) == "" {
		return usageFail(ctx, "agents get requires an agent id\nusage: agentosctl agents get AGENT_ID")
	}
	client := clientFor(ctx)
	agent, err := client.GetAgent(ctxRun(ctx), id)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, agent)
	}
	printDetail(ctx.stdout, map[string]string{
		"id":           agent.ID,
		"name":         agent.Name,
		"model":        agent.Model,
		"status":       agent.Status,
		"version":      agent.CurrentVersionID,
		"instructions": agent.Instructions,
		"created":      agent.CreatedAt.UTC().Format(time.RFC3339),
		"updated":      agent.UpdatedAt.UTC().Format(time.RFC3339),
	})
	return exitOK
}

// agentsRun implements `agents run AGENT_ID [INPUT]` (POST /v1/runs).
func agentsRun(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "agents run")
	inputFlag := fs.String("input", "", "run input (or pass it as a positional argument)")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	agentID := popFront(&args)
	input := firstNonEmpty(*inputFlag, strings.Join(args, " "))
	if strings.TrimSpace(agentID) == "" {
		return usageFail(ctx, "agents run requires an agent id\nusage: agentosctl agents run AGENT_ID [INPUT]")
	}
	client := clientFor(ctx)
	res, err := client.CreateRun(ctxRun(ctx), sdk.CreateRunRequest{AgentID: agentID, Input: input})
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, res)
	}
	printDetail(ctx.stdout, map[string]string{
		"run id": res.RunID,
		"status": res.Status,
		"next":   fmt.Sprintf("agentosctl runs watch %s", res.RunID),
	})
	return exitOK
}
