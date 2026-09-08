package main

// Issue #77 handler tests — queue introspection + dead-letter requeue half:
// RBAC (reads runs.read; requeue queue.manage OWNER/ADMIN with MEMBER/VIEWER
// denied), org scoping from the auth claims, keyset pagination through the
// HTTP contract, cross-tenant 404s, the full dead-letter → requeue → picked
// up loop in BOTH backends (in-memory and miniredis) and the audit trail.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/config"
	"agentos/internal/queue"

	"github.com/alicebob/miniredis/v2"
)

// queueHandlerEnv wires the handler stack and returns bearer tokens covering
// every role relevant to the queue surface (reads: all roles; requeue:
// OWNER/ADMIN only).
type queueHandlerEnv struct {
	mux         *http.ServeMux
	q           *queue.Queue
	auditSvc    *audit.Service
	orgID       string
	ownerToken  string
	adminToken  string
	memberToken string
	viewerToken string
	otherToken  string
}

func newQueueHandlerEnv(t *testing.T, q *queue.Queue) *queueHandlerEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()
	auditSvc := audit.NewService()

	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	token := func(id, email, role string) string {
		t.Helper()
		generated, err := authSvc.GenerateToken(&auth.User{
			ID: id, Organization: owner.Organization, Email: email, Role: role,
		})
		if err != nil {
			t.Fatalf("GenerateToken(%s) returned error: %v", role, err)
		}
		return generated
	}
	_, foreign, err := authSvc.Register("OtherCo", "owner@other.test", "secret123")
	if err != nil {
		t.Fatalf("Register(foreign) returned error: %v", err)
	}
	otherToken, err := authSvc.GenerateToken(foreign)
	if err != nil {
		t.Fatalf("GenerateToken(foreign) returned error: %v", err)
	}

	if q == nil {
		q = queue.NewQueue()
	}
	mux := http.NewServeMux()
	registerQueueOpsRoutes(mux, q, authSvc, apiKeysSvc, auditSvc)

	return &queueHandlerEnv{
		mux:         mux,
		q:           q,
		auditSvc:    auditSvc,
		orgID:       owner.Organization,
		ownerToken:  token("user-owner", "owner@acme.test", "OWNER"),
		adminToken:  token("user-admin", "admin@acme.test", "ADMIN"),
		memberToken: token("user-member", "member@acme.test", "MEMBER"),
		viewerToken: token("user-viewer", "viewer@acme.test", "VIEWER"),
		otherToken:  otherToken,
	}
}

func (e *queueHandlerEnv) do(t *testing.T, method, path, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	var decoded map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
	}
	return rr, decoded
}

// errCodeQops extracts error.code from the shared error envelope.
func errCodeQops(decoded map[string]any) string {
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObj["code"].(string)
	return code
}

// deadLetterThroughEngineQops drives the real worker loop with a failing
// handler until the task dead-letters (4 failed attempts).
func deadLetterThroughEngineQops(t *testing.T, q *queue.Queue, orgID string) *queue.Task {
	t.Helper()
	sent := q.Enqueue("agent.run", map[string]any{"organization_id": orgID, "run_id": "r-dl", "input": "hi"})
	if sent == nil {
		t.Fatal("Enqueue returned nil task")
	}
	worker := queue.NewWorker(q, func(*queue.Task) error { return errors.New("transient failure") })
	for i := 0; i < 10; i++ {
		_ = worker.ProcessNext() // transient failures surface as errors; the loop keeps walking
		if sent.Status == queue.StatusDeadLetter {
			return sent
		}
	}
	t.Fatal("task never reached dead_letter through the engine loop")
	return nil
}

func TestQueueTasksListPaginationHTTP(t *testing.T) {
	env := newQueueHandlerEnv(t, nil)
	ids := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		task := env.q.Enqueue("agent.run", map[string]any{"organization_id": env.orgID, "i": i})
		ids = append(ids, task.ID)
	}
	for i := 0; i < 5; i++ {
		env.q.Enqueue("agent.run", map[string]any{"organization_id": "org-foreign", "i": i})
	}

	// Walking pages yields every task exactly once, newest enqueue first,
	// and never a foreign-org task.
	seen := make(map[string]bool)
	var firstID string
	cursor := ""
	pages := 0
	for {
		rr, decoded := env.do(t, http.MethodGet, "/queue/tasks?limit=10"+cursorParam(cursor), env.ownerToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /queue/tasks page %d should be 200, got %d: %v", pages, rr.Code, rr.Body.String())
		}
		tasks, ok := decoded["tasks"].([]any)
		if !ok {
			t.Fatalf("response must carry a tasks array: %v", decoded)
		}
		if len(tasks) == 0 || len(tasks) > 10 {
			t.Fatalf("page %d carried %d tasks, want 1..10", pages, len(tasks))
		}
		if pages == 0 && len(tasks) > 0 {
			first, _ := tasks[0].(map[string]any)
			firstID, _ = first["id"].(string)
			for _, key := range []string{"id", "type", "status", "attempts", "requeues", "created_at", "updated_at"} {
				if _, present := first[key]; !present {
					t.Fatalf("task view missing %q: %v", key, first)
				}
			}
			if _, leaked := first["organization_id"]; leaked {
				t.Fatalf("task view must not leak organization_id: %v", first)
			}
			if first["status"] != queue.StatusQueued {
				t.Fatalf("fresh task must be queued, got %v", first["status"])
			}
		}
		for _, raw := range tasks {
			task, _ := raw.(map[string]any)
			id, _ := task["id"].(string)
			if seen[id] {
				t.Fatalf("task %s appeared on two pages", id)
			}
			seen[id] = true
		}
		pages++
		next, _ := decoded["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("pagination walk exceeded the expected page count")
		}
	}
	if pages != 3 {
		t.Fatalf("expected 3 pages of 10/10/5, got %d", pages)
	}
	if len(seen) != 25 {
		t.Fatalf("pagination walked %d tasks, want 25 (foreign-org tasks excluded)", len(seen))
	}
	if firstID != ids[len(ids)-1] {
		t.Fatalf("listing must be newest-enqueue-first: first page head %q, want %q", firstID, ids[len(ids)-1])
	}

	// Malformed cursor answers 400 INVALID_CURSOR.
	rr, decoded := env.do(t, http.MethodGet, "/queue/tasks?limit=10&cursor=not-a-cursor", env.ownerToken)
	if rr.Code != http.StatusBadRequest || errCodeQops(decoded) != "INVALID_CURSOR" {
		t.Fatalf("malformed cursor should be 400 INVALID_CURSOR, got %d %v", rr.Code, decoded)
	}
	// Unknown status filter answers 400; a valid one filters.
	rr, decoded = env.do(t, http.MethodGet, "/queue/tasks?status=zombie", env.ownerToken)
	if rr.Code != http.StatusBadRequest || errCodeQops(decoded) != "INVALID_REQUEST" {
		t.Fatalf("invalid status should be 400 INVALID_REQUEST, got %d %v", rr.Code, decoded)
	}
	rr, decoded = env.do(t, http.MethodGet, "/queue/tasks?status=queued&limit=3", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid status filter should be 200, got %d", rr.Code)
	}
	if tasks, _ := decoded["tasks"].([]any); len(tasks) != 3 {
		t.Fatalf("limit=3 must cap the page, got %d tasks", len(tasks))
	}
	// Garbage and non-positive limits answer 400.
	rr, _ = env.do(t, http.MethodGet, "/queue/tasks?limit=soon", env.ownerToken)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("non-integer limit should be 400, got %d", rr.Code)
	}
	rr, _ = env.do(t, http.MethodGet, "/queue/tasks?limit=0", env.ownerToken)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 should be 400, got %d", rr.Code)
	}
	// Unauthenticated callers are rejected before any data leaves.
	rr, _ = env.do(t, http.MethodGet, "/queue/tasks", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated listing should be 401, got %d", rr.Code)
	}
}

// cursorParam renders the query fragment for a follow-up page.
func cursorParam(cursor string) string {
	if cursor == "" {
		return ""
	}
	return "&cursor=" + cursor
}

func TestQueueTaskDetailHTTP(t *testing.T) {
	env := newQueueHandlerEnv(t, nil)
	sent := deadLetterThroughEngineQops(t, env.q, env.orgID)

	rr, decoded := env.do(t, http.MethodGet, "/queue/tasks/"+sent.ID, env.viewerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /queue/tasks/{id} should be 200 (reads are runs.read), got %d: %v", rr.Code, rr.Body.String())
	}
	if decoded["id"] != sent.ID || decoded["type"] != "agent.run" {
		t.Fatalf("detail = %v, want task %s", decoded, sent.ID)
	}
	if decoded["status"] != queue.StatusDeadLetter {
		t.Fatalf("detail status = %v, want dead_letter", decoded["status"])
	}
	if decoded["attempts"] != float64(4) {
		t.Fatalf("detail attempts = %v, want 4", decoded["attempts"])
	}
	if decoded["last_error"] != "transient failure" {
		t.Fatalf("detail last_error = %v, want the handler failure", decoded["last_error"])
	}
	for _, key := range []string{"created_at", "updated_at"} {
		if _, present := decoded[key]; !present {
			t.Fatalf("detail missing %q: %v", key, decoded)
		}
	}
	if _, leaked := decoded["organization_id"]; leaked {
		t.Fatalf("detail must not leak organization_id: %v", decoded)
	}

	// Unknown id: 404 with the contract envelope.
	rr, decoded = env.do(t, http.MethodGet, "/queue/tasks/no-such-task", env.ownerToken)
	if rr.Code != http.StatusNotFound || errCodeQops(decoded) != "NOT_FOUND" {
		t.Fatalf("unknown task should be 404 NOT_FOUND, got %d %v", rr.Code, decoded)
	}
	// Cross-tenant id: the SAME 404 (no existence leak).
	rr, _ = env.do(t, http.MethodGet, "/queue/tasks/"+sent.ID, env.otherToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("foreign-tenant task detail should be 404, got %d", rr.Code)
	}
}

func TestQueueRequeueFlowMemoryHTTP(t *testing.T) {
	env := newQueueHandlerEnv(t, nil)
	sent := deadLetterThroughEngineQops(t, env.q, env.orgID)
	requeuePath := "/queue/tasks/" + sent.ID + "/requeue"

	// RBAC: MEMBER and VIEWER are denied queue.manage; nothing mutates.
	for _, tc := range []struct {
		name  string
		token string
	}{{"member", env.memberToken}, {"viewer", env.viewerToken}} {
		rr, _ := env.do(t, http.MethodPost, requeuePath, tc.token)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s requeue should be 403, got %d", tc.name, rr.Code)
		}
	}
	if entries, err := env.auditSvc.ListCtx(t.Context(), env.orgID); err != nil || len(entries) != 0 {
		t.Fatalf("rejected requeues must not be audited: entries=%d err=%v", len(entries), err)
	}

	// Unauthenticated: 401.
	if rr, _ := env.do(t, http.MethodPost, requeuePath, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated requeue should be 401")
	}

	// ADMIN passes.
	rr, decoded := env.do(t, http.MethodPost, requeuePath, env.adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin requeue should be 200, got %d: %v", rr.Code, rr.Body.String())
	}
	if decoded["status"] != queue.StatusQueued || decoded["attempts"] != float64(0) || decoded["requeues"] != float64(1) {
		t.Fatalf("requeued record = %v, want queued/0 attempts/1 requeue", decoded)
	}
	if got := decoded["last_error"]; got != nil && got != "" {
		t.Fatalf("requeue must clear last_error, got %v", got)
	}
	if _, present := decoded["requeued_at"]; !present {
		t.Fatalf("requeue must stamp requeued_at: %v", decoded)
	}
	if env.q.Length() != 1 {
		t.Fatalf("requeued task must be back in the work list, length=%d", env.q.Length())
	}

	// Second requeue is a 409 CONFLICT: the task is queued, not dead-lettered.
	rr, decoded = env.do(t, http.MethodPost, requeuePath, env.ownerToken)
	if rr.Code != http.StatusConflict || errCodeQops(decoded) != "CONFLICT" {
		t.Fatalf("second requeue should be 409 CONFLICT, got %d %v", rr.Code, decoded)
	}

	// The engine picks the revived task up and completes it; attempt
	// bookkeeping restarts from zero while the requeues counter survives.
	var processed []*queue.Task
	worker := queue.NewWorker(env.q, func(task *queue.Task) error {
		processed = append(processed, task)
		return nil
	})
	if err := worker.ProcessNext(); err != nil {
		t.Fatalf("worker failed to pick up the requeued task: %v", err)
	}
	if len(processed) != 1 || processed[0].ID != sent.ID {
		t.Fatalf("worker processed %#v, want the revived task %s", processed, sent.ID)
	}
	if processed[0].Attempts != 1 {
		t.Fatalf("revived task must restart attempt bookkeeping, got attempts=%d", processed[0].Attempts)
	}
	_, completed := env.do(t, http.MethodGet, "/queue/tasks/"+sent.ID, env.memberToken)
	if completed["status"] != queue.StatusCompleted || completed["requeues"] != float64(1) {
		t.Fatalf("completed record = %v, want completed with requeues=1", completed)
	}

	// Exactly one audit row for the one successful requeue (the 409 wrote
	// nothing): actor, action, org, resource and metadata pinned.
	entries, err := env.auditSvc.ListCtx(t.Context(), env.orgID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit trail must hold exactly one requeue row, got %d entries err=%v", len(entries), err)
	}
	entry := entries[0]
	if entry.Action != "queue.task_requeued" || entry.OrganizationID != env.orgID || entry.Resource != "queue/tasks/"+sent.ID {
		t.Fatalf("audit row = action %q org %q resource %q", entry.Action, entry.OrganizationID, entry.Resource)
	}
	if entry.Metadata["task_type"] != "agent.run" || entry.Metadata["requeues"] != 1 {
		t.Fatalf("audit metadata = %v, want task_type + requeues", entry.Metadata)
	}
}

func TestQueueRequeueFlowRedisHTTP(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := config.Config{Queue: config.QueueConfig{Mode: config.QueueModeRedis, Redis: config.RedisConfig{
		Addr: server.Addr(), QueueKey: "agentos:test:queue-ops",
	}}}
	q, err := queue.NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewFromConfig(redis) returned error: %v", err)
	}
	defer q.Close()
	env := newQueueHandlerEnv(t, q)

	// Drive the task to dead_letter through the engine loop against the
	// Redis backend (dequeue hands back decoded copies, so poll the record).
	sent := env.q.Enqueue("agent.run", map[string]any{"organization_id": env.orgID, "run_id": "r-redis"})
	if sent == nil {
		t.Fatal("Enqueue returned nil task")
	}
	worker := queue.NewWorker(q, func(*queue.Task) error { return errors.New("transient failure") })
	deadLetterID := ""
	for i := 0; i < 10; i++ {
		_ = worker.ProcessNext()
		if _, detail := env.do(t, http.MethodGet, "/queue/tasks/"+sent.ID, env.ownerToken); detail["status"] == queue.StatusDeadLetter {
			deadLetterID = sent.ID
			break
		}
	}
	if deadLetterID == "" {
		t.Fatal("task never reached dead_letter through the engine loop (redis)")
	}
	if _, detail := env.do(t, http.MethodGet, "/queue/tasks/"+sent.ID, env.ownerToken); detail["attempts"] != float64(4) {
		t.Fatalf("redis dead-letter record attempts = %v, want 4", detail["attempts"])
	}

	// Requeue over HTTP: attempts reset, work list restored.
	requeuePath := "/queue/tasks/" + sent.ID + "/requeue"
	rr, decoded := env.do(t, http.MethodPost, requeuePath, env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("redis requeue should be 200, got %d: %v", rr.Code, rr.Body.String())
	}
	if decoded["status"] != queue.StatusQueued || decoded["attempts"] != float64(0) || decoded["requeues"] != float64(1) {
		t.Fatalf("redis requeued record = %v", decoded)
	}
	if q.Length() != 1 {
		t.Fatalf("requeued task must be back in the redis work list, length=%d", q.Length())
	}
	// Second requeue: 409 (record now queued).
	if rr, decoded := env.do(t, http.MethodPost, requeuePath, env.ownerToken); rr.Code != http.StatusConflict || errCodeQops(decoded) != "CONFLICT" {
		t.Fatalf("second redis requeue should be 409 CONFLICT, got %d %v", rr.Code, decoded)
	}

	// A succeeding worker picks it up; the record reflects completion.
	success := queue.NewWorker(q, func(*queue.Task) error { return nil })
	if err := success.ProcessNext(); err != nil {
		t.Fatalf("worker failed to pick up the requeued redis task: %v", err)
	}
	_, completed := env.do(t, http.MethodGet, "/queue/tasks/"+sent.ID, env.ownerToken)
	if completed["status"] != queue.StatusCompleted || completed["requeues"] != float64(1) {
		t.Fatalf("redis completed record = %v, want completed with requeues=1", completed)
	}
}

func TestQueueStatsHTTP(t *testing.T) {
	env := newQueueHandlerEnv(t, nil)
	deadLetterThroughEngineQops(t, env.q, env.orgID)
	env.q.Enqueue("agent.run", map[string]any{"organization_id": env.orgID})
	running := env.q.Enqueue("agent.run", map[string]any{"organization_id": env.orgID})
	env.q.MarkStarted(running)
	done := env.q.Enqueue("agent.run", map[string]any{"organization_id": env.orgID})
	env.q.Ack(done)
	env.q.Enqueue("agent.run", map[string]any{"organization_id": "org-foreign"})

	rr, decoded := env.do(t, http.MethodGet, "/queue/stats", env.viewerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /queue/stats should be 200 (reads are runs.read), got %d", rr.Code)
	}
	stats, ok := decoded["stats"].(map[string]any)
	if !ok {
		t.Fatalf("response must carry a stats object: %v", decoded)
	}
	want := map[string]float64{
		queue.StatusQueued:     1,
		queue.StatusRunning:    1,
		queue.StatusDeadLetter: 1,
		queue.StatusCompleted:  1,
	}
	for status, count := range want {
		if stats[status] != count {
			t.Fatalf("stats[%s] = %v, want %v (full map: %v)", status, stats[status], count, stats)
		}
	}
	if decoded["total"] != float64(4) {
		t.Fatalf("total = %v, want 4 (the foreign-org task must not count)", decoded["total"])
	}

	// Org scoping: the foreign tenant sees only its own (empty) depth.
	rr, decoded = env.do(t, http.MethodGet, "/queue/stats", env.otherToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign stats should be 200, got %d", rr.Code)
	}
	if decoded["total"] != float64(0) {
		t.Fatalf("foreign total = %v, want 0", decoded["total"])
	}
	// Unauthenticated callers are rejected.
	if rr, _ := env.do(t, http.MethodGet, "/queue/stats", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stats should be 401")
	}
}

// TestQueueCrossTenantRequeueIs404 pins the no-existence-leak contract on the
// mutating path: a foreign dead-letter id is indistinguishable from an
// unknown one, and nothing is audited.
func TestQueueCrossTenantRequeueIs404(t *testing.T) {
	env := newQueueHandlerEnv(t, nil)
	sent := deadLetterThroughEngineQops(t, env.q, env.orgID)

	rr, decoded := env.do(t, http.MethodPost, "/queue/tasks/"+sent.ID+"/requeue", env.otherToken)
	if rr.Code != http.StatusNotFound || errCodeQops(decoded) != "NOT_FOUND" {
		t.Fatalf("foreign requeue should be 404 NOT_FOUND, got %d %v", rr.Code, decoded)
	}
	// A dead-lettered task sits OUTSIDE the work list (it was dequeued when
	// it failed its 4th attempt); the foreign refusal must not add an entry.
	if env.q.Length() != 0 {
		t.Fatalf("foreign requeue must not touch the work list, length=%d", env.q.Length())
	}
	// The task is untouched: still dead-lettered for its own org, which can
	// still requeue it (the foreign caller learned nothing either way).
	_, detail := env.do(t, http.MethodGet, "/queue/tasks/"+sent.ID, env.ownerToken)
	if detail["status"] != queue.StatusDeadLetter {
		t.Fatalf("foreign requeue must not mutate the task, status = %v", detail["status"])
	}
	if rr, decoded := env.do(t, http.MethodPost, "/queue/tasks/"+sent.ID+"/requeue", env.ownerToken); rr.Code != http.StatusOK {
		t.Fatalf("owner requeue after the foreign refusal should be 200, got %d %v", rr.Code, errCodeQops(decoded))
	}
}
