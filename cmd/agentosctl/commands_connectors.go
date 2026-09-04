package main

import (
	"fmt"
	"strings"

	"agentos/internal/sdk"
)

// cmdConnectors dispatches the connectors subcommands.
func cmdConnectors(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "connectors requires a subcommand: create, list, test, delete")
	}
	switch args[0] {
	case "create":
		return connectorsCreate(ctx, args[1:])
	case "list":
		return connectorsList(ctx, args[1:])
	case "test":
		return connectorsTest(ctx, args[1:])
	case "delete":
		return connectorsDelete(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, connectorsUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown connectors subcommand %q (want create, list, test, delete)", args[0])
	}
}

const connectorsUsage = `usage: agentosctl connectors <subcommand> [flags]

  connectors create                 register a new org-scoped connector
      flags: -name NAME -type webhook|http -base-url URL
             [-auth-style none|bearer|basic|api_key_header] [-header K=V]...
             [-api-key-header NAME] [-api-key-prefix PREFIX] [-username USER]
             [-secret-ref SECRET_NAME] [-status active|disabled] [-json]
  connectors list                   list the caller's connectors
      flags: [-json]
  connectors test CONNECTOR_ID      run the live health check
      flags: [-json]
  connectors delete CONNECTOR_ID    hard-delete a connector
      flags: [-json]
`

// headerFlags collects repeated -header K=V pairs into a map (a plain
// flag.Value so static header templates can be set more than once).
type headerFlags map[string]string

func (h headerFlags) String() string {
	parts := make([]string, 0, len(h))
	for k, v := range h {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (h headerFlags) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("header must be K=V, got %q", s)
	}
	h[strings.TrimSpace(k)] = v
	return nil
}

// connectorsCreate implements `connectors create` (POST /v1/connectors).
// The request carries a secret NAME reference only — never a secret value.
func connectorsCreate(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "connectors create")
	nameFlag := fs.String("name", "", "connector name (required)")
	typeFlag := fs.String("type", "", "connector type: webhook | http (required)")
	baseURLFlag := fs.String("base-url", "", "remote base URL (required)")
	authFlag := fs.String("auth-style", "", "none | bearer | basic | api_key_header")
	headers := headerFlags{}
	fs.Var(headers, "header", "static header template K=V (repeatable)")
	apiKeyHeaderFlag := fs.String("api-key-header", "", "header name for api_key_header auth")
	apiKeyPrefixFlag := fs.String("api-key-prefix", "", "prefix prepended to the resolved secret")
	userFlag := fs.String("username", "", "basic-auth username")
	secretRefFlag := fs.String("secret-ref", "", "secret NAME reference (never the value)")
	statusFlag := fs.String("status", "", "active (default) or disabled")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*nameFlag) == "" || strings.TrimSpace(*typeFlag) == "" || strings.TrimSpace(*baseURLFlag) == "" {
		return usageFail(ctx, "connectors create requires -name, -type and -base-url\nusage: agentosctl connectors create -name NAME -type webhook|http -base-url URL [-auth-style STYLE] [-secret-ref NAME]")
	}
	client := clientFor(ctx)
	conn, err := client.CreateConnector(ctxRun(ctx), sdk.CreateConnectorRequest{
		Name:         *nameFlag,
		Type:         *typeFlag,
		BaseURL:      *baseURLFlag,
		AuthStyle:    *authFlag,
		Headers:      map[string]string(headers),
		APIKeyHeader: *apiKeyHeaderFlag,
		APIKeyPrefix: *apiKeyPrefixFlag,
		Username:     *userFlag,
		SecretRef:    *secretRefFlag,
		Status:       *statusFlag,
	})
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, conn)
	}
	printDetail(ctx.stdout, map[string]string{
		"id":         conn.ID,
		"name":       conn.Name,
		"type":       conn.Type,
		"base url":   conn.BaseURL,
		"auth style": conn.Config.AuthStyle,
		"secret ref": conn.SecretRef,
		"status":     conn.Status,
	})
	return exitOK
}

// connectorsList implements `connectors list` (GET /v1/connectors).
func connectorsList(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "connectors list")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	list, err := client.ListConnectors(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, list)
	}
	rows := make([][]string, 0, len(list.Connectors))
	for _, c := range list.Connectors {
		lastCheck := "never"
		if c.LastCheckAt != nil {
			lastCheck = fmt.Sprintf("%s/%s", c.LastCheckAt.Format(dayLayout), c.LastCheckStatus)
		}
		rows = append(rows, []string{c.ID, c.Name, c.Type, c.BaseURL, c.Status, lastCheck})
	}
	printTable(ctx.stdout, []string{"ID", "NAME", "TYPE", "BASE URL", "STATUS", "LAST CHECK"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d connector(s)\n", len(list.Connectors))
	return exitOK
}

// connectorsTest implements `connectors test ID`
// (POST /v1/connectors/{id}/test).
func connectorsTest(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "connectors test")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	id := popFront(&rest)
	if strings.TrimSpace(id) == "" {
		return usageFail(ctx, "connectors test requires a connector id\nusage: agentosctl connectors test CONNECTOR_ID")
	}
	client := clientFor(ctx)
	res, err := client.TestConnector(ctxRun(ctx), id)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, res)
	}
	printDetail(ctx.stdout, map[string]string{
		"connector":  res.ConnectorID,
		"status":     res.Status,
		"http code":  fmt.Sprintf("%d", res.StatusCode),
		"latency ms": fmt.Sprintf("%d", res.LatencyMS),
	})
	if res.Error != "" {
		fmt.Fprintf(ctx.stdout, "error: %s\n", res.Error)
	}
	return exitOK
}

// connectorsDelete implements `connectors delete ID`
// (DELETE /v1/connectors/{id}).
func connectorsDelete(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "connectors delete")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	id := popFront(&rest)
	if strings.TrimSpace(id) == "" {
		return usageFail(ctx, "connectors delete requires a connector id\nusage: agentosctl connectors delete CONNECTOR_ID")
	}
	client := clientFor(ctx)
	if err := client.DeleteConnector(ctxRun(ctx), id); err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, map[string]bool{"deleted": true})
	}
	fmt.Fprintf(ctx.stdout, "deleted connector %s\n", id)
	return exitOK
}
