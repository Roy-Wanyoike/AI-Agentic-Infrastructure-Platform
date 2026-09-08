// Command loadtest is the AgentOS baseline load harness (issue #80).
//
// It drives the canonical user journey against a running AgentOS API in
// zero-infrastructure (in-memory) mode:
//
//	register -> login -> create-agent -> create-run -> poll run status
//
// Each virtual user repeats that journey until the deadline, recording
// per-step latencies, request throughput and every transport error or
// unexpected status as a scenario error. Results print as a percentile table
// (P50/P95/P99/mean/max) plus totals; -json emits the same data
// machine-readably.
//
// Usage (server must be up; see docs/load-testing.md for the full recipe):
//
//	go run ./scripts/loadtest -base http://127.0.0.1:8080 -vus 8 -d 30s
//
// The harness is pure stdlib so it adds no module dependencies. A poll stops
// early on a terminal status (COMPLETED/FAILED); without a worker process the
// run stays QUEUED in in-memory mode, so polls are capped (-polls) and the
// measured poll latency is the polling read path, not run execution.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Scenario names used as metric keys.
const (
	stepRegister = "register"
	stepLogin    = "login"
	stepAgent    = "create_agent"
	stepRun      = "create_run"
	stepPoll     = "poll_run"
	stepIter     = "iteration" // full journey end-to-end
)

var stepOrder = []string{stepRegister, stepLogin, stepAgent, stepRun, stepPoll, stepIter}

// terminalStatuses stop the poll loop early (with a worker process attached).
var terminalStatuses = map[string]bool{"COMPLETED": true, "FAILED": true}

type config struct {
	base     string
	vus      int
	duration time.Duration
	warmup   time.Duration
	polls    int
	pollWait time.Duration
	timeout  time.Duration
	jsonOut  bool
	prefix   string
	password string
}

// sample accumulates per-step latency samples across all virtual users.
type sample struct {
	mu        sync.Mutex
	latencies map[string][]time.Duration
	errors    map[string]int
}

func newSample() *sample {
	return &sample{
		latencies: make(map[string][]time.Duration),
		errors:    make(map[string]int),
	}
}

func (s *sample) record(step string, d time.Duration) {
	s.mu.Lock()
	s.latencies[step] = append(s.latencies[step], d)
	s.mu.Unlock()
}

func (s *sample) recordErr(step string) {
	s.mu.Lock()
	s.errors[step]++
	s.mu.Unlock()
}

// runner drives one virtual user: HTTP client plus metric recording.
type runner struct {
	cfg    *config
	client *http.Client
	s      *sample
	reqs   *atomic.Int64
	warmup bool
}

// call performs one request and returns the response when both the transport
// and the status code match expectations; otherwise it drains the response,
// records an error for the step, and returns nil. Latency is recorded for
// every completed exchange (success or failure), matching k6's
// http_req_duration semantics.
func (r *runner) call(step string, req *http.Request, want int) *http.Response {
	t0 := time.Now()
	resp, err := r.client.Do(req)
	r.reqs.Add(1)
	elapsed := time.Since(t0)
	if err != nil {
		if !r.warmup {
			r.s.recordErr(step)
			r.s.record(step, elapsed)
		}
		return nil
	}
	if resp.StatusCode != want {
		drain(resp)
		if !r.warmup {
			r.s.recordErr(step)
			r.s.record(step, elapsed)
		}
		return nil
	}
	if !r.warmup {
		r.s.record(step, elapsed)
	}
	return resp
}

func drain(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func (r *runner) decode(step string, resp *http.Response, v any) bool {
	defer drain(resp)
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		if !r.warmup {
			r.s.recordErr(step)
		}
		return false
	}
	return true
}

func (r *runner) jsonRequest(method, url, authz, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		panic(err) // unreachable: all URLs are built from fixed prefixes
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	return req
}

// journey executes one full user journey, recording the end-to-end iteration
// latency; returns true when every step succeeded.
func (r *runner) journey(seq *atomic.Int64) bool {
	iterStart := time.Now()
	ok := r.journeySteps(seq)
	if !r.warmup {
		r.s.record(stepIter, time.Since(iterStart))
		if !ok {
			r.s.recordErr(stepIter)
		}
	}
	return ok
}

// journeySteps performs the five journey steps; returns false at the first
// failed step (all failures are recorded by the step helpers).
func (r *runner) journeySteps(seq *atomic.Int64) bool {
	// Unique org/email per journey keeps tenants isolated like real traffic.
	n := seq.Add(1)
	uniq := fmt.Sprintf("%d-%d", time.Now().UnixNano(), n)
	org := "loadorg-" + uniq
	email := "loaduser-" + uniq + "@loadtest.local"

	// 1. register
	resp := r.call(stepRegister, r.jsonRequest(http.MethodPost, r.cfg.base+r.cfg.prefix+"/auth/register", "",
		fmt.Sprintf(`{"organization":%q,"email":%q,"password":%q}`, org, email, r.cfg.password)), http.StatusCreated)
	if resp == nil || !r.decode(stepRegister, resp, &struct{}{}) {
		return false
	}

	// 2. login
	resp = r.call(stepLogin, r.jsonRequest(http.MethodPost, r.cfg.base+r.cfg.prefix+"/auth/login", "",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, r.cfg.password)), http.StatusOK)
	if resp == nil {
		return false
	}
	var login struct {
		Token string `json:"token"`
	}
	if !r.decode(stepLogin, resp, &login) || login.Token == "" {
		return false
	}
	authz := "Bearer " + login.Token

	// 3. create agent
	resp = r.call(stepAgent, r.jsonRequest(http.MethodPost, r.cfg.base+r.cfg.prefix+"/agents/create", authz,
		`{"name":"load-agent","description":"loadtest","instructions":"echo the input","model":"gpt-4o-mini"}`), http.StatusCreated)
	if resp == nil {
		return false
	}
	var agent struct {
		ID string `json:"ID"` // Agent has no json tags: default field names apply
	}
	if !r.decode(stepAgent, resp, &agent) || agent.ID == "" {
		return false
	}

	// 4. create run
	resp = r.call(stepRun, r.jsonRequest(http.MethodPost, r.cfg.base+r.cfg.prefix+"/runs", authz,
		fmt.Sprintf(`{"agent_id":%q,"input":"2+2"}`, agent.ID)), http.StatusCreated)
	if resp == nil {
		return false
	}
	var run struct {
		RunID string `json:"run_id"`
	}
	if !r.decode(stepRun, resp, &run) || run.RunID == "" {
		return false
	}

	// 5. poll run status until terminal or cap. Without a worker process the
	// run stays QUEUED in in-memory mode; the cap bounds the journey.
	for attempt := 0; attempt < r.cfg.polls; attempt++ {
		time.Sleep(r.cfg.pollWait)
		resp = r.call(stepPoll, r.jsonRequest(http.MethodGet, r.cfg.base+r.cfg.prefix+"/runs/"+run.RunID, authz, ""), http.StatusOK)
		if resp == nil {
			return false
		}
		var got struct {
			Status string `json:"Status"` // Run has no json tags for Status
		}
		if !r.decode(stepPoll, resp, &got) || got.Status == "" {
			return false
		}
		if terminalStatuses[got.Status] {
			break
		}
	}
	return true
}

type stepStats struct {
	Step     string  `json:"step"`
	Requests int     `json:"requests"`
	Errors   int     `json:"errors"`
	P50ms    float64 `json:"p50_ms"`
	P95ms    float64 `json:"p95_ms"`
	P99ms    float64 `json:"p99_ms"`
	MeanMs   float64 `json:"mean_ms"`
	MaxMs    float64 `json:"max_ms"`
}

type report struct {
	Base        string      `json:"base"`
	VUs         int         `json:"vus"`
	DurationSec float64     `json:"duration_s"`
	Iterations  int         `json:"iterations"`
	FailedIters int         `json:"failed_iterations"`
	Requests    int64       `json:"http_requests"`
	RPS         float64     `json:"http_rps"`
	IterRPS     float64     `json:"iteration_rps"`
	Steps       []stepStats `json:"steps"`
}

func buildReport(cfg *config, s *sample, iterations, requests int64, elapsed time.Duration) report {
	rep := report{
		Base:        cfg.base,
		VUs:         cfg.vus,
		DurationSec: elapsed.Seconds(),
		Iterations:  int(iterations),
		Requests:    requests,
	}
	if elapsed > 0 {
		rep.RPS = float64(requests) / elapsed.Seconds()
		rep.IterRPS = float64(iterations) / elapsed.Seconds()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, step := range stepOrder {
		ls := s.latencies[step]
		st := stepStats{Step: step, Requests: len(ls), Errors: s.errors[step]}
		if len(ls) == 0 {
			rep.Steps = append(rep.Steps, st)
			continue
		}
		sort.Slice(ls, func(i, j int) bool { return ls[i] < ls[j] })
		var sum time.Duration
		for _, d := range ls {
			sum += d
		}
		st.P50ms = ms(ls[len(ls)*50/100])
		st.P95ms = ms(ls[len(ls)*95/100])
		st.P99ms = ms(ls[len(ls)*99/100])
		st.MeanMs = ms(sum / time.Duration(len(ls)))
		st.MaxMs = ms(ls[len(ls)-1])
		rep.Steps = append(rep.Steps, st)
	}
	return rep
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

func printTable(w io.Writer, rep report) {
	fmt.Fprintf(w, "AgentOS load test baseline (issue #80)\n")
	fmt.Fprintf(w, "base=%s vus=%d duration=%.1fs\n\n", rep.Base, rep.VUs, rep.DurationSec)
	fmt.Fprintf(w, "%-14s %10s %8s %10s %10s %10s %10s %10s\n",
		"STEP", "REQUESTS", "ERRORS", "P50(ms)", "P95(ms)", "P99(ms)", "MEAN(ms)", "MAX(ms)")
	for _, st := range rep.Steps {
		fmt.Fprintf(w, "%-14s %10d %8d %10.2f %10.2f %10.2f %10.2f %10.2f\n",
			st.Step, st.Requests, st.Errors, st.P50ms, st.P95ms, st.P99ms, st.MeanMs, st.MaxMs)
	}
	fmt.Fprintf(w, "\niterations=%d failed=%d http_requests=%d http_rps=%.1f iteration_rps=%.1f\n",
		rep.Iterations, rep.FailedIters, rep.Requests, rep.RPS, rep.IterRPS)
}

func main() {
	cfg := &config{}
	flag.StringVar(&cfg.base, "base", "http://127.0.0.1:8080", "API base URL")
	flag.IntVar(&cfg.vus, "vus", 8, "concurrent virtual users")
	flag.DurationVar(&cfg.duration, "d", 30*time.Second, "measurement duration")
	flag.DurationVar(&cfg.warmup, "warmup", 5*time.Second, "warmup duration (results discarded)")
	flag.IntVar(&cfg.polls, "polls", 8, "max poll attempts per run")
	flag.DurationVar(&cfg.pollWait, "poll-wait", 20*time.Millisecond, "sleep between polls")
	flag.DurationVar(&cfg.timeout, "timeout", 5*time.Second, "per-request timeout")
	flag.BoolVar(&cfg.jsonOut, "json", false, "print JSON report instead of the table")
	flag.StringVar(&cfg.prefix, "prefix", "/v1", "API route prefix (/v1 or /api/v1)")
	flag.StringVar(&cfg.password, "password", "loadtest-passw0rd", "password for created users")
	flag.Parse()

	s := newSample()
	var iterations, requests atomic.Int64
	client := &http.Client{Timeout: cfg.timeout}
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Warmup phase: same journey, results discarded (JIT-free Go needs no
	// JIT warmup, but the first requests pay map growth / connection setup).
	if cfg.warmup > 0 {
		warmDeadline := time.Now().Add(cfg.warmup)
		for i := 0; i < cfg.vus; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				r := &runner{cfg: cfg, client: client, s: newSample(), reqs: &requests, warmup: true}
				seq := new(atomic.Int64)
				seq.Store(int64(id) * 1_000_000)
				for time.Now().Before(warmDeadline) {
					r.journey(seq)
				}
			}(i)
		}
		wg.Wait()
	}

	// Measurement phase.
	start := time.Now()
	for i := 0; i < cfg.vus; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r := &runner{cfg: cfg, client: client, s: s, reqs: &requests, warmup: false}
			seq := new(atomic.Int64)
			seq.Store(int64(id) * 1_000_000)
			for {
				select {
				case <-stop:
					return
				default:
				}
				r.journey(seq)
				iterations.Add(1)
			}
		}(i)
	}
	time.Sleep(cfg.duration)
	close(stop)
	wg.Wait()
	elapsed := time.Since(start)

	rep := buildReport(cfg, s, iterations.Load(), requests.Load(), elapsed)
	// Failed iterations are tracked per-journey through stepIter errors; the
	// count doubles as the failure total for the exit-code gate.
	rep.FailedIters = s.errors[stepIter]

	if cfg.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(os.Stderr, "encode report: %v\n", err)
			os.Exit(1)
		}
	} else {
		printTable(os.Stdout, rep)
	}
	// Non-zero exit when any journey failed so CI can gate on it.
	if rep.FailedIters > 0 {
		os.Exit(2)
	}
}
