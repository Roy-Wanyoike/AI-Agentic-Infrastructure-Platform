package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient wires a client to an httptest server handled by h.
func newTestClient(t *testing.T, h http.HandlerFunc, opts ...Option) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	all := append([]Option{WithBaseURL(srv.URL)}, opts...)
	return New(all...), srv
}

// ---------- Agents (bare array + bare object envelopes) ----------

func TestListAgentsBareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents" {
			t.Errorf("path = %q", r.URL.Path)
		}
		// The handler encodes the slice directly: a bare array, not an envelope.
		_, _ = w.Write([]byte(`[{"ID":"a-1","OrganizationID":"org-1","Name":"Support","Instructions":"hi","Model":"gpt-4o-mini","Status":"DRAFT","CurrentVersionID":"v1","CreatedAt":"2025-01-01T12:00:00Z","UpdatedAt":"2025-01-01T12:00:00Z"}]`))
	}))
	defer srv.Close()

	agents, err := New(WithBaseURL(srv.URL)).ListAgents(context.Background(), "")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].ID != "a-1" || agents[0].Status != "DRAFT" || agents[0].Model != "gpt-4o-mini" {
		t.Errorf("agents = %+v", agents)
	}
}

func TestListAgentsEmptyArrayStaysSlice(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	agents, err := c.ListAgents(context.Background(), "")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if agents == nil || len(agents) != 0 {
		t.Errorf("want non-nil empty slice, got %#v", agents)
	}
}

func TestCreateAgentBareObject(t *testing.T) {
	var reqBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/agents/create" {
			t.Errorf("request line = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ID":"a-9","Name":"Researcher","Status":"DRAFT","CurrentVersionID":"version-a-9-1"}`))
	})
	agent, err := c.CreateAgent(context.Background(), CreateAgentRequest{
		Name: "Researcher", Instructions: "do things", Model: "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if agent.ID != "a-9" || agent.Status != "DRAFT" {
		t.Errorf("agent = %+v", agent)
	}
	if reqBody["name"] != "Researcher" || reqBody["model"] != "gpt-4o-mini" {
		t.Errorf("request body = %v", reqBody)
	}
}

func TestGetAgent(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != "/v1/agents/a%2Fb" {
			t.Errorf("path ids should be escaped, got %q", r.RequestURI)
		}
		_, _ = w.Write([]byte(`{"ID":"a/b","Name":"x"}`))
	})
	agent, err := c.GetAgent(context.Background(), "a/b")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if agent.ID != "a/b" {
		t.Errorf("agent = %+v", agent)
	}
}

// ---------- Runs (wrapped envelopes + SSE) ----------

func TestCreateRunWrappedResponse(t *testing.T) {
	var reqBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs" {
			t.Errorf("request line = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run_id":"run-1","status":"queued"}`))
	})
	res, err := c.CreateRun(context.Background(), CreateRunRequest{AgentID: "a-1", Input: "hello"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if res.RunID != "run-1" || res.Status != "queued" {
		t.Errorf("res = %+v", res)
	}
	if reqBody["agent_id"] != "a-1" || reqBody["input"] != "hello" {
		t.Errorf("request body = %v", reqBody)
	}
}

func TestGetRunBareObject(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ID":"run-1","OrganizationID":"org-1","AgentID":"a-1","Input":"2+2","Output":"4","Status":"COMPLETED","total_cost_cents":3.5,"CreatedAt":"2025-01-01T12:00:00Z","UpdatedAt":"2025-01-01T12:00:05Z"}`))
	})
	run, err := c.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != RunStatusCompleted || run.TotalCostCents != 3.5 || run.ID != "run-1" {
		t.Errorf("run = %+v", run)
	}
}

func TestListRunsWrappedEnvelope(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// listRunsHandler wraps: {"runs":[…]} — NOT a bare array.
		_, _ = w.Write([]byte(`{"runs":[{"ID":"r1","Status":"QUEUED"},{"ID":"r2","Status":"RUNNING"}]}`))
	})
	list, err := c.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(list.Runs) != 2 || list.Runs[1].Status != RunStatusRunning {
		t.Errorf("list = %+v", list)
	}
}

func TestListRunsEmptyBecomesSlice(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"runs":[]}`))
	})
	list, err := c.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if list.Runs == nil {
		t.Error("want non-nil empty Runs slice")
	}
}

func TestStepsWrappedEnvelope(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/run-1/steps" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"run_id":"run-1","steps":[{"ID":"s1","RunID":"run-1","StepType":"model","Status":"succeeded","InputMeta":{"index":1},"OutputMeta":{"output":"4"},"TokenUsage":{"total_tokens":42},"Cost":1.5}]}`))
	})
	steps, err := c.Steps(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if steps.RunID != "run-1" || len(steps.Steps) != 1 {
		t.Fatalf("steps = %+v", steps)
	}
	s := steps.Steps[0]
	if s.StepType != "model" || s.TokenUsage["total_tokens"].(float64) != 42 || s.Cost != 1.5 {
		t.Errorf("step = %+v", s)
	}
}

func TestStreamEventsSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/run-1/events" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// history event + comment + live event with a multi-line data field
		_, _ = w.Write([]byte("data: {\"run_id\":\"run-1\",\"type\":\"status.changed\",\"name\":\"RUNNING\",\"payload\":{\"status\":\"RUNNING\"},\"created_at\":\"2025-01-01T12:00:00Z\"}\n\n"))
		_, _ = w.Write([]byte(": keepalive\n\n"))
		_, _ = w.Write([]byte("data: {\"run_id\":\"run-1\",\"type\":\"status.changed\",\n"))
		_, _ = w.Write([]byte("data:  \"name\":\"COMPLETED\",\"payload\":{\"status\":\"COMPLETED\"},\"created_at\":\"2025-01-01T12:00:05Z\"}\n\n"))
		flusher.Flush()
	}))
	defer srv.Close()

	stream, err := New(WithBaseURL(srv.URL)).StreamEvents(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	defer stream.Close()

	first, err := stream.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if first.Name != "RUNNING" || first.Type != "status.changed" || first.Payload["status"] != "RUNNING" {
		t.Errorf("first = %+v", first)
	}
	second, err := stream.Next()
	if err != nil {
		t.Fatalf("Next (multi-line): %v", err)
	}
	if second.Name != "COMPLETED" {
		t.Errorf("second = %+v (multi-line data join failed)", second)
	}
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

func TestStreamEventsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "run not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := New(WithBaseURL(srv.URL)).StreamEvents(context.Background(), "missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Fatalf("want 404 *APIError, got %v", err)
	}
}

// ---------- Workflows (wrapped envelopes + 422 arrays) ----------

const workflowDetailJSON = `{"workflow":{"id":"wf-1","name":"Pipeline","status":"draft","current_version":0,` +
	`"created_at":"2025-01-01T12:00:00Z","updated_at":"2025-01-01T12:00:00Z",` +
	`"dsl":{"nodes":[{"id":"n1","type":"agent","config":{"agent_id":"a-1"}}],"edges":[]}}}`

func TestCreateWorkflowWrapped(t *testing.T) {
	var reqBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/workflows/create" {
			t.Errorf("request line = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(workflowDetailJSON))
	})
	res, err := c.CreateWorkflow(context.Background(), CreateWorkflowRequest{
		Name: "Pipeline",
		DSL:  DSL{Nodes: []Node{{ID: "n1", Type: "agent", Config: map[string]any{"agent_id": "a-1"}}}},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	if res.Workflow.ID != "wf-1" || len(res.Workflow.DSL.Nodes) != 1 || res.Workflow.DSL.Nodes[0].Type != "agent" {
		t.Errorf("res = %+v", res)
	}
	if reqBody["name"] != "Pipeline" {
		t.Errorf("request body = %v", reqBody)
	}
}

func TestListWorkflowsWrapped(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"workflows":[{"id":"wf-1","name":"P","status":"published","current_version":2}]}`))
	})
	list, err := c.ListWorkflows(context.Background())
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(list.Workflows) != 1 || list.Workflows[0].CurrentVersion != 2 {
		t.Errorf("list = %+v", list)
	}
}

func TestGetWorkflowWrapped(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/workflows/wf-1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(workflowDetailJSON))
	})
	wf, err := c.GetWorkflow(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.Name != "Pipeline" || wf.Status != "draft" {
		t.Errorf("wf = %+v", wf)
	}
}

func TestExecuteWorkflow(t *testing.T) {
	var reqBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/workflows/wf-1/execute" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		_, _ = w.Write([]byte(`{"workflow_run_id":"wr-1","run_ids":["r1","r2"],"status":"pending"}`))
	})
	res, err := c.ExecuteWorkflow(context.Background(), "wf-1", "deploy")
	if err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	if res.WorkflowRunID != "wr-1" || len(res.RunIDs) != 2 || res.Status != "pending" {
		t.Errorf("res = %+v", res)
	}
	if reqBody["input"] != "deploy" {
		t.Errorf("request body = %v", reqBody)
	}
}

func TestGetWorkflowRun(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"wr-1","workflow_id":"wf-1","status":"pending","node_runs":[{"id":"nr-1","workflow_run_id":"wr-1","node_id":"n1","run_id":"r1","status":"pending","attempt":0,"created_at":"2025-01-01T12:00:00Z"}]}`))
	})
	wr, err := c.GetWorkflowRun(context.Background(), "wr-1")
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if wr.Status != "pending" || len(wr.NodeRuns) != 1 || wr.NodeRuns[0].NodeID != "n1" {
		t.Errorf("wr = %+v", wr)
	}
}

// ---------- Knowledge ----------

func TestKnowledgeSearch(t *testing.T) {
	var reqBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/knowledge/search" {
			t.Errorf("request line = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		_, _ = w.Write([]byte(`{"results":[{"document_id":"d1","document_title":"Doc","chunk_id":"c1","chunk_ordinal":0,"content":"text","score":0.9,"citation":"Doc p.1"}]}`))
	})
	res, err := c.Search(context.Background(), "find text", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Results) != 1 || res.Results[0].Score != 0.9 || res.Results[0].DocumentTitle != "Doc" {
		t.Errorf("res = %+v", res)
	}
	if reqBody["query"] != "find text" || reqBody["k"].(float64) != 5 {
		t.Errorf("request body = %v", reqBody)
	}
}

func TestKnowledgeAddAndList(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/knowledge/documents":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"document":{"id":"d1","title":"T","source":"s","chunk_count":2,"created_at":"2025-01-01T12:00:00Z","updated_at":"2025-01-01T12:00:00Z"},"warning":"embeddings unavailable"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/knowledge/documents":
			_, _ = w.Write([]byte(`{"documents":[{"id":"d1","title":"T","chunk_count":2,"created_at":"2025-01-01T12:00:00Z","updated_at":"2025-01-01T12:00:00Z"}]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	add, err := c.AddDocument(context.Background(), AddDocumentRequest{Title: "T", Content: "C", Source: "s"})
	if err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if add.Document.ID != "d1" || add.Document.ChunkCount != 2 || add.Warning == "" {
		t.Errorf("add = %+v", add)
	}
	list, err := c.ListDocuments(context.Background())
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(list.Documents) != 1 || list.Documents[0].Title != "T" {
		t.Errorf("list = %+v", list)
	}
}

// ---------- Usage + Tools ----------

func TestUsageCosts(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage/costs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"total_cost_cents":123.5,"series":[{"bucket":"2025-01-01","cost_cents":100,"runs":2},{"agent_id":"a-1","cost_cents":23.5,"runs":1}]}`))
	})
	res, err := c.Costs(context.Background(), CostsQuery{From: "2025-01-01", To: "2025-01-31", GroupBy: "day"})
	if err != nil {
		t.Fatalf("Costs: %v", err)
	}
	if res.TotalCostCents != 123.5 || len(res.Series) != 2 || res.Series[0].Bucket != "2025-01-01" {
		t.Errorf("res = %+v", res)
	}
	if res.Series[1].AgentID != "a-1" || res.Series[1].Bucket != "" {
		t.Errorf("bucket 2 = %+v", res.Series[1])
	}
	if gotQuery != "from=2025-01-01&group_by=day&to=2025-01-31" {
		t.Errorf("query = %q", gotQuery)
	}
}

func TestCostsEmptySeriesBecomesSlice(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total_cost_cents":0,"series":[]}`))
	})
	res, err := c.Costs(context.Background(), CostsQuery{})
	if err != nil {
		t.Fatalf("Costs: %v", err)
	}
	if res.Series == nil {
		t.Error("want non-nil empty Series")
	}
}

func TestListTools(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"tools":[{"name":"calculator","description":"arithmetic","input_schema":{"type":"object"}},{"name":"http_request"}]}`))
	})
	list, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list.Tools) != 2 || list.Tools[0].Name != "calculator" {
		t.Errorf("list = %+v", list)
	}
	if list.Tools[1].Description != "" || list.Tools[1].InputSchema != nil {
		t.Errorf("sparse tool = %+v", list.Tools[1])
	}
}

// ---------- Misc ----------

func TestDecodeErrorNamesRoute(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	})
	_, err := c.ListRuns(context.Background())
	if err == nil || !strings.Contains(err.Error(), "GET /runs") {
		t.Errorf("decode error should name the route, got %v", err)
	}
}

func TestTerminalRunStatuses(t *testing.T) {
	if !TerminalRunStatuses[RunStatusCompleted] || !TerminalRunStatuses[RunStatusFailed] {
		t.Error("COMPLETED/FAILED must be terminal")
	}
	if TerminalRunStatuses[RunStatusQueued] || TerminalRunStatuses[RunStatusRunning] {
		t.Error("QUEUED/RUNNING must not be terminal")
	}
}

func TestTimestampsRoundTrip(t *testing.T) {
	// The SDK relies on RFC3339 parsing for all typed timestamps.
	stamp := "2025-01-01T12:00:00.123456789Z"
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ID":"r","CreatedAt":%q,"UpdatedAt":%q}`, stamp, stamp)
	})
	run, err := c.GetRun(context.Background(), "r")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	want := time.Date(2025, 1, 1, 12, 0, 0, 123456789, time.UTC)
	if !run.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", run.CreatedAt, want)
	}
}
