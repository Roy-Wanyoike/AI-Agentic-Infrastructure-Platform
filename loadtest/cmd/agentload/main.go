// Command agentload is the AgentOS comprehensive load-testing CLI (issue
// #125). It drives the six load scenarios of the suite against a running
// AgentOS API, records JSON baselines and gates current runs against them:
//
//	agentload run -scenario sustained -base http://127.0.0.1:8080 -d 60s
//	agentload record -o loadtest/baselines/sustained-inmemory.json -d 30s
//	agentload compare baseline.json current.json
//	agentload list
//
// Exit codes: 0 success/pass, 1 baseline regression (compare only), 2 usage
// or setup error. Interrupts (SIGINT/SIGTERM) shut the run down gracefully:
// generation stops, in-flight requests complete, and the partial report is
// still printed and written.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"agentos/loadtest"
)

const usageText = `agentload — AgentOS comprehensive load suite (issue #125)

Usage:

  agentload run -scenario <name> [flags]      run one scenario, print the report
  agentload record -o FILE [flags]            run a scenario and record it as a baseline
  agentload compare BASELINE.json CURRENT.json [-p95-tol 1.10] [-err-tol 0.02]
  agentload list                              list the scenarios

Scenarios: sustained | spike | soak | concurrency | workflow | lifecycle

Common run/record flags (see loadtest/README.md for tuning guidance):

  -base URL              API base URL (default http://127.0.0.1:8080)
  -prefix /v1            API route prefix (/v1 or /api/v1)
  -d 60s                 measurement duration
  -rps 100               target requests/sec of the primary op
  -workers 16            generator pool size / closed-loop concurrency
  -warmup 5s             warmup phase, samples discarded
  -timeout 5s            per-request timeout
  -json                  print the JSON report to stdout (run)
  -out FILE              also write the JSON report to FILE (run)

Exit codes: 0 pass, 1 regression (compare), 2 usage/setup error.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "record":
		cmdRecord(os.Args[2:])
	case "compare":
		cmdCompare(os.Args[2:])
	case "list":
		for _, name := range loadtest.ScenarioNames() {
			fmt.Println(name)
		}
	case "help", "-h", "-help", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
}

// runConfig bundles the flags shared by run and record.
type runConfig struct {
	cfg       *loadtest.Config
	scenario  string
	out       string
	jsonOut   bool
	isRecord  bool
	recordOut string
}

// parseRunFlags registers the run/record flag set. record forces an output
// path (-o) and defaults to the sustained scenario.
func parseRunFlags(args []string, isRecord bool) (*runConfig, error) {
	rc := &runConfig{cfg: loadtest.DefaultConfig(), isRecord: isRecord}
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	if !isRecord {
		fs = flag.NewFlagSet("run", flag.ContinueOnError)
	}
	loadtest.RegisterFlags(fs, rc.cfg)
	fs.StringVar(&rc.scenario, "scenario", "sustained", "scenario to run")
	fs.BoolVar(&rc.jsonOut, "json", false, "print the JSON report to stdout")
	if isRecord {
		fs.StringVar(&rc.recordOut, "o", "", "baseline output path (required)")
	} else {
		fs.StringVar(&rc.out, "out", "", "write the JSON report to this file (optional)")
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if isRecord && rc.recordOut == "" {
		return nil, fmt.Errorf("record requires -o FILE")
	}
	if err := rc.cfg.Validate(rc.scenario); err != nil {
		return nil, err
	}
	if _, err := loadtest.LookupScenario(rc.scenario); err != nil {
		return nil, err
	}
	return rc, nil
}

// runScenario executes one scenario under signal-aware graceful shutdown and
// prints the summary (plus optional JSON output).
func runScenario(rc *runConfig) (*loadtest.Report, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := loadtest.NewEnv(rc.cfg, loadtest.NewPooledClient(rc.cfg.Timeout))
	sc, err := loadtest.LookupScenario(rc.scenario)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "agentload: running %s against %s ...\n", sc.Name(), rc.cfg.Base)
	rep, err := sc.Run(ctx, env)
	if rep != nil {
		if rc.jsonOut {
			_ = rep.WriteJSON(os.Stdout)
		} else {
			rep.WriteSummary(os.Stdout)
		}
		out := rc.out
		if rc.isRecord {
			out = rc.recordOut
		}
		if out != "" {
			f, ferr := os.Create(out)
			if ferr != nil {
				return rep, fmt.Errorf("create %s: %w", out, ferr)
			}
			defer f.Close()
			if jerr := rep.WriteJSON(f); jerr != nil {
				return rep, fmt.Errorf("write %s: %w", out, jerr)
			}
			fmt.Fprintf(os.Stderr, "agentload: report written to %s\n", out)
		}
	}
	return rep, err
}

func cmdRun(args []string) {
	rc, err := parseRunFlags(args, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentload run: %v\n", err)
		os.Exit(2)
	}
	_, err = runScenario(rc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentload run: %v\n", err)
		os.Exit(2)
	}
}

func cmdRecord(args []string) {
	rc, err := parseRunFlags(args, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentload record: %v\n", err)
		os.Exit(2)
	}
	rep, err := runScenario(rc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentload record: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "agentload: baseline captured (%s, %d requests, error_rate %.4f) -> %s\n",
		rep.Scenario, rep.Totals.Requests, rep.Totals.ErrorRate, rc.recordOut)
}

func cmdCompare(args []string) {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	p95Tol := fs.Float64("p95-tol", loadtest.DefaultP95Tolerance, "p95 regression gate (relative)")
	errTol := fs.Float64("err-tol", loadtest.DefaultErrorTolerance, "error-rate gate (absolute percentage points)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: agentload compare BASELINE.json CURRENT.json [-p95-tol 1.10] [-err-tol 0.02]")
		os.Exit(2)
	}
	baseline, err := loadtest.LoadReport(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentload compare: %v\n", err)
		os.Exit(2)
	}
	current, err := loadtest.LoadReport(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentload compare: %v\n", err)
		os.Exit(2)
	}
	regs := loadtest.CompareReports(baseline, current, *p95Tol, *errTol)
	loadtest.WriteComparison(os.Stdout, baseline, current, regs, *p95Tol, *errTol)
	if len(regs) > 0 {
		os.Exit(1)
	}
}
