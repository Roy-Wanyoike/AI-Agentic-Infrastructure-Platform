package main

import (
	"fmt"
	"strings"

	"agentos/internal/sdk"
)

// cmdMarketplace dispatches the marketplace subcommands.
func cmdMarketplace(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "marketplace requires a subcommand: search, show, install, publish")
	}
	switch args[0] {
	case "search":
		return marketplaceSearch(ctx, args[1:])
	case "show":
		return marketplaceShow(ctx, args[1:])
	case "install":
		return marketplaceInstall(ctx, args[1:])
	case "publish":
		return marketplacePublish(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, marketplaceUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown marketplace subcommand %q (want search, show, install, publish)", args[0])
	}
}

const marketplaceUsage = `usage: agentosctl marketplace <subcommand> [flags]

  marketplace search                search the global published catalog
      flags: [-q TEXT] [-tags a,b] [-limit N] [-cursor CUR] [-json]
  marketplace show SLUG             show one listing with its config snapshot
      flags: [-json]
  marketplace install SLUG          install the listing as a NEW agent
      flags: [-json]
  marketplace publish               publish one of your agents to the catalog
      flags: -agent AGENT_ID [-version N] [-name NAME] [-slug SLUG]
             [-description TEXT] [-tags a,b] [-status published|draft] [-json]
`

// marketplaceSearch implements `marketplace search`
// (GET /v1/marketplace/listings?q&tags&limit&cursor).
func marketplaceSearch(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "marketplace search")
	qFlag := fs.String("q", "", "text match over name/description")
	tagsFlag := fs.String("tags", "", "comma-separated tag filter (ANY overlap)")
	limitFlag := fs.Int("limit", 0, "page size (0 = server default)")
	cursorFlag := fs.String("cursor", "", "next_cursor from a previous page")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	page, err := client.BrowseListings(ctxRun(ctx), sdk.BrowseOptions{
		Query:  *qFlag,
		Tags:   splitCSV(*tagsFlag),
		Limit:  *limitFlag,
		Cursor: *cursorFlag,
	})
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, page)
	}
	rows := make([][]string, 0, len(page.Listings))
	for _, l := range page.Listings {
		rows = append(rows, []string{
			l.Slug,
			l.Name,
			l.Status,
			fmt.Sprintf("%d", l.DownloadCount),
			strings.Join(l.Tags, ","),
		})
	}
	printTable(ctx.stdout, []string{"SLUG", "NAME", "STATUS", "DOWNLOADS", "TAGS"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d listing(s)\n", len(page.Listings))
	if page.NextCursor != "" {
		fmt.Fprintf(ctx.stdout, "next page: agentosctl marketplace search -cursor %s\n", page.NextCursor)
	}
	return exitOK
}

// marketplaceShow implements `marketplace show SLUG`
// (GET /v1/marketplace/listings/{slug}).
func marketplaceShow(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "marketplace show")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	slug := popFront(&rest)
	if strings.TrimSpace(slug) == "" {
		return usageFail(ctx, "marketplace show requires a listing slug\nusage: agentosctl marketplace show SLUG")
	}
	client := clientFor(ctx)
	listing, err := client.GetListing(ctxRun(ctx), slug)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, listing)
	}
	printDetail(ctx.stdout, map[string]string{
		"slug":         listing.Slug,
		"name":         listing.Name,
		"status":       listing.Status,
		"downloads":    fmt.Sprintf("%d", listing.DownloadCount),
		"tags":         strings.Join(listing.Tags, ","),
		"source agent": listing.SourceAgentID,
		"publisher":    listing.PublisherOrgID,
	})
	if snap := listing.VersionSnapshot; snap != nil {
		printDetail(ctx.stdout, map[string]string{
			"snapshot model":        snap.Model,
			"snapshot instructions": snap.Instructions,
		})
	}
	return exitOK
}

// marketplaceInstall implements `marketplace install SLUG`
// (POST /v1/marketplace/listings/{slug}/install).
func marketplaceInstall(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "marketplace install")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	slug := popFront(&rest)
	if strings.TrimSpace(slug) == "" {
		return usageFail(ctx, "marketplace install requires a listing slug\nusage: agentosctl marketplace install SLUG")
	}
	client := clientFor(ctx)
	res, err := client.InstallListing(ctxRun(ctx), slug)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, res)
	}
	printDetail(ctx.stdout, map[string]string{
		"agent id":   res.Agent.ID,
		"agent name": res.Agent.Name,
		"model":      res.Agent.Model,
		"from slug":  res.Listing.Slug,
	})
	return exitOK
}

// marketplacePublish implements `marketplace publish`
// (POST /v1/marketplace/listings). -agent is required; the slug and
// description are derived server-side when omitted.
func marketplacePublish(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "marketplace publish")
	agentFlag := fs.String("agent", "", "source agent id (required)")
	versionFlag := fs.Int("version", 0, "publish immutable config version N (0 = live config)")
	nameFlag := fs.String("name", "", "listing name (default: agent name)")
	slugFlag := fs.String("slug", "", "listing slug (default: derived from name)")
	descFlag := fs.String("description", "", "listing description")
	tagsFlag := fs.String("tags", "", "comma-separated tags")
	statusFlag := fs.String("status", "", "published (default) or draft")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*agentFlag) == "" {
		return usageFail(ctx, "marketplace publish requires -agent AGENT_ID\nusage: agentosctl marketplace publish -agent AGENT_ID [-name NAME] [-slug SLUG]")
	}
	switch strings.TrimSpace(*statusFlag) {
	case "", "published", "draft":
	default:
		return usageFail(ctx, "-status must be published or draft")
	}
	client := clientFor(ctx)
	listing, err := client.PublishListing(ctxRun(ctx), sdk.PublishListingRequest{
		AgentID:     *agentFlag,
		Version:     *versionFlag,
		Name:        *nameFlag,
		Slug:        *slugFlag,
		Description: *descFlag,
		Tags:        splitCSV(*tagsFlag),
		Status:      *statusFlag,
	})
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, listing)
	}
	printDetail(ctx.stdout, map[string]string{
		"slug":   listing.Slug,
		"name":   listing.Name,
		"status": listing.Status,
	})
	return exitOK
}

// splitCSV splits a comma-separated flag value into trimmed, non-empty
// entries (empty string -> nil so the parameter is omitted entirely).
func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
