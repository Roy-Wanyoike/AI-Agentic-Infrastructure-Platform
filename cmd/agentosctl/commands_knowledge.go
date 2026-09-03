package main

import (
	"fmt"
	"os"
	"strings"

	"agentos/internal/sdk"
)

// cmdKnowledge dispatches the knowledge subcommands.
func cmdKnowledge(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "knowledge requires a subcommand: search, add, list")
	}
	switch args[0] {
	case "search":
		return knowledgeSearch(ctx, args[1:])
	case "add":
		return knowledgeAdd(ctx, args[1:])
	case "list":
		return knowledgeList(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, knowledgeUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown knowledge subcommand %q (want search, add, list)", args[0])
	}
}

const knowledgeUsage = `usage: agentosctl knowledge <subcommand> [flags]

  knowledge search QUERY            top-k chunk retrieval over the org index
      flags: [-k 5] [-json]
  knowledge add                     ingest a document (chunk -> embed -> store)
      flags: -title TITLE (-content TEXT | -content-file FILE) [-source SRC] [-json]
  knowledge list                    list ingested documents
      flags: [-json]
`

// knowledgeSearch implements `knowledge search QUERY` (POST /v1/knowledge/search).
func knowledgeSearch(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "knowledge search")
	kFlag := fs.Int("k", 5, "number of results")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	query := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(query) == "" {
		return usageFail(ctx, "knowledge search requires a query\nusage: agentosctl knowledge search QUERY [-k 5]")
	}
	client := clientFor(ctx)
	res, err := client.Search(ctxRun(ctx), query, *kFlag)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, res)
	}
	rows := make([][]string, 0, len(res.Results))
	for _, r := range res.Results {
		rows = append(rows, []string{
			fmt.Sprintf("%.4f", r.Score),
			r.DocumentTitle,
			fmt.Sprintf("%d", r.ChunkOrdinal),
			r.Content,
			r.Citation,
		})
	}
	printTable(ctx.stdout, []string{"SCORE", "DOCUMENT", "CHUNK", "CONTENT", "CITATION"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d result(s)\n", len(res.Results))
	return exitOK
}

// knowledgeAdd implements `knowledge add` (POST /v1/knowledge/documents).
func knowledgeAdd(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "knowledge add")
	titleFlag := fs.String("title", "", "document title (required)")
	contentFlag := fs.String("content", "", "document content (or -content-file)")
	contentFile := fs.String("content-file", "", "read the content from a file")
	sourceFlag := fs.String("source", "", "document source label")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	content := *contentFlag
	if content == "" && *contentFile != "" {
		raw, err := os.ReadFile(*contentFile)
		if err != nil {
			return fail(ctx, err)
		}
		content = string(raw)
	}
	if strings.TrimSpace(*titleFlag) == "" || strings.TrimSpace(content) == "" {
		return usageFail(ctx, "knowledge add requires -title and content (-content or -content-file)")
	}
	client := clientFor(ctx)
	res, err := client.AddDocument(ctxRun(ctx), sdk.AddDocumentRequest{
		Title:   *titleFlag,
		Content: content,
		Source:  *sourceFlag,
	})
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, res)
	}
	printDetail(ctx.stdout, map[string]string{
		"id":     res.Document.ID,
		"title":  res.Document.Title,
		"chunks": fmt.Sprintf("%d", res.Document.ChunkCount),
		"source": res.Document.Source,
	})
	if res.Warning != "" {
		fmt.Fprintf(ctx.stdout, "warning: %s\n", res.Warning)
	}
	return exitOK
}

// knowledgeList implements `knowledge list` (GET /v1/knowledge/documents).
func knowledgeList(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "knowledge list")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	list, err := client.ListDocuments(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, list)
	}
	rows := make([][]string, 0, len(list.Documents))
	for _, d := range list.Documents {
		rows = append(rows, []string{d.ID, d.Title, fmt.Sprintf("%d", d.ChunkCount), d.Source})
	}
	printTable(ctx.stdout, []string{"ID", "TITLE", "CHUNKS", "SOURCE"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d document(s)\n", len(list.Documents))
	return exitOK
}
