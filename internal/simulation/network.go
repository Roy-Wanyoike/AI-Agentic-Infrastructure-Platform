package simulation

// network.go — issue #133 network adapter: a message-graph world with a
// small deterministic vocabulary (send / receive / route) and graph state.
//
// WORLD MODEL (in-memory, one instance per run):
//
//   - messages: id "m-<seq>", FIFO inboxes per agent capped at
//     Params["inbox_depth"] (default 20; overflow drops the OLDEST —
//     documented, countable via rejection-free eviction).
//   - graph: directed edge sender→recipient per send/route, surfaced as
//     an edge count per sender.
//   - hops: route increments the hop count; hops ≥ maxHops (8) is rejected
//     "max_hops" (bounded cascades).
//   - viral_message event (payload {"topic"}): a "system" message lands in
//     EVERY agent's inbox (documented activity injection).
//
// BEHAVIOR RENDERING FROM THE SAME CORE IDENTITY: verb weights derive from
// CoreIdentity.Traits:
//
//      send    = 0.30 + 0.50 × extraversion   (initiating)
//      receive = 0.25 + 0.30 × agreeableness  (consuming)
//      route   = 0.10 + 0.30 × openness       (relaying/exploring)
//
// so the SAME identity with an overlay pinning {"send":1} versus
// {"route":1} diverges observably (senders grow the graph directly;
// routers only relay what they received — cascade behavior).

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"time"
)

// maxHops bounds route cascades; defaultInboxDepth is the inbox cap.
const (
	maxHops           = 8
	defaultInboxDepth = 20
)

// netMessage is one delivered message.
type netMessage struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	To    string `json:"to"`
	Topic string `json:"topic"`
	Hops  int    `json:"hops"`
	Round int    `json:"round"`
	Read  bool   `json:"read"`
}

// NetworkEnvironment is the in-memory message-graph world.
type NetworkEnvironment struct {
	cfg       EnvConfig
	rnd       *rand.Rand
	clock     Clock
	depth     int
	inboxes   map[string][]*netMessage
	edges     map[string]map[string]int // sender -> recipient -> count
	byID      map[string]*netMessage
	agentIDs  []string
	round     int
	seq       int
	viralCnt  int
	delivered int
}

// NewNetworkEnvironment returns an unconfigured network world.
func NewNetworkEnvironment() *NetworkEnvironment {
	return &NetworkEnvironment{
		inboxes: map[string][]*netMessage{},
		edges:   map[string]map[string]int{},
		byID:    map[string]*netMessage{},
	}
}

// Initialize implements SimulationEnvironment (resets all state).
func (e *NetworkEnvironment) Initialize(_ context.Context, cfg EnvConfig) error {
	e.cfg = cfg
	e.rnd = rand.New(rand.NewSource(cfg.Seed))
	e.clock = cfg.Clock
	e.depth = defaultInboxDepth
	if d, err := strconv.Atoi(cfg.Params["inbox_depth"]); err == nil && d > 0 && d <= 1000 {
		e.depth = d
	}
	e.inboxes = map[string][]*netMessage{}
	e.edges = map[string]map[string]int{}
	e.byID = map[string]*netMessage{}
	e.agentIDs = make([]string, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		e.agentIDs = append(e.agentIDs, a.CoreIdentity.ID)
	}
	sort.Strings(e.agentIDs)
	e.round = 0
	e.seq = 0
	e.viralCnt = 0
	e.delivered = 0
	return nil
}

// InjectEvent implements EventInjector: kind "viral_message" delivers a
// system message to every inbox (see file header). Unknown kinds: no-op.
func (e *NetworkEnvironment) InjectEvent(_ context.Context, ev ActivityEvent) error {
	if ev.Kind != "viral_message" {
		return nil
	}
	topic := ev.Payload["topic"]
	if topic == "" {
		topic = fmt.Sprintf("viral-r%d", ev.Round)
	}
	e.viralCnt++
	for _, to := range e.agentIDs {
		e.deliverRound("system", to, topic, 0, ev.Round)
	}
	return nil
}

// deliver appends a message stamped with the CURRENT round.
func (e *NetworkEnvironment) deliver(from, to, topic string, hops int) *netMessage {
	return e.deliverRound(from, to, topic, hops, e.round)
}

// deliverRound appends a message to the recipient's inbox (capped, oldest
// evicted); round is the stamping round (send/route: current round;
// viral injections: the event's scheduled round).
func (e *NetworkEnvironment) deliverRound(from, to, topic string, hops, round int) *netMessage {
	e.seq++
	msg := &netMessage{
		ID:    fmt.Sprintf("m-%d", e.seq),
		From:  from,
		To:    to,
		Topic: topic,
		Hops:  hops,
		Round: round,
	}
	e.byID[msg.ID] = msg
	inbox := append(e.inboxes[to], msg)
	if len(inbox) > e.depth {
		// documented overflow policy: drop the OLDEST unread-or-read head.
		evicted := inbox[0]
		delete(e.byID, evicted.ID)
		inbox = inbox[1:]
	}
	e.inboxes[to] = inbox
	e.delivered++
	return msg
}

// Step implements SimulationEnvironment (each agent proposes one action).
func (e *NetworkEnvironment) Step(ctx context.Context, agents []SimAgent) ([]ActionResult, error) {
	e.round++
	results := make([]ActionResult, 0, len(agents))
	for _, ag := range agents {
		view := e.snapshot()
		act := ag.Decide(ctx, view, e.rnd)
		act.AgentID = ag.Persona().CoreIdentity.ID
		results = append(results, e.apply(act))
	}
	return results, nil
}

// now reads the run clock injected via EnvConfig.
func (e *NetworkEnvironment) now() time.Time {
	if e.clock == nil {
		return RealClock{}.Now()
	}
	return e.clock.Now()
}

// apply executes one action against the world (the only mutation path).
func (e *NetworkEnvironment) apply(act Action) ActionResult {
	at := e.now()
	switch act.Kind {
	case "idle":
		return ActionResult{Action: act, Accepted: false, Reason: "idle", At: at}
	case "send":
		if !e.knownAgent(act.Target) || act.Target == act.AgentID {
			return ActionResult{Action: act, Accepted: false, Reason: "unknown_peer", At: at}
		}
		topic := act.Payload["topic"]
		if topic == "" {
			topic = "general"
		}
		e.deliver(act.AgentID, act.Target, topic, 0)
		e.addEdge(act.AgentID, act.Target)
		return ActionResult{Action: act, Accepted: true, At: at}
	case "receive":
		msg := e.resolveUnread(act.AgentID, act.Target)
		if msg == nil {
			return ActionResult{Action: act, Accepted: false, Reason: "inbox_empty", At: at}
		}
		msg.Read = true
		return ActionResult{Action: act, Accepted: true,
			Detail: fmt.Sprintf("received %s from %s (topic=%s hops=%d)", msg.ID, msg.From, msg.Topic, msg.Hops), At: at}
	case "route":
		msg := e.resolveUnread(act.AgentID, act.Payload["message_id"])
		if msg == nil {
			return ActionResult{Action: act, Accepted: false, Reason: "inbox_empty", At: at}
		}
		if msg.Hops >= maxHops {
			msg.Read = true
			return ActionResult{Action: act, Accepted: false, Reason: "max_hops", At: at}
		}
		if !e.knownAgent(act.Target) || act.Target == act.AgentID || act.Target == msg.From {
			return ActionResult{Action: act, Accepted: false, Reason: "unknown_peer", At: at}
		}
		msg.Read = true // consumed by the relay
		e.deliver(msg.From, act.Target, msg.Topic, msg.Hops+1)
		e.addEdge(act.AgentID, act.Target)
		return ActionResult{Action: act, Accepted: true,
			Detail: fmt.Sprintf("relayed %s (topic=%s hops=%d)", msg.ID, msg.Topic, msg.Hops+1), At: at}
	default:
		return ActionResult{Action: act, Accepted: false, Reason: "unknown_action", At: at}
	}
}

// addEdge records one directed graph edge.
func (e *NetworkEnvironment) addEdge(from, to string) {
	if e.edges[from] == nil {
		e.edges[from] = map[string]int{}
	}
	e.edges[from][to]++
}

// knownAgent reports whether id is one of the configured personas.
func (e *NetworkEnvironment) knownAgent(id string) bool {
	for _, a := range e.agentIDs {
		if a == id {
			return true
		}
	}
	return false
}

// unread returns the agent's unread messages (inbox order, FIFO).
func (e *NetworkEnvironment) unread(agent string) []*netMessage {
	out := make([]*netMessage, 0, len(e.inboxes[agent]))
	for _, m := range e.inboxes[agent] {
		if !m.Read {
			out = append(out, m)
		}
	}
	return out
}

// resolveUnread picks the message an agent's receive/route consumes: the
// explicitly named id when it is one of the agent's UNREAD messages, else
// the head of the FIFO unread queue (documented fallback); nil when empty.
func (e *NetworkEnvironment) resolveUnread(agent, messageID string) *netMessage {
	queue := e.unread(agent)
	if len(queue) == 0 {
		return nil
	}
	if messageID != "" {
		for _, m := range queue {
			if m.ID == messageID && m.To == agent {
				return m
			}
		}
	}
	return queue[0]
}

// snapshot builds the EnvState copy (graph + inbox heads).
func (e *NetworkEnvironment) snapshot() EnvState {
	unreadCounts := map[string]int{}
	inboxViews := map[string]any{}
	for _, id := range e.agentIDs {
		unread := e.unread(id)
		unreadCounts[id] = len(unread)
		head := make([]map[string]any, 0, len(e.inboxes[id]))
		for _, m := range e.inboxes[id] {
			head = append(head, map[string]any{
				"id": m.ID, "from": m.From, "topic": m.Topic, "hops": m.Hops, "read": m.Read,
			})
		}
		inboxViews[id] = head
	}
	edgeTotal := 0
	for _, targets := range e.edges {
		edgeTotal += len(targets)
	}
	peers := append([]string(nil), e.agentIDs...)
	return EnvState{
		Kind:     KindNetwork,
		Round:    e.round,
		AgentIDs: peers,
		Facts: map[string]any{
			"peers":     peers, // genericAgent contract (registry.go); includes self — callers filter
			"edges":     edgeTotal,
			"messages":  e.delivered,
			"unread":    unreadCounts,
			"inbox":     inboxViews,
			"viral_cnt": e.viralCnt,
		},
	}
}

// GetState implements SimulationEnvironment.
func (e *NetworkEnvironment) GetState(context.Context) (EnvState, error) { return e.snapshot(), nil }

// Close implements SimulationEnvironment (in-memory: nothing to release).
func (e *NetworkEnvironment) Close() error { return nil }

// NetworkAgent renders a SimAgentPersona into the network world's policy.
type NetworkAgent struct{ persona SimAgentPersona }

// Persona implements SimAgent.
func (a *NetworkAgent) Persona() SimAgentPersona { return a.persona }

// Decide implements SimAgent: trait-derived weights (documented on the file
// header), peers drawn uniformly from the snapshot (self excluded);
// receive/route target the OLDEST unread message (FIFO contract).
func (a *NetworkAgent) Decide(_ context.Context, view EnvState, rnd *rand.Rand) Action {
	beh := behaviorFor(a.persona, KindNetwork)
	verb := pickWeighted(beh, map[string]float64{
		"send":    0.30 + 0.50*trait(a.persona, "extraversion"),
		"receive": 0.25 + 0.30*trait(a.persona, "agreeableness"),
		"route":   0.10 + 0.30*trait(a.persona, "openness"),
	}, rnd)
	if verb == "" {
		return Action{Kind: "idle"}
	}
	switch verb {
	case "send":
		if peer := pickPeer(view, a.persona.CoreIdentity.ID, "", rnd); peer != "" {
			topic := fmt.Sprintf("topic-%d", rnd.Intn(5)) // documented 5-topic space
			return Action{Kind: "send", Target: peer, Payload: map[string]string{"topic": topic}}
		}
	case "receive":
		if id := oldestUnread(view, a.persona.CoreIdentity.ID); id != "" {
			return Action{Kind: "receive", Target: id}
		}
	case "route":
		if id := oldestUnread(view, a.persona.CoreIdentity.ID); id != "" {
			// relay target: any peer except self and the original sender
			if peer := pickPeer(view, a.persona.CoreIdentity.ID, originalFrom(view, a.persona.CoreIdentity.ID, id), rnd); peer != "" {
				return Action{Kind: "route", Target: peer, Payload: map[string]string{"message_id": id}}
			}
		}
	}
	return Action{Kind: "idle"}
}

// pickPeer draws uniformly over the peers list excluding self and exclude.
func pickPeer(view EnvState, self, exclude string, rnd *rand.Rand) string {
	candidates := make([]string, 0, len(view.AgentIDs))
	for _, id := range view.AgentIDs {
		if id != self && id != exclude {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return candidates[rnd.Intn(len(candidates))]
}

// oldestUnread reads the id of the agent's oldest UNREAD message from the
// snapshot (FIFO contract: the world consumes strictly in inbox order, so
// the first unread entry is always the one a receive/route will consume).
func oldestUnread(view EnvState, agent string) string {
	inbox, ok := view.Facts["unread"].(map[string]int)
	if !ok || inbox[agent] == 0 {
		return ""
	}
	messages, ok := view.Facts["inbox"].(map[string]any)
	if !ok {
		return ""
	}
	own, ok := messages[agent].([]map[string]any)
	if !ok {
		return ""
	}
	for _, m := range own {
		if read, _ := m["read"].(bool); !read {
			id, _ := m["id"].(string)
			return id
		}
	}
	return ""
}

// originalFrom reads the original sender of a message from the snapshot.
func originalFrom(view EnvState, agent, msgID string) string {
	messages, ok := view.Facts["inbox"].(map[string]any)
	if !ok {
		return ""
	}
	own, ok := messages[agent].([]map[string]any)
	if !ok {
		return ""
	}
	for _, m := range own {
		if id, _ := m["id"].(string); id == msgID {
			from, _ := m["from"].(string)
			return from
		}
	}
	return ""
}
