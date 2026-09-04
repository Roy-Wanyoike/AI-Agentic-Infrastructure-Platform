package main

import (
	"fmt"
	"strings"

	"agentos/internal/sdk"
)

// EnvSCIMToken carries a dedicated scim_ bearer credential for the SCIM
// 2.0 surface. Unlike the other AGENTOS_* variables it is NEVER persisted
// into the config file: the API's RequireSCIMToken middleware accepts ONLY
// this credential class on /scim/v2/* (session tokens and API keys are
// deliberately rejected), and directory credentials should not outlive the
// shell that minted them.
const EnvSCIMToken = "AGENTOS_SCIM_TOKEN"

// cmdSSO dispatches the sso subcommands (SSO connections + SCIM directory).
func cmdSSO(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "sso requires a subcommand: list, login, token")
	}
	switch args[0] {
	case "list":
		return ssoList(ctx, args[1:])
	case "login":
		return ssoLogin(ctx, args[1:])
	case "token":
		return ssoToken(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, ssoUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown sso subcommand %q (want list, login, token)", args[0])
	}
}

const ssoUsage = `usage: agentosctl sso <subcommand> [flags]

  sso list                          list directory identities (SCIM users)
      flags: [-filter 'userName eq "x@y.z"'] [-scim-token SCIM_...] [-json]
  sso login ORG_SLUG                resolve the IdP authorize URL for the
                                    org's SSO connection (prints it; the
                                    browser flow happens outside the CLI)
      flags: [-json]
  sso token                         mint a SCIM bearer credential (OWNER;
                                    the secret is shown EXACTLY ONCE)
      flags: [-json]
`

// scimTokenFor resolves the scim_ credential: -scim-token wins over
// AGENTOS_SCIM_TOKEN (the credential is never read from the config file).
func scimTokenFor(flagValue string) string {
	return firstNonEmpty(flagValue, envValue(EnvSCIMToken))
}

// ssoList implements `sso list` (GET /v1/scim/v2/Users). The SCIM surface
// accepts ONLY the dedicated scim_ bearer credential, so this call runs on a
// dedicated client whose token IS that credential — the platform session/API
// key would be rejected server-side by design.
func ssoList(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "sso list")
	filterFlag := fs.String("filter", "", `SCIM filter: 'userName eq "user@org.tld"'`)
	tokenFlag := fs.String("scim-token", "", "scim_ bearer credential (or "+EnvSCIMToken+")")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	scimToken := scimTokenFor(*tokenFlag)
	if strings.TrimSpace(scimToken) == "" {
		return usageFail(ctx, "sso list requires a scim_ credential: pass -scim-token or set %s (mint one with `agentosctl sso token`)", EnvSCIMToken)
	}
	scimClient := sdk.New(
		sdk.WithBaseURL(ctx.cfg.URL),
		sdk.WithToken(scimToken),
	)
	list, err := scimClient.ListSCIMUsers(ctxRun(ctx), *filterFlag)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, list)
	}
	rows := make([][]string, 0, len(list.Resources))
	for _, u := range list.Resources {
		emails := make([]string, 0, len(u.Emails))
		for _, e := range u.Emails {
			emails = append(emails, e.Value)
		}
		rows = append(rows, []string{
			u.UserName,
			u.ID,
			fmt.Sprintf("%t", u.Active),
			strings.Join(emails, ","),
		})
	}
	printTable(ctx.stdout, []string{"USERNAME", "ID", "ACTIVE", "EMAILS"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d identity(ies) (totalResults=%d)\n", len(list.Resources), list.TotalResults)
	return exitOK
}

// ssoLogin implements `sso login ORG_SLUG`
// (GET /v1/auth/sso/{org_slug}/login). The API answers with a 302 to the
// IdP authorize URL; the CLI prints that URL instead of following it — the
// login itself is a browser flow and yields a session token via the
// callback, not a CLI-held secret.
func ssoLogin(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "sso login")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	slug := popFront(&rest)
	if strings.TrimSpace(slug) == "" {
		return usageFail(ctx, "sso login requires an organization login slug\nusage: agentosctl sso login ORG_SLUG")
	}
	client := clientFor(ctx)
	authorizeURL, err := client.BeginSSOLogin(ctxRun(ctx), slug)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, map[string]string{
			"org_slug":      slug,
			"authorize_url": authorizeURL,
		})
	}
	fmt.Fprintf(ctx.stdout, "open this URL to complete the SSO login for %s:\n%s\n", slug, authorizeURL)
	return exitOK
}

// ssoToken implements `sso token` (POST /v1/scim/tokens, OWNER only). The
// plaintext scim_ secret is printed ONCE behind an explicit warning line.
func ssoToken(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "sso token")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	minted, err := client.MintSCIMToken(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, minted)
	}
	printDetail(ctx.stdout, map[string]string{
		"id":       minted.Token.ID,
		"org":      minted.Token.OrganizationID,
		"created":  minted.Token.CreatedAt.Format(dayLayout),
		"use with": "-scim-token / " + EnvSCIMToken,
	})
	fmt.Fprintf(ctx.stdout, "warning: the SCIM token below is shown EXACTLY ONCE — store it now\n")
	fmt.Fprintln(ctx.stdout, minted.Secret)
	return exitOK
}
