package main

// Track 3-d handler tests — knowledge half: auth (401/403), ingest -> list ->
// search flows through the registered middleware chain, the contract's search
// result JSON shape, validation errors, and tenant isolation. All in-memory,
// no infrastructure.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/knowledge"
)

// knowledgeHandlerEnv wires the handler stack and returns bearer tokens for
// one tenant's OWNER/VIEWER plus a foreign tenant's OWNER.
type knowledgeHandlerEnv struct {
	mux         *http.ServeMux
	svc         *knowledge.Service
	orgID       string
	ownerToken  string
	viewerToken string
	otherToken  string
}

func newKnowledgeHandlerEnv(t *testing.T) *knowledgeHandlerEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()

	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	ownerToken, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken(owner) returned error: %v", err)
	}
	viewerToken, err := authSvc.GenerateToken(&auth.User{
		ID: "user-viewer", Organization: owner.Organization, Email: "viewer@acme.test", Role: "VIEWER",
	})
	if err != nil {
		t.Fatalf("GenerateToken(viewer) returned error: %v", err)
	}
	_, foreign, err := authSvc.Register("OtherCo", "owner@other.test", "secret123")
	if err != nil {
		t.Fatalf("Register(foreign) returned error: %v", err)
	}
	otherToken, err := authSvc.GenerateToken(foreign)
	if err != nil {
		t.Fatalf("GenerateToken(foreign) returned error: %v", err)
	}

	svc := knowledge.NewService()
	mux := http.NewServeMux()
	registerKnowledgeRoutes(mux, svc, authSvc, apiKeysSvc)

	return &knowledgeHandlerEnv{
		mux:         mux,
		svc:         svc,
		orgID:       owner.Organization,
		ownerToken:  ownerToken,
		viewerToken: viewerToken,
		otherToken:  otherToken,
	}
}

func (e *knowledgeHandlerEnv) do(t *testing.T, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	var decoded map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
	}
	return rr, decoded
}

func TestKnowledgeIngestListSearchFlow(t *testing.T) {
	env := newKnowledgeHandlerEnv(t)

	rr, decoded := env.do(t, "POST", "/knowledge/documents", env.ownerToken, `{
                "title": "Billing FAQ",
                "content": "Invoices are generated monthly and sent to the billing contact by email.",
                "source": "confluence/billing",
                "metadata": {"team": "billing"}
        }`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /knowledge/documents should be 201, got %d: %v", rr.Code, rr.Body.String())
	}
	doc, ok := decoded["document"].(map[string]any)
	if !ok {
		t.Fatalf("response must carry a document object: %v", decoded)
	}
	for _, key := range []string{"id", "title", "source", "chunk_count", "created_at", "updated_at"} {
		if _, present := doc[key]; !present {
			t.Fatalf("document view missing %q: %v", key, doc)
		}
	}
	if _, leaked := doc["organization_id"]; leaked {
		t.Fatalf("document view must not leak organization_id: %v", doc)
	}
	if doc["title"] != "Billing FAQ" || doc["chunk_count"] != float64(1) {
		t.Fatalf("unexpected document fields: %v", doc)
	}

	rr, decoded = env.do(t, "GET", "/knowledge/documents", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /knowledge/documents should be 200, got %d", rr.Code)
	}
	docs, ok := decoded["documents"].([]any)
	if !ok || len(docs) != 1 {
		t.Fatalf("listing should contain the ingested document: %v", decoded)
	}

	rr, decoded = env.do(t, "POST", "/knowledge/search", env.ownerToken, `{"query": "invoices monthly billing email", "k": 3}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /knowledge/search should be 200, got %d: %v", rr.Code, rr.Body.String())
	}
	results, ok := decoded["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("search should return results: %v", decoded)
	}
	hit, ok := results[0].(map[string]any)
	if !ok {
		t.Fatalf("result must be an object: %v", results[0])
	}
	// Contract-pinned search result shape.
	for _, key := range []string{"document_id", "chunk_ordinal", "content", "score", "citation"} {
		if _, present := hit[key]; !present {
			t.Fatalf("search result missing %q: %v", key, hit)
		}
	}
	if hit["document_id"] != doc["id"] {
		t.Fatalf("result must cite the ingested document: %v vs %v", hit["document_id"], doc["id"])
	}
	if hit["chunk_ordinal"] != float64(0) {
		t.Fatalf("unexpected chunk_ordinal: %v", hit["chunk_ordinal"])
	}
	if score, ok := hit["score"].(float64); !ok || score <= 0 || score > 1 {
		t.Fatalf("score must be in (0,1], got %v", hit["score"])
	}
	if !strings.Contains(hit["citation"].(string), "Billing FAQ") {
		t.Fatalf("citation should name the document: %v", hit["citation"])
	}
}

func TestKnowledgeIngestEmbedFailureReturns201WithWarning(t *testing.T) {
	// Dedicated env whose service embedder always fails: ingest must stay
	// non-fatal and retrieval must fall back to lexical scoring.
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()
	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	ownerToken, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken(owner) returned error: %v", err)
	}
	mux := http.NewServeMux()
	registerKnowledgeRoutes(mux, knowledge.NewServiceWithEmbedder(failingEmbedder{}), authSvc, apiKeysSvc)

	do := func(method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		var decoded map[string]any
		if rr.Body.Len() > 0 {
			_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
		}
		return rr, decoded
	}

	rr, decoded := do("POST", "/knowledge/documents", `{"title": "Doc", "content": "Content stored without vectors."}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("embedder failure must be non-fatal (201), got %d: %v", rr.Code, rr.Body.String())
	}
	if warning, ok := decoded["warning"].(string); !ok || warning == "" {
		t.Fatalf("response should carry the embedder warning: %v", decoded)
	}
	// The unembedded chunk is still searchable via the lexical fallback.
	rr, decoded = do("POST", "/knowledge/search", `{"query": "content stored"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("search after failed embed should be 200, got %d", rr.Code)
	}
	if results, _ := decoded["results"].([]any); len(results) != 1 {
		t.Fatalf("lexical fallback should match the unembedded chunk: %v", decoded)
	}
}

// failingEmbedder always errors (drives the non-fatal ingest warning path).
type failingEmbedder struct{}

func (failingEmbedder) Name() string { return "failing" }

func (failingEmbedder) Embed(_ context.Context, _ []string) ([][]float64, error) {
	return nil, errors.New("embeddings backend down")
}

func TestKnowledgeValidationErrors(t *testing.T) {
	env := newKnowledgeHandlerEnv(t)

	rr, decoded := env.do(t, "POST", "/knowledge/documents", env.ownerToken, `{"title": "  ", "content": "c"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank title must be 422, got %d", rr.Code)
	}
	if code := errCodeKnb(decoded); code != "VALIDATION_ERROR" {
		t.Fatalf("expected VALIDATION_ERROR, got %v", decoded)
	}

	rr, _ = env.do(t, "POST", "/knowledge/documents", env.ownerToken, `not json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body must be 400, got %d", rr.Code)
	}

	rr, decoded = env.do(t, "POST", "/knowledge/search", env.ownerToken, `{"query": "   "}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank query must be 422, got %d", rr.Code)
	}
	if code := errCodeKnb(decoded); code != "VALIDATION_ERROR" {
		t.Fatalf("expected VALIDATION_ERROR, got %v", decoded)
	}
}

func TestKnowledgeAuthAndRBAC(t *testing.T) {
	env := newKnowledgeHandlerEnv(t)

	rr, _ := env.do(t, "GET", "/knowledge/documents", "", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list must be 401, got %d", rr.Code)
	}
	rr, _ = env.do(t, "POST", "/knowledge/documents", env.ownerToken, `{"title":"t","content":"c"}`)
	if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
		t.Fatalf("owner write should pass RBAC, got %d", rr.Code)
	}
	rr, _ = env.do(t, "POST", "/knowledge/documents", env.viewerToken, `{"title":"t","content":"c"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer write must be 403, got %d", rr.Code)
	}
	rr, _ = env.do(t, "GET", "/knowledge/documents", env.viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer read should pass, got %d", rr.Code)
	}
	rr, _ = env.do(t, "POST", "/knowledge/search", env.viewerToken, `{"query":"q"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer search should pass, got %d", rr.Code)
	}
}

func TestKnowledgeTenantIsolation(t *testing.T) {
	env := newKnowledgeHandlerEnv(t)
	env.do(t, "POST", "/knowledge/documents", env.ownerToken,
		`{"title": "Internal", "content": "Private pricing agreement with Acme caps discounts at 40 percent."}`)

	rr, decoded := env.do(t, "GET", "/knowledge/documents", env.otherToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign list should be 200 (empty), got %d", rr.Code)
	}
	if docs, _ := decoded["documents"].([]any); len(docs) != 0 {
		t.Fatalf("cross-tenant document leak: %v", decoded)
	}
	rr, decoded = env.do(t, "POST", "/knowledge/search", env.otherToken, `{"query": "private pricing agreement Acme discounts"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign search should be 200, got %d", rr.Code)
	}
	if results, _ := decoded["results"].([]any); len(results) != 0 {
		t.Fatalf("cross-tenant search leak: %v", decoded)
	}
}

// errCodeKnb extracts error.code from the shared error envelope.
func errCodeKnb(decoded map[string]any) string {
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObj["code"].(string)
	return code
}
