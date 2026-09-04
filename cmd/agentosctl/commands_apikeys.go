package main

import (
	"fmt"
	"strings"

	"agentos/internal/sdk"
)

// cmdAPIKeys dispatches the api-keys subcommands.
func cmdAPIKeys(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "api-keys requires a subcommand: create, list, revoke")
	}
	switch args[0] {
	case "create":
		return apiKeysCreate(ctx, args[1:])
	case "list":
		return apiKeysList(ctx, args[1:])
	case "revoke":
		return apiKeysRevoke(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, apiKeysUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown api-keys subcommand %q (want create, list, revoke)", args[0])
	}
}

const apiKeysUsage = `usage: agentosctl api-keys <subcommand> [flags]

  api-keys create                   mint a key — the value is shown EXACTLY ONCE
      flags: -name NAME [-json]
  api-keys list                     list key metadata (never values)
      flags: [-json]
  api-keys revoke KEY_ID            revoke a key (idempotent)
      flags: [-json]
`

// apiKeysCreate implements `api-keys create` (POST /v1/api-keys). The
// plaintext value is returned exactly once by the API; the CLI prints it
// once behind an explicit warning line and nothing else ever shows it.
func apiKeysCreate(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "api-keys create")
	nameFlag := fs.String("name", "", "key name (required)")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*nameFlag) == "" {
		return usageFail(ctx, "api-keys create requires -name NAME\nusage: agentosctl api-keys create -name NAME")
	}
	client := clientFor(ctx)
	created, err := client.CreateAPIKey(ctxRun(ctx), sdk.CreateAPIKeyRequest{Name: *nameFlag})
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, created)
	}
	printDetail(ctx.stdout, map[string]string{
		"id":      created.APIKey.ID,
		"name":    created.APIKey.Name,
		"prefix":  created.APIKey.Prefix,
		"created": created.APIKey.CreatedAt.Format(dayLayout),
	})
	fmt.Fprintf(ctx.stdout, "warning: the key value below is shown EXACTLY ONCE — copy it now\n")
	fmt.Fprintln(ctx.stdout, created.Value)
	return exitOK
}

// apiKeysList implements `api-keys list` (GET /v1/api-keys) — metadata only,
// newest first.
func apiKeysList(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "api-keys list")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	list, err := client.ListAPIKeys(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, list)
	}
	rows := make([][]string, 0, len(list.APIKeys))
	for _, k := range list.APIKeys {
		rows = append(rows, []string{
			k.ID,
			k.Name,
			k.Prefix,
			k.CreatedBy,
			fmt.Sprintf("%t", k.Revoked),
			k.CreatedAt.Format(dayLayout),
		})
	}
	printTable(ctx.stdout, []string{"ID", "NAME", "PREFIX", "CREATED BY", "REVOKED", "CREATED AT"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d key(s)\n", len(list.APIKeys))
	return exitOK
}

// apiKeysRevoke implements `api-keys revoke ID` (DELETE /v1/api-keys/{id}).
func apiKeysRevoke(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "api-keys revoke")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	id := popFront(&rest)
	if strings.TrimSpace(id) == "" {
		return usageFail(ctx, "api-keys revoke requires a key id\nusage: agentosctl api-keys revoke KEY_ID")
	}
	client := clientFor(ctx)
	if err := client.RevokeAPIKey(ctxRun(ctx), id); err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, map[string]bool{"revoked": true})
	}
	fmt.Fprintf(ctx.stdout, "revoked api key %s\n", id)
	return exitOK
}
