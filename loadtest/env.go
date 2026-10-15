package loadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// NewPooledClient builds the shared HTTP client every scenario uses: one
// connection pool shared by all worker goroutines (keep-alive, generous
// idle-connection headroom) with a per-request timeout.
func NewPooledClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// Env is the per-run environment handed to scenarios: config, shared client,
// the /metrics sampler factory and tenant provisioning helpers that mirror
// the canonical user journey (register -> login -> create agent -> activate).
type Env struct {
	cfg    *Config
	client *http.Client
	seq    atomic.Int64
}

// NewEnv wires an environment.
func NewEnv(cfg *Config, client *http.Client) *Env {
	return &Env{cfg: cfg, client: client}
}

// Config exposes the run configuration to scenarios.
func (e *Env) Config() *Config { return e.cfg }

// Client exposes the shared pooled client.
func (e *Env) Client() *http.Client { return e.client }

// apiURL joins the configured base and prefix with a path.
func (e *Env) apiURL(path string) string {
	return strings.TrimSuffix(e.cfg.Base, "/") + e.cfg.Prefix + path
}

// plainURL joins the base with an unversioned path (healthz/readyz).
func (e *Env) plainURL(path string) string {
	return strings.TrimSuffix(e.cfg.Base, "/") + path
}

// newRequest builds a request; all load-test URLs derive from fixed config
// prefixes, so a malformed URL is a programming error (panic is unreachable).
func (e *Env) newRequest(ctx context.Context, method, url, authz, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		panic(fmt.Sprintf("loadtest: build %s %s: %v", method, url, err))
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	return req
}

// measure performs one request against the collector: latency is recorded for
// every completed exchange; an unexpected status or transport error counts as
// an error. status is -1 on transport errors.
func (e *Env) measure(ctx context.Context, c *collector, want int, req *http.Request) int {
	t0 := time.Now()
	resp, err := e.client.Do(req)
	latMs := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		c.add(-1, want, latMs)
		return -1
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	status := resp.StatusCode
	_ = resp.Body.Close()
	c.add(status, want, latMs)
	return status
}

// measureJSON performs a request and decodes a JSON body on success.
func (e *Env) measureJSON(ctx context.Context, c *collector, want int, req *http.Request, v any) (int, error) {
	t0 := time.Now()
	resp, err := e.client.Do(req)
	latMs := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		c.add(-1, want, latMs)
		return -1, err
	}
	if resp.StatusCode != want {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		c.add(resp.StatusCode, want, latMs)
		return resp.StatusCode, fmt.Errorf("status %d (want %d)", resp.StatusCode, want)
	}
	err = json.NewDecoder(resp.Body).Decode(v)
	_ = resp.Body.Close()
	c.add(resp.StatusCode, want, latMs)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	return resp.StatusCode, nil
}

// Tenant is one provisioned load-test tenant (organization + owner token).
type Tenant struct {
	Org    string
	Email  string
	Token  string
	Authz  string // "Bearer <token>"
	UserID string
}

// RegisterTenant provisions a fresh tenant through the public API:
// POST /auth/register then POST /auth/login. Setup latencies land in the
// scenario's setup collector (reported but excluded from gating).
func (e *Env) RegisterTenant(ctx context.Context, setup *collector) (*Tenant, error) {
	n := e.seq.Add(1)
	uniq := fmt.Sprintf("%d-%d", time.Now().UnixNano(), n)
	org := "loadorg-" + uniq
	email := "loaduser-" + uniq + "@loadtest.local"

	req := e.newRequest(ctx, http.MethodPost, e.apiURL("/auth/register"), "",
		fmt.Sprintf(`{"organization":%q,"email":%q,"password":%q}`, org, email, e.cfg.Password))
	if _, err := e.measureJSON(ctx, setup, http.StatusCreated, req, &struct{}{}); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}

	login := struct {
		Token string `json:"token"`
	}{}
	req = e.newRequest(ctx, http.MethodPost, e.apiURL("/auth/login"), "",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, e.cfg.Password))
	if _, err := e.measureJSON(ctx, setup, http.StatusOK, req, &login); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	if login.Token == "" {
		return nil, fmt.Errorf("login returned an empty token")
	}
	return &Tenant{Org: org, Email: email, Token: login.Token, Authz: "Bearer " + login.Token}, nil
}

// CreateAgent creates an agent (draft state) and returns its ID. The Agent
// wire shape carries PascalCase field names (no json tags).
func (e *Env) CreateAgent(ctx context.Context, c *collector, t *Tenant, name string) (string, error) {
	req := e.newRequest(ctx, http.MethodPost, e.apiURL("/agents/create"), t.Authz,
		fmt.Sprintf(`{"name":%q,"description":"loadtest","instructions":"echo the input","model":"gpt-4o-mini"}`, name))
	var agent struct {
		ID string `json:"ID"`
	}
	if _, err := e.measureJSON(ctx, c, http.StatusCreated, req, &agent); err != nil {
		return "", fmt.Errorf("create agent: %w", err)
	}
	if agent.ID == "" {
		return "", fmt.Errorf("create agent returned an empty ID")
	}
	return agent.ID, nil
}

// TransitionAgent moves an agent along the lifecycle state machine
// (POST /agents/{id}/transition). target is case-insensitive.
func (e *Env) TransitionAgent(ctx context.Context, c *collector, t *Tenant, agentID, target string) error {
	req := e.newRequest(ctx, http.MethodPost, e.apiURL("/agents/"+agentID+"/transition"), t.Authz,
		fmt.Sprintf(`{"target_state":%q}`, target))
	var agent struct {
		ID string `json:"ID"`
	}
	if _, err := e.measureJSON(ctx, c, http.StatusOK, req, &agent); err != nil {
		return fmt.Errorf("transition agent: %w", err)
	}
	return nil
}

// ActivateAgent walks draft -> building -> active so the agent accepts runs
// (issue #106 lifecycle guard on POST /runs).
func (e *Env) ActivateAgent(ctx context.Context, c *collector, t *Tenant, agentID string) error {
	if err := e.TransitionAgent(ctx, c, t, agentID, "building"); err != nil {
		return err
	}
	return e.TransitionAgent(ctx, c, t, agentID, "active")
}

// CreateRun creates one run on an ACTIVE agent (POST /runs) and returns the
// run id. Latency lands in c.
func (e *Env) CreateRun(ctx context.Context, c *collector, t *Tenant, agentID, input string) (string, error) {
	req := e.newRequest(ctx, http.MethodPost, e.apiURL("/runs"), t.Authz,
		fmt.Sprintf(`{"agent_id":%q,"input":%q}`, agentID, input))
	var run struct {
		RunID string `json:"run_id"`
	}
	if _, err := e.measureJSON(ctx, c, http.StatusCreated, req, &run); err != nil {
		return "", fmt.Errorf("create run: %w", err)
	}
	if run.RunID == "" {
		return "", fmt.Errorf("create run returned an empty run_id")
	}
	return run.RunID, nil
}

// Healthz measures the unversioned /healthz control endpoint (no auth, no
// tenant work — the latency floor control op).
func (e *Env) Healthz(ctx context.Context, c *collector) {
	req := e.newRequest(ctx, http.MethodGet, e.plainURL("/healthz"), "", "")
	e.measure(ctx, c, http.StatusOK, req)
}

// jsonMarshal serializes request bodies (all bodies are built from fixed
// shapes; a marshal failure is a programming error and panics).
func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}
	return string(b), nil
}
