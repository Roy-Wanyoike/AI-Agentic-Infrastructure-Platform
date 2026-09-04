package main

import (
	"fmt"
	"strings"
)

// cmdBilling dispatches the billing subcommands.
func cmdBilling(ctx *cliContext, args []string) int {
	if len(args) == 0 {
		return usageFail(ctx, "billing requires a subcommand: show, plans, invoices")
	}
	switch args[0] {
	case "show":
		return billingShow(ctx, args[1:])
	case "plans":
		return billingPlans(ctx, args[1:])
	case "invoices":
		return billingInvoices(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(ctx.stdout, billingUsage)
		return exitOK
	default:
		return usageFail(ctx, "unknown billing subcommand %q (want show, plans, invoices)", args[0])
	}
}

const billingUsage = `usage: agentosctl billing <subcommand> [flags]

  billing show                      current subscription + run-quota snapshot
      flags: [-json]
  billing plans                     list the global plan catalog
      flags: [-json]
  billing invoices                  list invoices; -id shows one with its lines
      flags: [-id INVOICE_ID] [-json]
`

// billingShow implements `billing show` (GET /v1/billing/subscription).
func billingShow(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "billing show")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	status, err := client.GetSubscription(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, status)
	}
	sub := status.Subscription
	canceled := "no"
	if sub.CancelAtPeriodEnd {
		canceled = "at period end"
	}
	if sub.CanceledAt != nil {
		canceled = sub.CanceledAt.Format(dayLayout)
	}
	printDetail(ctx.stdout, map[string]string{
		"subscription": sub.ID,
		"plan":         sub.PlanID,
		"status":       sub.Status,
		"period":       fmt.Sprintf("%s .. %s", sub.PeriodStart.Format(dayLayout), sub.PeriodEnd.Format(dayLayout)),
		"cancel":       canceled,
	})
	quota := status.Quota
	included := fmt.Sprintf("%d", quota.IncludedRuns)
	remaining := fmt.Sprintf("%d", quota.RemainingRuns)
	if quota.Unlimited {
		included = "unlimited"
		remaining = "unlimited"
	}
	printDetail(ctx.stdout, map[string]string{
		"quota included":  included,
		"quota consumed":  fmt.Sprintf("%d", quota.ConsumedRuns),
		"quota remaining": remaining,
		"quota exceeded":  fmt.Sprintf("%t", quota.Exceeded),
	})
	return exitOK
}

// billingPlans implements `billing plans` (GET /v1/billing/plans).
func billingPlans(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "billing plans")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	list, err := client.ListPlans(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, list)
	}
	rows := make([][]string, 0, len(list.Plans))
	for _, p := range list.Plans {
		included := fmt.Sprintf("%d", p.IncludedQuota)
		if p.IncludedQuota == 0 {
			included = "unlimited"
		}
		rows = append(rows, []string{
			p.ID,
			p.Name,
			fmt.Sprintf("%d", p.PriceCents),
			p.Currency,
			included,
		})
	}
	printTable(ctx.stdout, []string{"ID", "NAME", "PRICE CENTS", "CURRENCY", "INCLUDED RUNS"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d plan(s)\n", len(list.Plans))
	return exitOK
}

// billingInvoices implements `billing invoices` (GET /v1/billing/invoices;
// -id INVOICE_ID switches to GET /v1/billing/invoices/{id} with its lines).
func billingInvoices(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "billing invoices")
	idFlag := fs.String("id", "", "show one invoice with its line items")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	client := clientFor(ctx)
	if id := strings.TrimSpace(*idFlag); id != "" {
		inv, err := client.GetInvoice(ctxRun(ctx), id)
		if err != nil {
			return fail(ctx, err)
		}
		if ctx.json || *jsonFlag {
			return printJSON(ctx.stdout, inv)
		}
		printDetail(ctx.stdout, map[string]string{
			"id":       inv.ID,
			"status":   inv.Status,
			"period":   fmt.Sprintf("%s .. %s", inv.PeriodStart.Format(dayLayout), inv.PeriodEnd.Format(dayLayout)),
			"subtotal": fmt.Sprintf("%d %s", inv.SubtotalCents, inv.Currency),
		})
		rows := make([][]string, 0, len(inv.Lines))
		for _, line := range inv.Lines {
			rows = append(rows, []string{
				line.Source,
				line.Description,
				fmt.Sprintf("%d", line.Quantity),
				fmt.Sprintf("%d", line.AmountCents),
			})
		}
		fmt.Fprintln(ctx.stdout)
		printTable(ctx.stdout, []string{"SOURCE", "DESCRIPTION", "QTY", "AMOUNT CENTS"}, rows)
		fmt.Fprintf(ctx.stdout, "\n%d line(s)\n", len(inv.Lines))
		return exitOK
	}
	list, err := client.ListInvoices(ctxRun(ctx))
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, list)
	}
	rows := make([][]string, 0, len(list.Invoices))
	for _, inv := range list.Invoices {
		rows = append(rows, []string{
			inv.ID,
			inv.PeriodStart.Format(dayLayout),
			inv.PeriodEnd.Format(dayLayout),
			fmt.Sprintf("%d", inv.SubtotalCents),
			inv.Currency,
			inv.Status,
		})
	}
	printTable(ctx.stdout, []string{"ID", "PERIOD START", "PERIOD END", "SUBTOTAL CENTS", "CURRENCY", "STATUS"}, rows)
	fmt.Fprintf(ctx.stdout, "\n%d invoice(s)\n", len(list.Invoices))
	return exitOK
}
