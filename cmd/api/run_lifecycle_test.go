package main

// run_lifecycle_test.go — issue #115: the run pause/resume/cancel HTTP
// surface, exercised through the REAL middleware chain
// (RequireAuthOrAPIKey → RequirePermission runs.control): the 401/403/404/409
// matrix, the enforced-transition happy paths, the resume re-enqueue into the
// task queue (the queue is the only execution trigger) and the nil-service
// guard. House pattern: cmd/api/residency_test.go.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

var rlCtx = context.Background

func newRunLifecycleTestRouter(t *testing.T) (http.Handler, *auth.Service, *runs.Service, *queue.Queue) {
	t.Helper()
	authSvc := auth.NewService("rl-test-secret")
	keysSvc := apikeys.NewService()
	runsSvc := runs.NewService()
	q := queue.NewQueue()
	apiMux := http.NewServeMux()
	registerRunLifecycleRoutes(apiMux, runsSvc, q, authSvc, keysSvc)
	return apiMux, authSvc, runsSvc, q
}

func rlTokenFor(t *testing.T, authSvc *auth.Service, email, orgID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&auth.User{ID: "user-" + email, Organization: orgID, Email: email, Role: role})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return token
}

func rlOwner(t *testing.T, authSvc *auth.Service, email string) (string, string) {
	t.Helper()
	org, user, err := authSvc.Register("RL Org", email, "secret123")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return org.ID, rlTokenFor(t, authSvc, email, org.ID, user.Role)
}

func rlDo(t *testing.T, h http.Handler, method, path, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr, body
}

func rlErrorCode(t *testing.T, body map[string]any) string {
	t.Helper()
	errObj, _ := body["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}

func rlRawBody(rr *httptest.ResponseRecorder) string {
	b, _ := io.ReadAll(rr.Body)
	return string(b)
}

func TestRunLifecycleAuthRequired(t *testing.T) {
	h, _, _, _ := newRunLifecycleTestRouter(t)
	for _, path := range []string{"/runs/run-1/pause", "/runs/run-1/resume", "/runs/run-1/cancel"} {
		rr, _ := rlDo(t, h, http.MethodPost, path, "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without credentials: want 401, got %d", path, rr.Code)
		}
	}
}

func TestRunLifecycleRBACMatrix(t *testing.T) {
	h, authSvc, runsSvc, _ := newRunLifecycleTestRouter(t)
	orgID, _ := rlOwner(t, authSvc, "owner@rl.test")
	run, err := runsSvc.CreateRunCtx(rlCtx(), orgID, "agent-1", "hi")
	if err != nil {
		t.Fatalf("CreateRunCtx: %v", err)
	}
	tokens := map[string]string{}
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER"} {
		tokens[role] = rlTokenFor(t, authSvc, strings.ToLower(role)+"@rl.test", orgID, role)
	}

	// runs.control is granted to OWNER/ADMIN/MEMBER — VIEWER never controls runs.
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER"} {
		rr, _ := rlDo(t, h, http.MethodPost, "/runs/"+run.ID+"/pause", tokens[role])
		if rr.Code != http.StatusOK {
			t.Errorf("pause as %s: want 200, got %d body=%s", role, rr.Code, rr.Body.String())
		}
		// Bring the run back for the next role.
		if _, err := runsSvc.ResumeRunQueued(rlCtx(), orgID, run.ID); err != nil {
			t.Fatalf("reset run: %v", err)
		}
	}
	rr, _ := rlDo(t, h, http.MethodPost, "/runs/"+run.ID+"/pause", tokens["VIEWER"])
	if rr.Code != http.StatusForbidden {
		t.Errorf("pause as VIEWER: want 403, got %d", rr.Code)
	}
}

func TestRunLifecycleHappyPath(t *testing.T) {
	h, authSvc, runsSvc, q := newRunLifecycleTestRouter(t)
	orgID, token := rlOwner(t, authSvc, "happy@rl.test")
	run, err := runsSvc.CreateRunCtx(rlCtx(), orgID, "agent-1", "the input")
	if err != nil {
		t.Fatalf("CreateRunCtx: %v", err)
	}

	// pause: queued → paused.
	rr, _ := rlDo(t, h, http.MethodPost, "/runs/"+run.ID+"/pause", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("pause: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	stored, _ := runsSvc.GetRunCtx(rlCtx(), orgID, run.ID)
	if stored.Status != runs.StatusPaused {
		t.Fatalf("durable status after pause = %s, want paused", stored.Status)
	}

	// resume: paused → queued AND the agent.run task re-enqueued (the queue
	// is the only execution trigger).
	before := q.Length()
	rr, _ = rlDo(t, h, http.MethodPost, "/runs/"+run.ID+"/resume", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("resume: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	stored, _ = runsSvc.GetRunCtx(rlCtx(), orgID, run.ID)
	if stored.Status != runs.RunStatus(runs.StateQueued) {
		t.Fatalf("durable status after resume = %q, want queued (canonical spelling; legacy StatusQueued is UPPERCASE)", stored.Status)
	}
	if q.Length() != before+1 {
		t.Fatalf("queue length = %d, want %d (resume must re-enqueue agent.run)", q.Length(), before+1)
	}

	// cancel: queued → cancelled (nothing in flight: direct terminal write).
	rr, _ = rlDo(t, h, http.MethodPost, "/runs/"+run.ID+"/cancel", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	stored, _ = runsSvc.GetRunCtx(rlCtx(), orgID, run.ID)
	if stored.Status != runs.StatusCancelled {
		t.Fatalf("durable status after cancel = %s, want cancelled", stored.Status)
	}
	// Idempotent cancel stays 200.
	rr, _ = rlDo(t, h, http.MethodPost, "/runs/"+run.ID+"/cancel", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("idempotent cancel: want 200, got %d", rr.Code)
	}
	// A rejected resume never enqueues tasks.
	qLen := q.Length()
	rr, body := rlDo(t, h, http.MethodPost, "/runs/"+run.ID+"/resume", token)
	if rr.Code != http.StatusConflict {
		t.Fatalf("resume of cancelled run: want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if code := rlErrorCode(t, body); code != "INVALID_STATE" {
		t.Fatalf("rejected resume error code = %s, want INVALID_STATE", code)
	}
	if q.Length() != qLen {
		t.Errorf("rejected resume must not enqueue tasks")
	}
}

func TestRunLifecycleTwoPhaseCancelOfRunningRun(t *testing.T) {
	h, authSvc, runsSvc, _ := newRunLifecycleTestRouter(t)
	orgID, token := rlOwner(t, authSvc, "running@rl.test")
	run, _ := runsSvc.CreateRunCtx(rlCtx(), orgID, "agent-1", "hi")
	if err := runsSvc.TransitionRunStatus(rlCtx(), orgID, run.ID, runs.StateRunning, ""); err != nil {
		t.Fatalf("to running: %v", err)
	}
	rr, _ := rlDo(t, h, http.MethodPost, "/runs/"+run.ID+"/cancel", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel running: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	stored, _ := runsSvc.GetRunCtx(rlCtx(), orgID, run.ID)
	if stored.Status != runs.RunStatus(runs.StateCancelling) {
		t.Fatalf("in-flight cancel must land in cancelling (two-phase), got %s", stored.Status)
	}
}

func TestRunLifecycleConflictMatrix(t *testing.T) {
	h, authSvc, runsSvc, _ := newRunLifecycleTestRouter(t)
	orgID, token := rlOwner(t, authSvc, "conflict@rl.test")

	// Terminal runs are immutable: pause/resume all 409. Cancel is
	// idempotent ONLY for the already-cancelled state (200); completed/
	// failed/timeout rejects it with 409.
	for _, status := range []runs.RunStatus{runs.StatusCompleted, runs.StatusFailed, runs.StatusTimeout} {
		run, _ := runsSvc.CreateRunCtx(rlCtx(), orgID, "agent-1", "hi")
		if err := runsSvc.UpdateStatusCtx(rlCtx(), orgID, run.ID, status, ""); err != nil {
			t.Fatalf("setup %s: %v", status, err)
		}
		for _, path := range []string{"/runs/" + run.ID + "/pause", "/runs/" + run.ID + "/resume", "/runs/" + run.ID + "/cancel"} {
			rr, body := rlDo(t, h, http.MethodPost, path, token)
			if rr.Code != http.StatusConflict {
				t.Errorf("%s on %s run: want 409, got %d body=%s", path, status, rr.Code, rlRawBody(rr))
				continue
			}
			if code := rlErrorCode(t, body); code != "INVALID_STATE" {
				t.Errorf("%s on %s run: error code = %s, want INVALID_STATE", path, status, code)
			}
		}
	}
	// cancelled: cancel stays 200 (idempotent no-op), pause/resume are 409.
	cancelRun, _ := runsSvc.CreateRunCtx(rlCtx(), orgID, "agent-1", "hi")
	if err := runsSvc.UpdateStatusCtx(rlCtx(), orgID, cancelRun.ID, runs.StatusCancelled, ""); err != nil {
		t.Fatalf("setup cancelled: %v", err)
	}
	for _, tc := range []struct {
		path string
		want int
	}{{"/runs/" + cancelRun.ID + "/pause", http.StatusConflict},
		{"/runs/" + cancelRun.ID + "/resume", http.StatusConflict},
		{"/runs/" + cancelRun.ID + "/cancel", http.StatusOK}} {
		rr, body := rlDo(t, h, http.MethodPost, tc.path, token)
		if rr.Code != tc.want {
			t.Errorf("%s on cancelled run: want %d, got %d", tc.path, tc.want, rr.Code)
		}
		if tc.want == http.StatusConflict && rlErrorCode(t, body) != "INVALID_STATE" {
			t.Errorf("%s on cancelled run: error code = %s, want INVALID_STATE", tc.path, rlErrorCode(t, body))
		}
	}

	// waiting_approval is gated: the endpoint resume must NOT bypass it (409),
	// while pause stays available to an operator.
	gated, _ := runsSvc.CreateRunCtx(rlCtx(), orgID, "agent-1", "hi")
	if err := runsSvc.UpdateStatusCtx(rlCtx(), orgID, gated.ID, runs.StatusWaitingApproval, ""); err != nil {
		t.Fatalf("setup gated: %v", err)
	}
	rr, _ := rlDo(t, h, http.MethodPost, "/runs/"+gated.ID+"/resume", token)
	if rr.Code != http.StatusConflict {
		t.Errorf("resume of waiting_approval run: want 409, got %d", rr.Code)
	}
	rr, _ = rlDo(t, h, http.MethodPost, "/runs/"+gated.ID+"/pause", token)
	if rr.Code != http.StatusOK {
		t.Errorf("pause of waiting_approval run: want 200, got %d", rr.Code)
	}
}

func TestRunLifecycleNotFoundAndTenantGuard(t *testing.T) {
	h, authSvc, runsSvc, _ := newRunLifecycleTestRouter(t)
	orgA, tokenA := rlOwner(t, authSvc, "a@rl.test")
	_, tokenB := rlOwner(t, authSvc, "b@rl.test")

	// Unknown run: 404 (no existence leak — foreign runs share the code).
	for _, path := range []string{"/runs/does-not-exist/pause", "/runs/does-not-exist/resume", "/runs/does-not-exist/cancel"} {
		rr, _ := rlDo(t, h, http.MethodPost, path, tokenA)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s unknown run: want 404, got %d", path, rr.Code)
		}
	}
	// Cross-tenant access is indistinguishable from unknown: 404.
	run, err := runsSvc.CreateRunCtx(rlCtx(), orgA, "agent-1", "hi")
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	rr, _ := rlDo(t, h, http.MethodPost, "/runs/"+run.ID+"/pause", tokenB)
	if rr.Code != http.StatusNotFound {
		t.Errorf("foreign run pause: want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	// The owner still controls the run afterwards.
	rr, _ = rlDo(t, h, http.MethodPost, "/runs/"+run.ID+"/pause", tokenA)
	if rr.Code != http.StatusOK {
		t.Errorf("owner pause after foreign attempt: want 200, got %d", rr.Code)
	}
}

func TestRunLifecycleNilRunsService503(t *testing.T) {
	authSvc := auth.NewService("rl-nil-secret")
	keysSvc := apikeys.NewService()
	apiMux := http.NewServeMux()
	registerRunLifecycleRoutes(apiMux, nil, nil, authSvc, keysSvc)
	token := rlTokenFor(t, authSvc, "nil@rl.test", "org-nil", "OWNER")
	rr, _ := rlDo(t, apiMux, http.MethodPost, "/runs/run-1/pause", token)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil runs service: want 503, got %d", rr.Code)
	}
}
