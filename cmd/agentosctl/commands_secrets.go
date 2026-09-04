package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"agentos/internal/sdk"
)

// cmdSecrets dispatches the secrets subcommands.
func cmdSecrets(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "secrets requires a subcommand: create, list, reveal, delete")
	}
	switch args[0] {
	case "create":
		return secretsCreate(ctx, args[1:])
	case "list":
		return secretsList(ctx, args[1:])
	case "reveal":
		return secretsReveal(ctx, args[1:])
	case "delete":
		return secretsDelete(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, secretsUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown secrets subcommand %q (want create, list, reveal, delete)", args[0])
	}
}

const secretsUsage = `usage: agentosctl secrets <subcommand> [flags]

  secrets create                    seal and store a new org-scoped secret
      flags: -name NAME (-value TEXT | -value-file FILE) [-json]
  secrets list                      list secret metadata (never values)
      flags: [-json]
  secrets reveal NAME               print the value — shown EXACTLY ONCE
      flags: [-json]
  secrets delete NAME               soft-delete (tombstone) a secret
      flags: [-json]
`

// secretsCreate implements `secrets create` (POST /v1/secrets). The value is
// consumed exactly once server-side and never echoed back.
func secretsCreate(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "secrets create")
	nameFlag := fs.String("name", "", "secret name (required)")
	valueFlag := fs.String("value", "", "secret value (or -value-file)")
	valueFile := fs.String("value-file", "", "read the value from a file (use - for stdin)")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	value := *valueFlag
	if value == "" && *valueFile != "" {
		raw, err := readSecretValue(*valueFile)
		if err != nil {
			return fail(ctx, err)
		}
		value = raw
	}
	if strings.TrimSpace(*nameFlag) == "" || value == "" {
		return usageFail(ctx, "secrets create requires -name and a value (-value or -value-file)\nusage: agentosctl secrets create -name NAME (-value TEXT | -value-file FILE)")
	}
	client := clientFor(ctx)
	meta, err := client.CreateSecret(ctxRun(ctx), sdk.CreateSecretRequest{
		Name:  *nameFlag,
		Value: value,
	})
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, meta)
	}
	printDetail(ctx.stdout, map[string]string{
		"name":        meta.Name,
		"key version": fmt.Sprintf("%d", meta.KeyVersion),
		"created by":  meta.CreatedBy,
		"note":        "value stored (never echoed back)",
	})
	return exitOK
}

// readSecretValue reads the secret value from a file or stdin ("-"), with
// no echoing or logging anywhere.
func readSecretValue(path string) (string, error) {
	if path == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(raw), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// secretsList implements `secrets list` (GET /v1/secrets) — metadata ONLY.
func secretsList(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "secrets list")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	list, err := client.ListSecrets(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, list)
	}
	rows := make([][]string, 0, len(list.Secrets))
	for _, s := range list.Secrets {
		rows = append(rows, []string{s.Name, fmt.Sprintf("%d", s.KeyVersion), s.CreatedBy, s.CreatedAt.Format(dayLayout)})
	}
	printTable(ctx.stdout, []string{"NAME", "KEY VERSION", "CREATED BY", "CREATED AT"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d secret(s)\n", len(list.Secrets))
	return exitOK
}

// secretsReveal implements `secrets reveal NAME`
// (POST /v1/secrets/{name}/reveal). The value is printed ONCE, preceded by
// an explicit warning line; the server will never hand it out again.
func secretsReveal(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "secrets reveal")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	name := popFront(&rest)
	if strings.TrimSpace(name) == "" {
		return usageFail(ctx, "secrets reveal requires a secret name\nusage: agentosctl secrets reveal NAME")
	}
	client := clientFor(ctx)
	secret, err := client.RevealSecret(ctxRun(ctx), name)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, secret)
	}
	fmt.Fprintf(ctx.stdout, "warning: the value of %q is revealed EXACTLY ONCE — copy it now\n", secret.Name)
	fmt.Fprintln(ctx.stdout, secret.Value)
	return exitOK
}

// secretsDelete implements `secrets delete NAME` (DELETE /v1/secrets/{name}).
func secretsDelete(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "secrets delete")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	name := popFront(&rest)
	if strings.TrimSpace(name) == "" {
		return usageFail(ctx, "secrets delete requires a secret name\nusage: agentosctl secrets delete NAME")
	}
	client := clientFor(ctx)
	if err := client.DeleteSecret(ctxRun(ctx), name); err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, map[string]bool{"deleted": true})
	}
	fmt.Fprintf(ctx.stdout, "deleted secret %s\n", name)
	return exitOK
}
