package orchestration

// bus_test.go — issue #108: agent communication bus semantics, all
// deterministic (channels + synchronization; the only wall-clock waits are
// the BOUNDED fallback polls in the goroutine-leak assertions, never used
// for message-passing correctness).

import (
	"context"
	"encoding/json"
	"runtime"
	"sync"
	"testing"
	"time"
)

func testBus(channelCap, ring int) *InProcessCommBus {
	stamp := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	seq := 0
	return NewInProcessCommBus(BusOptions{
		ChannelCapacity: channelCap,
		InboxRing:       ring,
		Now:             func() time.Time { return stamp },
		NewID: func() string {
			seq++
			return "msg-" + string(rune('a'+seq-1))
		},
	})
}

func mustReceive(t *testing.T, b *InProcessCommBus, org, agent string) <-chan AgentMessage {
	t.Helper()
	ch, err := b.Receive(context.Background(), org, agent)
	if err != nil {
		t.Fatalf("Receive(%s, %s): %v", org, agent, err)
	}
	return ch
}

func mustSend(t *testing.T, b *InProcessCommBus, msg AgentMessage) AgentMessage {
	t.Helper()
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if msg.MessageID == "" {
		msg.MessageID = "m-fixed"
	}
	if err := b.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	return msg
}

// TestSendReceiveRoundTrip pins the full AgentMessage contract on the live
// channel path (defaults filled, payload preserved).
func TestSendReceiveRoundTrip(t *testing.T) {
	b := testBus(0, 0)
	ch := mustReceive(t, b, "org-a", "agent-b")
	payload := json.RawMessage(`{"task":"compute 2+2"}`)
	mustSend(t, b, AgentMessage{
		OrgID:       "org-a",
		FromAgentID: "agent-a",
		ToAgentID:   "agent-b",
		RunID:       "run-1",
		MessageType: MessageTypeRequest,
		Payload:     payload,
	})
	select {
	case got := <-ch:
		if got.OrgID != "org-a" || got.FromAgentID != "agent-a" || got.ToAgentID != "agent-b" {
			t.Fatalf("routing fields wrong: %+v", got)
		}
		if got.MessageType != MessageTypeRequest || got.RunID != "run-1" {
			t.Fatalf("type/run wrong: %+v", got)
		}
		if string(got.Payload) != string(payload) {
			t.Fatalf("payload = %s, want %s", got.Payload, payload)
		}
		if got.Timestamp.IsZero() || got.MessageID == "" {
			t.Fatalf("defaults not filled: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no message on the live channel")
	}
	if b.SubscriberCount() != 1 {
		t.Fatalf("subscriber count = %d, want 1", b.SubscriberCount())
	}
}

// TestReceiveIsIdempotent: repeat Receive returns the SAME channel.
func TestReceiveIsIdempotent(t *testing.T) {
	b := testBus(0, 0)
	ch1, err := b.Receive(context.Background(), "org-a", "agent-a")
	if err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	ch2, err := b.Receive(context.Background(), "org-a", "agent-a")
	if err != nil {
		t.Fatalf("second Receive: %v", err)
	}
	if ch1 != ch2 {
		t.Fatal("Receive returned different channels for the same subscriber")
	}
}

// TestSendWithoutSubscriber: unknown targets are a typed error and create
// nothing (no phantom inboxes).
func TestSendWithoutSubscriber(t *testing.T) {
	b := testBus(0, 0)
	err := b.Send(context.Background(), AgentMessage{
		OrgID: "org-a", FromAgentID: "agent-a", ToAgentID: "agent-ghost",
		MessageType: MessageTypeRequest,
	})
	if !isErr(err, ErrNoSubscriber) {
		t.Fatalf("err = %v, want ErrNoSubscriber", err)
	}
	if b.SubscriberCount() != 0 {
		t.Fatalf("phantom subscriber created: %d", b.SubscriberCount())
	}
}

// isErr is a tiny errors.Is helper so the table tests stay flat.
func isErr(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestSendValidation: table-driven invalid-message matrix.
func TestSendValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  AgentMessage
		want error
	}{
		{"missing org", AgentMessage{FromAgentID: "a", ToAgentID: "b", MessageType: MessageTypeRequest}, ErrInvalidMessage},
		{"missing from", AgentMessage{OrgID: "o", ToAgentID: "b", MessageType: MessageTypeRequest}, ErrInvalidMessage},
		{"missing to", AgentMessage{OrgID: "o", FromAgentID: "a", MessageType: MessageTypeRequest}, ErrInvalidMessage},
		{"unknown type", AgentMessage{OrgID: "o", FromAgentID: "a", ToAgentID: "b", MessageType: "gossip"}, ErrInvalidMessage},
		{"nil message", AgentMessage{}, ErrInvalidMessage}, // all fields empty
	} {
		b := testBus(0, 0)
		if err := b.Send(context.Background(), tc.msg); !isErr(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestSendBusFullAtomic: a saturated channel yields typed ErrBusFull and the
// message must NOT appear in the inbox-of-record (atomic delivery).
func TestSendBusFullAtomic(t *testing.T) {
	b := testBus(1, 8) // channel capacity 1
	ch := mustReceive(t, b, "org-a", "agent-a")
	first := mustSend(t, b, AgentMessage{OrgID: "org-a", FromAgentID: "x", ToAgentID: "agent-a", MessageType: MessageTypeRequest})
	err := b.Send(context.Background(), AgentMessage{OrgID: "org-a", FromAgentID: "x", ToAgentID: "agent-a", MessageType: MessageTypeRequest})
	if !isErr(err, ErrBusFull) {
		t.Fatalf("second send err = %v, want ErrBusFull", err)
	}
	// Exactly the first message is receivable; the ring holds exactly it.
	select {
	case got := <-ch:
		if got.MessageID != first.MessageID {
			t.Fatalf("received %s, want %s", got.MessageID, first.MessageID)
		}
	default:
		t.Fatal("first message lost")
	}
	recent, err := b.RecentMessages("org-a", "agent-a", 0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(recent) != 1 || recent[0].MessageID != first.MessageID {
		t.Fatalf("ring must hold exactly the delivered message: %+v", recent)
	}
}

// TestRecentMessagesInboxRing: the retained ring is the inbox of record
// (newest first, bounded, independent of channel consumption).
func TestRecentMessagesInboxRing(t *testing.T) {
	b := testBus(64, 3)
	mustReceive(t, b, "org-a", "agent-a")
	for i := 0; i < 5; i++ {
		mustSend(t, b, AgentMessage{
			OrgID: "org-a", FromAgentID: "x", ToAgentID: "agent-a",
			MessageType: MessageTypeRequest, MessageID: string(rune('1' + i)),
		})
	}
	recent, err := b.RecentMessages("org-a", "agent-a", 0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(recent) != 3 {
		t.Fatalf("ring size = %d, want 3 (bounded)", len(recent))
	}
	// Newest first: the last three sent ids, reversed.
	want := []string{"5", "4", "3"}
	for i, id := range want {
		if recent[i].MessageID != id {
			t.Errorf("recent[%d] = %s, want %s", i, recent[i].MessageID, id)
		}
	}
	limited, _ := b.RecentMessages("org-a", "agent-a", 1)
	if len(limited) != 1 || limited[0].MessageID != "5" {
		t.Fatalf("limit=1 → %+v, want only the newest", limited)
	}
	// Unknown inbox is a typed error.
	if _, err := b.RecentMessages("org-a", "agent-ghost", 0); !isErr(err, ErrNoSubscriber) {
		t.Fatalf("unknown inbox err = %v, want ErrNoSubscriber", err)
	}
}

// TestOrgIsolation: same agent id in two organizations never cross — neither
// on the live channel nor in the inbox of record.
func TestOrgIsolation(t *testing.T) {
	b := testBus(0, 0)
	chA := mustReceive(t, b, "org-a", "agent-x")
	mustReceive(t, b, "org-b", "agent-x")
	mustSend(t, b, AgentMessage{OrgID: "org-a", FromAgentID: "y", ToAgentID: "agent-x", MessageType: MessageTypeRequest})
	select {
	case got := <-chA:
		if got.OrgID != "org-a" {
			t.Fatalf("org-a inbox got foreign message: %+v", got)
		}
	default:
		t.Fatal("org-a inbox lost its message")
	}
	recentB, _ := b.RecentMessages("org-b", "agent-x", 0)
	if len(recentB) != 0 {
		t.Fatalf("org-b inbox leaked org-a traffic: %+v", recentB)
	}
}

// TestBroadcastFanOutAndIsolation: fan-out reaches every OTHER subscriber of
// the org, excludes the sender, and never crosses organizations.
func TestBroadcastFanOutAndIsolation(t *testing.T) {
	b := testBus(0, 0)
	chA1 := mustReceive(t, b, "org-a", "agent-1")
	chA2 := mustReceive(t, b, "org-a", "agent-2")
	chA3 := mustReceive(t, b, "org-a", "agent-3")
	chB := mustReceive(t, b, "org-b", "agent-b1")
	payload := json.RawMessage(`{"hello":"team"}`)
	if err := b.Broadcast(context.Background(), "org-a", "agent-1", payload); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	for name, ch := range map[string]<-chan AgentMessage{"agent-2": chA2, "agent-3": chA3} {
		select {
		case got := <-ch:
			if got.FromAgentID != "agent-1" || got.MessageType != MessageTypeBroadcast || got.ToAgentID != name {
				t.Fatalf("%s broadcast delivery wrong: %+v", name, got)
			}
			if string(got.Payload) != string(payload) {
				t.Fatalf("%s payload = %s", name, got.Payload)
			}
		default:
			t.Fatalf("%s missed the broadcast", name)
		}
	}
	select {
	case got := <-chA1:
		t.Fatalf("sender must not self-echo: %+v", got)
	default:
	}
	select {
	case got := <-chB:
		t.Fatalf("org-b must not receive org-a broadcast: %+v", got)
	default:
	}
	// Broadcast into an org with no other subscribers is a no-op success.
	if err := b.Broadcast(context.Background(), "org-empty", "agent-z", nil); err != nil {
		t.Fatalf("empty-org broadcast: %v", err)
	}
	// Broadcast validation.
	if err := b.Broadcast(context.Background(), "", "agent-1", nil); !isErr(err, ErrInvalidMessage) {
		t.Fatalf("missing org broadcast err = %v", err)
	}
}

// TestBroadcastSkipsSaturatedPeer: a full channel is skipped, peers still
// receive, and the skipped message stays out of the ring.
func TestBroadcastSkipsSaturatedPeer(t *testing.T) {
	b := testBus(1, 8)
	mustReceive(t, b, "org-a", "agent-sender")
	chFull := mustReceive(t, b, "org-a", "agent-full")
	chOk := mustReceive(t, b, "org-a", "agent-ok")
	// Saturate agent-full's channel (capacity 1).
	prefill := mustSend(t, b, AgentMessage{OrgID: "org-a", FromAgentID: "z", ToAgentID: "agent-full", MessageType: MessageTypeRequest})
	if err := b.Broadcast(context.Background(), "org-a", "agent-sender", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	// The saturated peer holds ONLY its pre-fill message (nothing from the
	// broadcast reached channel or ring).
	if got, ok := <-chFull; !ok || got.MessageID != prefill.MessageID {
		t.Fatalf("pre-fill message wrong: %+v ok=%v", got, ok)
	}
	select {
	case got := <-chFull:
		t.Fatalf("saturated peer must be skipped by broadcasts: %+v", got)
	default:
	}
	select {
	case got := <-chOk:
		if got.FromAgentID != "agent-sender" {
			t.Fatalf("healthy peer delivery wrong: %+v", got)
		}
	default:
		t.Fatal("healthy peer missed the broadcast")
	}
	ring, _ := b.RecentMessages("org-a", "agent-full", 0)
	if len(ring) != 1 {
		t.Fatalf("saturated peer ring must hold only its direct message: %d", len(ring))
	}
}

// TestUnregisterLifecycle: Unregister closes the channel (drain what was
// buffered, then observe the close), rejects further sends and double
// unregistration.
func TestUnregisterLifecycle(t *testing.T) {
	b := testBus(4, 8)
	ch := mustReceive(t, b, "org-a", "agent-a")
	sent := mustSend(t, b, AgentMessage{OrgID: "org-a", FromAgentID: "x", ToAgentID: "agent-a", MessageType: MessageTypeRequest})
	if err := b.Unregister("org-a", "agent-a"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if b.SubscriberCount() != 0 {
		t.Fatalf("subscriber count after unregister = %d, want 0", b.SubscriberCount())
	}
	// Buffered messages are still drained, then the channel is closed.
	if got, ok := <-ch; !ok || got.MessageID != sent.MessageID {
		t.Fatalf("drain after unregister: got %+v ok=%v", got, ok)
	}
	if got, ok := <-ch; ok || got.MessageID != "" {
		t.Fatalf("channel must be closed after drain: %+v ok=%v", got, ok)
	}
	err := b.Send(context.Background(), AgentMessage{OrgID: "org-a", FromAgentID: "x", ToAgentID: "agent-a", MessageType: MessageTypeRequest})
	if !isErr(err, ErrNoSubscriber) {
		t.Fatalf("send after unregister err = %v, want ErrNoSubscriber", err)
	}
	if err := b.Unregister("org-a", "agent-a"); !isErr(err, ErrNoSubscriber) {
		t.Fatalf("double unregister err = %v, want ErrNoSubscriber", err)
	}
}

// TestCloseBus: Close closes every channel and rejects all further traffic.
func TestCloseBus(t *testing.T) {
	b := testBus(0, 0)
	ch := mustReceive(t, b, "org-a", "agent-a")
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := <-ch; ok {
		t.Fatal("subscriber channel must be closed by Close")
	}
	err := b.Send(context.Background(), AgentMessage{OrgID: "org-a", FromAgentID: "x", ToAgentID: "agent-a", MessageType: MessageTypeRequest})
	if !isErr(err, ErrBusClosed) {
		t.Fatalf("send after Close err = %v, want ErrBusClosed", err)
	}
	if _, err := b.Receive(context.Background(), "org-a", "agent-a"); !isErr(err, ErrBusClosed) {
		t.Fatalf("receive after Close err = %v, want ErrBusClosed", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("double Close must be idempotent, got %v", err)
	}
}

// TestContextAwareReceiveAndSend: an already-canceled context fails fast.
func TestContextAwareReceiveAndSend(t *testing.T) {
	b := testBus(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Receive(ctx, "org-a", "agent-a"); err == nil {
		t.Fatal("Receive with canceled context must fail")
	}
	err := b.Send(ctx, AgentMessage{OrgID: "org-a", FromAgentID: "a", ToAgentID: "b", MessageType: MessageTypeRequest})
	if err == nil {
		t.Fatal("Send with canceled context must fail")
	}
}

// TestNoGoroutineLeaks: consumers that exit via channel close leave no
// goroutines behind (NumGoroutine before == after, with a bounded fallback
// poll for scheduler epilogues — never for message-passing).
func TestNoGoroutineLeaks(t *testing.T) {
	b := testBus(8, 8)
	before := runtime.NumGoroutine()
	const consumers = 4
	// Register the subscriber FIRST so consumer goroutines can never race
	// the teardown unregister (deterministic lifecycle).
	if _, err := b.Receive(context.Background(), "org-a", "agent-a"); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < consumers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, err := b.Receive(context.Background(), "org-a", "agent-a")
			if err != nil {
				return
			}
			for {
				select {
				case <-stop:
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
				}
			}
		}()
	}
	// Producer traffic (best-effort: consumers may not have drained yet —
	// this test asserts goroutine lifecycle, not delivery totals).
	for i := 0; i < 16; i++ {
		_ = b.Send(context.Background(), AgentMessage{OrgID: "org-a", FromAgentID: "p", ToAgentID: "agent-a", MessageType: MessageTypeRequest})
	}
	// Deterministic teardown: close the subscriber channel via Unregister,
	// then stop the fallback loop and wait for every consumer to EXIT.
	if err := b.Unregister("org-a", "agent-a"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	close(stop)
	wg.Wait()
	// Bounded epilogue poll (scheduler bookkeeping only).
	for i := 0; i < 200; i++ {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("goroutine leak: before=%d after=%d", before, runtime.NumGoroutine())
}

// TestConcurrentSendBroadcastReceive: hammer one bus from many senders,
// a broadcaster and a drain loop — race detector target (run with -race).
func TestConcurrentSendBroadcastReceive(t *testing.T) {
	b := testBus(256, 256)
	ch := mustReceive(t, b, "org-a", "agent-a")
	_ = mustReceive(t, b, "org-a", "agent-b") // broadcast peer (undrained; Close reaps it)
	drainDone := make(chan int)
	go func() {
		n := 0
		for range ch {
			n++
		}
		drainDone <- n
	}()
	const senders = 8
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_ = b.Send(context.Background(), AgentMessage{
					OrgID: "org-a", FromAgentID: "s", ToAgentID: "agent-a",
					MessageType: MessageTypeRequest,
				})
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 25; j++ {
			_ = b.Broadcast(context.Background(), "org-a", "agent-a", nil)
		}
	}()
	wg.Wait()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The drain loop exits when Close closes the channel; the exact total is
	// load-dependent (senders may hit a full channel under contention) — the
	// guarantee under test is clean shutdown with no race/deadlock.
	if n := <-drainDone; n == 0 {
		t.Fatal("drain loop observed zero messages")
	}
}
