package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/events"
)

// --- service basics -------------------------------------------------------

func TestCreateWebhookReturnsSecretOnceAndStoresHash(t *testing.T) {
	svc := NewService()
	wh, secret, err := svc.CreateWebhook(context.Background(), "org-1", "https://example.com/hook", []string{events.EventRunFailed, events.EventRunCompleted})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if wh.ID == "" || wh.Status != StatusActive {
		t.Fatalf("webhook should be created active, got %+v", wh)
	}
	if secret == "" {
		t.Fatal("secret must be returned exactly once at creation")
	}
	// durable record must carry the hash only
	if wh.SecretHash == "" || wh.SecretHash == secret {
		t.Fatalf("SecretHash should be set and never equal the raw secret: %q", wh.SecretHash)
	}
	if !VerifySecret(wh.SecretHash, secret) {
		t.Error("stored hash should verify against the raw secret")
	}
	if strings.Contains(strings.ToLower(wh.SecretHash), "secret") {
		t.Error("hash should be a hex digest")
	}
	// re-fetch: no raw secret anywhere on the record
	fetched, err := svc.GetWebhook(context.Background(), "org-1", wh.ID)
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if strings.Contains(fetched.SecretHash, secret) {
		t.Error("raw secret leaked through the stored record")
	}
}

func TestCreateWebhookValidation(t *testing.T) {
	svc := NewService()
	cases := []struct {
		name   string
		orgID  string
		url    string
		events []string
	}{
		{"missing org", "", "https://example.com", nil},
		{"bad url", "org-1", "not-a-url", nil},
		{"ftp url", "org-1", "ftp://example.com", nil},
		{"unknown event type", "org-1", "https://example.com", []string{"bogus.event"}},
	}
	for _, tc := range cases {
		if _, _, err := svc.CreateWebhook(context.Background(), tc.orgID, tc.url, tc.events); !errors.Is(err, ErrInvalidWebhook) {
			t.Errorf("%s: expected ErrInvalidWebhook, got %v", tc.name, err)
		}
	}
	// empty events list is a wildcard subscription and must be allowed
	if _, _, err := svc.CreateWebhook(context.Background(), "org-1", "https://example.com", nil); err != nil {
		t.Errorf("empty events should be allowed (wildcard), got %v", err)
	}
}

func TestListAndGetScopedByOrganization(t *testing.T) {
	svc := NewService()
	whA, _, err := svc.CreateWebhook(context.Background(), "org-a", "https://a.example.com", nil)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, _, err := svc.CreateWebhook(context.Background(), "org-b", "https://b.example.com", nil); err != nil {
		t.Fatalf("create B: %v", err)
	}
	listA, err := svc.ListWebhooks(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != whA.ID {
		t.Fatalf("org-a should see exactly its own webhook, got %+v", listA)
	}
	if _, err := svc.GetWebhook(context.Background(), "org-b", whA.ID); !errors.Is(err, ErrWebhookNotFound) {
		t.Error("cross-tenant get must be not-found (tenant guard)")
	}
	if _, err := svc.GetWebhook(context.Background(), "org-a", "missing-id"); !errors.Is(err, ErrWebhookNotFound) {
		t.Error("unknown id must be not-found")
	}
}

func TestDeleteWebhookScoped(t *testing.T) {
	svc := NewService()
	wh, _, err := svc.CreateWebhook(context.Background(), "org-a", "https://a.example.com", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.DeleteWebhook(context.Background(), "org-b", wh.ID); !errors.Is(err, ErrWebhookNotFound) {
		t.Error("cross-tenant delete must be not-found")
	}
	if err := svc.DeleteWebhook(context.Background(), "org-a", wh.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.GetWebhook(context.Background(), "org-a", wh.ID); !errors.Is(err, ErrWebhookNotFound) {
		t.Error("deleted webhook must be gone")
	}
}

func TestWebhooksForEventMatchingAndDisabled(t *testing.T) {
	svc := NewService()
	matching, _, _ := svc.CreateWebhook(context.Background(), "org-1", "https://one.example.com", []string{events.EventRunFailed})
	wildcard, _, _ := svc.CreateWebhook(context.Background(), "org-1", "https://two.example.com", nil)
	other, _, _ := svc.CreateWebhook(context.Background(), "org-1", "https://three.example.com", []string{events.EventAgentCreated})

	matched, err := svc.WebhooksForEvent(context.Background(), "org-1", events.EventRunFailed)
	if err != nil {
		t.Fatalf("WebhooksForEvent: %v", err)
	}
	ids := map[string]bool{}
	for _, wh := range matched {
		ids[wh.ID] = true
	}
	if !ids[matching.ID] || !ids[wildcard.ID] || ids[other.ID] {
		t.Errorf("match set wrong: %+v", ids)
	}

	// disabled endpoints drop out of the match set
	if err := svc.SetWebhookStatus(context.Background(), "org-1", matching.ID, StatusDisabled); err != nil {
		t.Fatalf("SetWebhookStatus: %v", err)
	}
	matched, _ = svc.WebhooksForEvent(context.Background(), "org-1", events.EventRunFailed)
	if len(matched) != 1 || matched[0].ID != wildcard.ID {
		t.Errorf("disabled webhook should be excluded, got %+v", matched)
	}
	if err := svc.SetWebhookStatus(context.Background(), "org-1", matching.ID, StatusActive); err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	if err := svc.SetWebhookStatus(context.Background(), "org-1", matching.ID, "bogus"); !errors.Is(err, ErrInvalidWebhook) {
		t.Error("invalid status must be rejected")
	}
}

// --- HMAC signing ---------------------------------------------------------

func TestSignPayloadKnownVector(t *testing.T) {
	// independent implementation of the pinned scheme
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte("hello body"))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := SignPayload("secret", []byte("hello body")); got != want {
		t.Errorf("SignPayload = %q, want %q", got, want)
	}
	if !VerifyPayload("secret", []byte("hello body"), want) {
		t.Error("VerifyPayload should accept a correct signature")
	}
	if VerifyPayload("secret", []byte("tampered"), want) {
		t.Error("VerifyPayload must reject a signature over different bytes")
	}
	if VerifyPayload("wrong-secret", []byte("hello body"), want) {
		t.Error("VerifyPayload must reject a signature from another secret")
	}
}

func TestDeliverySecretMatchesCreatedSecret(t *testing.T) {
	svc := NewService()
	wh, secret, err := svc.CreateWebhook(context.Background(), "org-1", "https://example.com", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if svc.secretForDelivery(wh) != secret {
		t.Error("worker must be able to re-derive exactly the secret handed out at creation")
	}
	// a different signing key changes derived secrets (documented behavior)
	svc2 := NewService()
	svc2.SetSigningKey("another-key")
	wh2, secret2, _ := svc2.CreateWebhook(context.Background(), "org-1", "https://example.com", nil)
	if svc2.secretForDelivery(wh2) != secret2 {
		t.Error("re-derivation must be stable per signing key")
	}
	if secret2 == secret {
		t.Error("different signing keys must yield different secrets")
	}
}

// --- in-memory delivery records -------------------------------------------

func TestDeliveryRecordsInMemory(t *testing.T) {
	svc := NewService()
	wh, _, err := svc.CreateWebhook(context.Background(), "org-1", "https://example.com", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx := context.Background()
	d1 := &Delivery{ID: "d1", WebhookID: wh.ID, OrganizationID: "org-1", EventID: "e1", EventType: events.EventRunFailed, Status: DeliveryDelivered, Attempts: 1, LastStatusCode: 200, LatencyMS: 12}
	if err := svc.UpsertDelivery(ctx, d1); err != nil {
		t.Fatalf("upsert d1: %v", err)
	}
	// retry updates mutate the same record
	d1.Status = DeliveryFailed
	d1.Attempts = 3
	d1.LastStatusCode = 500
	if err := svc.UpsertDelivery(ctx, d1); err != nil {
		t.Fatalf("upsert d1 retry: %v", err)
	}
	if err := svc.UpsertDelivery(ctx, &Delivery{ID: "d2", WebhookID: wh.ID, OrganizationID: "org-1", EventID: "e2", EventType: events.EventRunCompleted, Status: DeliveryDelivered, Attempts: 1}); err != nil {
		t.Fatalf("upsert d2: %v", err)
	}

	list, err := svc.ListDeliveries(ctx, "org-1", wh.ID, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 deliveries, got %d", len(list))
	}
	if list[0].ID != "d2" || list[1].ID != "d1" {
		t.Errorf("deliveries should be newest first, got %s then %s", list[0].ID, list[1].ID)
	}
	if list[1].Attempts != 3 || list[1].Status != DeliveryFailed || list[1].LastStatusCode != 500 {
		t.Errorf("attempt updates should mutate the same record, got %+v", list[1])
	}
	// tenant guard on deliveries
	if _, err := svc.ListDeliveries(ctx, "org-2", wh.ID, 50); !errors.Is(err, ErrWebhookNotFound) {
		t.Error("cross-tenant delivery listing must be not-found")
	}
	// unknown webhook
	if _, err := svc.ListDeliveries(ctx, "org-1", "missing", 50); !errors.Is(err, ErrWebhookNotFound) {
		t.Error("unknown webhook listing must be not-found")
	}
}

func TestServiceConcurrentAccess(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			wh, _, _ := svc.CreateWebhook(ctx, "org-1", "https://x.example.com", nil)
			_, _ = svc.ListWebhooks(ctx, "org-1")
			_ = svc.DeleteWebhook(ctx, "org-1", wh.ID)
		}(i)
	}
	wg.Wait()
}

// --- event publishing through the memory publisher (integration path) -----

func TestWorkerEndToEndWithMemoryPublisher(t *testing.T) {
	pub := events.NewMemoryPublisher()
	svc := NewService()
	_, _, err := svc.CreateWebhook(context.Background(), "org-1", "http://127.0.0.1:1/nope", nil) // nothing listens; attempts all fail
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fake := &recordingClient{respStatus: 500}
	worker := NewWorker(svc, pub, fake, nil,
		WithBackoff([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = worker.Run(ctx)
		close(done)
	}()
	// wait until the worker's subscription is live before publishing
	subDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(subDeadline) && pub.Subscribers() == 0 {
		time.Sleep(time.Millisecond)
	}

	// emit matching + non-matching events
	if err := pub.Publish(context.Background(), events.NewEvent(events.EventRunFailed, "org-1", "run", "r1", map[string]any{"reason": "boom"})); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := pub.Publish(context.Background(), events.NewEvent(events.EventRunFailed, "org-other", "run", "r2", nil)); err != nil {
		t.Fatalf("publish other tenant: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, _ := svc.ListDeliveries(context.Background(), "org-1", mustFirstWebhookID(t, svc, "org-1"), 50)
		if len(list) == 1 && list[0].Status == DeliveryFailed && list[0].Attempts == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	list, _ := svc.ListDeliveries(context.Background(), "org-1", mustFirstWebhookID(t, svc, "org-1"), 50)
	if len(list) != 1 {
		t.Fatalf("only the tenant-matching event should create a delivery, got %d", len(list))
	}
	d := list[0]
	if d.Status != DeliveryFailed || d.Attempts != 3 {
		t.Errorf("exhausted retries should end failed with 3 attempts, got %+v", d)
	}
	if fake.calls < 3 {
		t.Errorf("expected 3 HTTP attempts, got %d", fake.calls)
	}
}

func mustFirstWebhookID(t *testing.T, svc *Service, orgID string) string {
	t.Helper()
	list, err := svc.ListWebhooks(context.Background(), orgID)
	if err != nil || len(list) == 0 {
		t.Fatalf("expected one webhook for %s", orgID)
	}
	return list[0].ID
}
