package main

import (
        "context"
        "encoding/json"
        "net/http"
        "net/http/httptest"
        "strings"
        "testing"
        "time"

        "agentos/internal/agents"
        "agentos/internal/auth"
        "agentos/internal/evaluations"
        "agentos/internal/runtime"
)

// stubEvalRunner is the fake AgentRunner injected into the evaluations
// service for handler tests (no real runtime/provider needed).
type stubEvalRunner struct {
        fn func(ctx context.Context, agentID, input string) (*runtime.Run, error)
}

func (s *stubEvalRunner) Run(ctx context.Context, agentID, input string) (*runtime.Run, error) {
        return s.fn(ctx, agentID, input)
}

type evalTestEnv struct {
        mux     *http.ServeMux
        authSvc *auth.Service
        evalSvc *evaluations.Service
        orgID   string
        token   string
        agentID string
}

func newEvalTestEnv(t *testing.T, runner *stubEvalRunner) *evalTestEnv {
        t.Helper()
        authSvc := auth.NewService("test-secret")
        org, user, err := authSvc.RegisterCtx(context.Background(), "Acme", "owner@acme.test", "password123")
        if err != nil {
                t.Fatalf("Register returned error: %v", err)
        }
        token, err := authSvc.GenerateToken(user)
        if err != nil {
                t.Fatalf("GenerateToken returned error: %v", err)
        }
        agentSvc := agents.NewService()
        agent, err := agentSvc.CreateAgentCtx(context.Background(), org.ID, "Eval Agent", "d", "be deterministic", "gpt-4o-mini")
        if err != nil {
                t.Fatalf("CreateAgentCtx returned error: %v", err)
        }
        evalSvc := evaluations.NewService(evaluations.Deps{
                Agents:      agentSvc,
                Runner:      runner,
                CaseTimeout: 2 * time.Second,
        })
        mux := http.NewServeMux()
        registerEvaluationsRoutes(mux, evalSvc, authSvc, nil)
        return &evalTestEnv{mux: mux, authSvc: authSvc, evalSvc: evalSvc, orgID: org.ID, token: token, agentID: agent.ID}
}

func (e *evalTestEnv) do(t *testing.T, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
        t.Helper()
        var reader *strings.Reader
        if body == "" {
                reader = strings.NewReader("")
        } else {
                reader = strings.NewReader(body)
        }
        req := httptest.NewRequest(method, path, reader)
        if body != "" {
                req.Header.Set("Content-Type", "application/json")
        }
        if token != "" {
                req.Header.Set("Authorization", "Bearer "+token)
        }
        rr := httptest.NewRecorder()
        e.mux.ServeHTTP(rr, req)
        payload := map[string]any{}
        if strings.Contains(rr.Header().Get("Content-Type"), "application/json") && rr.Body.Len() > 0 {
                if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
                        t.Fatalf("response is not valid JSON: %v (%s)", err, rr.Body.String())
                }
        }
        return rr, payload
}

func TestEvalRoutesRequireAuth(t *testing.T) {
        env := newEvalTestEnv(t, &stubEvalRunner{fn: func(context.Context, string, string) (*runtime.Run, error) {
                return &runtime.Run{Status: runtime.StatusCompleted, Output: "out"}, nil
        }})
        for _, tc := range []struct{ method, path string }{
                {http.MethodGet, "/eval-datasets"},
                {http.MethodPost, "/eval-datasets/create"},
                {http.MethodGet, "/eval-datasets/ds-1"},
                {http.MethodPost, "/eval-datasets/ds-1/run"},
                {http.MethodGet, "/eval-runs/run-1"},
                {http.MethodPost, "/eval-runs/compare"},
        } {
                rr, _ := env.do(t, tc.method, tc.path, "", "{}")
                if rr.Code != http.StatusUnauthorized {
                        t.Fatalf("%s %s without credentials: want 401 got %d", tc.method, tc.path, rr.Code)
                }
        }
}

func TestEvalViewerCanReadButNotWrite(t *testing.T) {
        env := newEvalTestEnv(t, &stubEvalRunner{fn: func(context.Context, string, string) (*runtime.Run, error) {
                return &runtime.Run{Status: runtime.StatusCompleted, Output: "out"}, nil
        }})
        // Token for a user unknown to the service: middleware falls back to the
        // token role (VIEWER) when resolving permissions.
        viewerToken, err := env.authSvc.GenerateToken(&auth.User{
                ID: "viewer-user", Organization: env.orgID, Email: "viewer@nowhere.test", Role: "VIEWER",
        })
        if err != nil {
                t.Fatalf("GenerateToken returned error: %v", err)
        }

        rr, payload := env.do(t, http.MethodGet, "/eval-datasets", viewerToken, "")
        if rr.Code != http.StatusOK {
                t.Fatalf("viewer list: want 200 got %d (%s)", rr.Code, rr.Body.String())
        }
        if datasets, ok := payload["datasets"].([]any); !ok || len(datasets) != 0 {
                t.Fatalf("expected empty datasets array, got %v", payload["datasets"])
        }

        rr, _ = env.do(t, http.MethodPost, "/eval-datasets/create", viewerToken,
                `{"name":"nope","cases":[{"id":"c1","scorer":"exact","expected":"x"}]}`)
        if rr.Code != http.StatusForbidden {
                t.Fatalf("viewer create: want 403 got %d", rr.Code)
        }
        rr, _ = env.do(t, http.MethodPost, "/eval-runs/compare", viewerToken,
                `{"baseline_run_id":"a","candidate_run_id":"b"}`)
        if rr.Code != http.StatusForbidden {
                t.Fatalf("viewer compare: want 403 got %d", rr.Code)
        }
        rr, _ = env.do(t, http.MethodPost, "/eval-datasets/any/run", viewerToken, `{"agent_id":"x"}`)
        if rr.Code != http.StatusForbidden {
                t.Fatalf("viewer run: want 403 got %d", rr.Code)
        }
}

func TestEvalDatasetCreateListDetailFlow(t *testing.T) {
        env := newEvalTestEnv(t, nil)

        // 405 on wrong method.
        rr, _ := env.do(t, http.MethodGet, "/eval-datasets/create", env.token, "")
        if rr.Code != http.StatusMethodNotAllowed {
                t.Fatalf("GET create: want 405 got %d", rr.Code)
        }

        // Validation error shape.
        rr, payload := env.do(t, http.MethodPost, "/eval-datasets/create", env.token,
                `{"name":"Bad","cases":[{"id":"c1","scorer":"bogus"}]}`)
        if rr.Code != http.StatusBadRequest {
                t.Fatalf("invalid scorer: want 400 got %d", rr.Code)
        }
        if code := errorCode(t, payload); code != "VALIDATION_ERROR" {
                t.Fatalf("expected VALIDATION_ERROR, got %q", code)
        }

        body := `{"name":"Regression suite","description":"core cases","cases":[` +
                `{"id":"c1","input":"1+1","expected":"2","scorer":"exact"},` +
                `{"id":"c2","input":"greet","expected":"^ok$","scorer":"regex","params":{"pattern":"^ok$"}}]}`
        rr, payload = env.do(t, http.MethodPost, "/eval-datasets/create", env.token, body)
        if rr.Code != http.StatusCreated {
                t.Fatalf("create: want 201 got %d (%s)", rr.Code, rr.Body.String())
        }
        dataset, ok := payload["dataset"].(map[string]any)
        if !ok {
                t.Fatalf("create should return {dataset}, got %v", payload)
        }
        datasetID, _ := dataset["id"].(string)
        if datasetID == "" {
                t.Fatal("dataset id should be set")
        }
        if dataset["case_count"] != float64(2) {
                t.Fatalf("case_count should be 2, got %v", dataset["case_count"])
        }
        if _, ok := dataset["created_at"].(string); !ok {
                t.Fatalf("created_at should be an RFC3339 string, got %v", dataset["created_at"])
        }
        cases, ok := dataset["cases"].([]any)
        if !ok || len(cases) != 2 {
                t.Fatalf("create should echo cases, got %v", dataset["cases"])
        }

        rr, payload = env.do(t, http.MethodGet, "/eval-datasets", env.token, "")
        if rr.Code != http.StatusOK {
                t.Fatalf("list: want 200 got %d", rr.Code)
        }
        items, ok := payload["datasets"].([]any)
        if !ok {
                t.Fatalf("listing should carry a datasets array, got %v", payload["datasets"])
        }
        if len(items) != 1 {
                t.Fatalf("expected 1 dataset in listing, got %d", len(items))
        }
        item, _ := items[0].(map[string]any)
        if item["case_count"] != float64(2) {
                t.Fatalf("listing case_count should be 2, got %v", item["case_count"])
        }
        if _, hasCases := item["cases"]; hasCases {
                t.Fatal("listing must not embed case bodies")
        }

        rr, payload = env.do(t, http.MethodGet, "/eval-datasets/"+datasetID, env.token, "")
        if rr.Code != http.StatusOK {
                t.Fatalf("detail: want 200 got %d", rr.Code)
        }
        detail, _ := payload["dataset"].(map[string]any)
        if detail == nil || detail["cases"] == nil {
                t.Fatalf("detail should include cases, got %v", payload)
        }

        rr, _ = env.do(t, http.MethodGet, "/eval-datasets/unknown-id", env.token, "")
        if rr.Code != http.StatusNotFound {
                t.Fatalf("unknown dataset: want 404 got %d", rr.Code)
        }
}

func payloadGet(t *testing.T, payload map[string]any, key string) map[string]any {
        t.Helper()
        if payload == nil {
                t.Fatal("payload is nil")
        }
        inner, _ := payload[key].(map[string]any)
        if inner == nil {
                t.Fatalf("payload[%q] is not an object: %v", key, payload[key])
        }
        return inner
}

func payloadArray(t *testing.T, v any) ([]any, bool) {
        t.Helper()
        arr, ok := v.([]any)
        return arr, ok
}

func errorCode(t *testing.T, payload map[string]any) string {
        t.Helper()
        errObj, _ := payload["error"].(map[string]any)
        if errObj == nil {
                t.Fatalf("payload should carry {error:{code,message}}, got %v", payload)
        }
        code, _ := errObj["code"].(string)
        return code
}

func TestEvalRunExecutionResultsAndCompare(t *testing.T) {
        // Runner echoes the input; the dataset expectations select which cases pass.
        runnerOutput := map[string]string{"q1": "q1", "q2": "q2"}
        env := newEvalTestEnv(t, &stubEvalRunner{fn: func(_ context.Context, _, input string) (*runtime.Run, error) {
                out, ok := runnerOutput[input]
                if !ok {
                        return nil, context.DeadlineExceeded
                }
                return &runtime.Run{Status: runtime.StatusCompleted, Output: out}, nil
        }})

        rr, payload := env.do(t, http.MethodPost, "/eval-datasets/create", env.token,
                `{"name":"Suite","cases":[
                        {"id":"c1","input":"q1","expected":"q1","scorer":"exact"},
                        {"id":"c2","input":"q2","expected":"different","scorer":"exact"}]}`)
        if rr.Code != http.StatusCreated {
                t.Fatalf("create: want 201 got %d (%s)", rr.Code, rr.Body.String())
        }
        datasetID, _ := payloadGet(t, payload, "dataset")["id"].(string)

        rr, _ = env.do(t, http.MethodPost, "/eval-datasets/"+datasetID+"/run", env.token, `{"agent_id":"missing-agent"}`)
        if rr.Code != http.StatusNotFound {
                t.Fatalf("run with foreign agent: want 404 got %d", rr.Code)
        }
        rr, _ = env.do(t, http.MethodPost, "/eval-datasets/does-not-exist/run", env.token, `{"agent_id":"x"}`)
        if rr.Code != http.StatusNotFound {
                t.Fatalf("run unknown dataset: want 404 got %d", rr.Code)
        }
        rr, _ = env.do(t, http.MethodPost, "/eval-datasets/"+datasetID+"/run", env.token, `{}`)
        if rr.Code != http.StatusBadRequest {
                t.Fatalf("run without agent_id: want 400 got %d", rr.Code)
        }

        rr, payload = env.do(t, http.MethodPost, "/eval-datasets/"+datasetID+"/run", env.token, `{"agent_id":"`+env.agentID+`"}`)
        if rr.Code != http.StatusOK {
                t.Fatalf("run: want 200 got %d (%s)", rr.Code, rr.Body.String())
        }
        if payload["status"] != "completed" {
                t.Fatalf("synchronous run should report completed, got %v", payload["status"])
        }
        runID, _ := payload["eval_run_id"].(string)
        if runID == "" {
                t.Fatal("eval_run_id should be set")
        }

        rr, payload = env.do(t, http.MethodGet, "/eval-runs/"+runID, env.token, "")
        if rr.Code != http.StatusOK {
                t.Fatalf("run detail: want 200 got %d", rr.Code)
        }
        if payload["id"] != runID || payload["dataset_id"] != datasetID || payload["agent_id"] != env.agentID {
                t.Fatalf("run payload identity mismatch: %v", payload)
        }
        results, ok := payload["results"].([]any)
        if !ok || len(results) != 2 {
                t.Fatalf("expected 2 results, got %v", payload["results"])
        }
        first, _ := results[0].(map[string]any)
        for _, key := range []string{"case_id", "output", "passed", "score", "latency_ms", "cost_cents", "error"} {
                if _, present := first[key]; !present {
                        t.Fatalf("result should carry %q, got %v", key, first)
                }
        }
        summary := payloadGet(t, payload, "summary")
        if summary["pass_rate"] != 0.5 {
                t.Fatalf("pass_rate should be 0.5, got %v", summary["pass_rate"])
        }
        byScorer := payloadGet(t, summary, "by_scorer")
        exact := payloadGet(t, byScorer, "exact")
        if exact["passed"] != float64(1) || exact["failed"] != float64(1) {
                t.Fatalf("by_scorer exact should be {1,1}, got %v", exact)
        }

        // Second run after flipping c2 output: candidate improves c2.
        runnerOutput["q2"] = "different"
        rr, payload = env.do(t, http.MethodPost, "/eval-datasets/"+datasetID+"/run", env.token, `{"agent_id":"`+env.agentID+`"}`)
        if rr.Code != http.StatusOK {
                t.Fatalf("candidate run: want 200 got %d", rr.Code)
        }
        candidateID, _ := payload["eval_run_id"].(string)

        rr, payload = env.do(t, http.MethodPost, "/eval-runs/compare", env.token,
                `{"baseline_run_id":"`+runID+`","candidate_run_id":"`+candidateID+`"}`)
        if rr.Code != http.StatusOK {
                t.Fatalf("compare: want 200 got %d (%s)", rr.Code, rr.Body.String())
        }
        if payloadGet(t, payload, "baseline") == nil || payloadGet(t, payload, "candidate") == nil {
                t.Fatalf("compare should embed both summaries: %v", payload)
        }
        regressions, _ := payloadArray(t, payload["regressions"])
        improvements, _ := payloadArray(t, payload["improvements"])
        if len(regressions) != 0 {
                t.Fatalf("expected no regressions, got %v", regressions)
        }
        if len(improvements) != 1 {
                t.Fatalf("expected 1 improvement, got %v", improvements)
        }
        improvement, _ := improvements[0].(map[string]any)
        if improvement["case_id"] != "c2" || improvement["baseline_passed"] != false || improvement["candidate_passed"] != true {
                t.Fatalf("unexpected improvement entry: %v", improvement)
        }

        rr, payload = env.do(t, http.MethodPost, "/eval-runs/compare", env.token, `{"baseline_run_id":"","candidate_run_id":"b"}`)
        if rr.Code != http.StatusBadRequest || errorCode(t, payload) != "VALIDATION_ERROR" {
                t.Fatalf("compare without ids: want 400 VALIDATION_ERROR got %d %v", rr.Code, payload)
        }
        rr, payload = env.do(t, http.MethodPost, "/eval-runs/compare", env.token,
                `{"baseline_run_id":"`+runID+`","candidate_run_id":"missing"}`)
        if rr.Code != http.StatusNotFound {
                t.Fatalf("compare with unknown candidate: want 404 got %d", rr.Code)
        }
        rr, _ = env.do(t, http.MethodGet, "/eval-runs/missing", env.token, "")
        if rr.Code != http.StatusNotFound {
                t.Fatalf("unknown run: want 404 got %d", rr.Code)
        }
}

func TestEvalTenantIsolation(t *testing.T) {
        env := newEvalTestEnv(t, &stubEvalRunner{fn: func(_ context.Context, _, input string) (*runtime.Run, error) {
                return &runtime.Run{Status: runtime.StatusCompleted, Output: input}, nil
        }})

        rr, payload := env.do(t, http.MethodPost, "/eval-datasets/create", env.token,
                `{"name":"Secret suite","cases":[{"id":"c1","expected":"x","scorer":"exact"}]}`)
        if rr.Code != http.StatusCreated {
                t.Fatalf("create: want 201 got %d", rr.Code)
        }
        datasetID, _ := payloadGet(t, payload, "dataset")["id"].(string)

        // A different tenant (fabricated token, unregistered email -> role path)
        // must not see or run the dataset.
        foreignToken, err := env.authSvc.GenerateToken(&auth.User{
                ID: "intruder", Organization: "org-foreign", Email: "intruder@nowhere.test", Role: "OWNER",
        })
        if err != nil {
                t.Fatalf("GenerateToken returned error: %v", err)
        }
        rr, _ = env.do(t, http.MethodGet, "/eval-datasets/"+datasetID, foreignToken, "")
        if rr.Code != http.StatusNotFound {
                t.Fatalf("foreign dataset read: want 404 got %d", rr.Code)
        }
        rr, _ = env.do(t, http.MethodGet, "/eval-datasets", foreignToken, "")
        if rr.Code != http.StatusOK {
                t.Fatalf("foreign list: want 200 got %d", rr.Code)
        }
}
