package simulation

// social.go — issue #133 social adapter: a public-timeline world with a
// small deterministic vocabulary (post / like / follow / comment) and
// feed state.
//
// WORLD MODEL (in-memory, one instance per run):
//
//   - posts: id `{authorID}:{seq}`, append-only; seq is per-author.
//   - feed: the most recent `feed_depth` posts (Params["feed_depth"], default
//     20), newest first — a PUBLIC timeline, so an agent's own posts are
//     visible to itself (this is what makes self-interaction — the echo
//     chamber signal — expressible).
//   - follow: directed edges agent→agent, surfaced as per-agent counts.
//   - trending_topic events pin a synthetic "system" post at the front of
//     the feed for the round (documented activity injection).
//
// BEHAVIOR RENDERING FROM THE SAME CORE IDENTITY (issue #133 contract):
// the verb weights below are derived from CoreIdentity.Traits, so two
// personas with the SAME identity but DIFFERENT PlatformBehavior overlays
// diverge purely through the overlay (weights/actions/frequency), while
// two personas with the same overlay but different traits diverge through
// the documented trait mapping:
//
//      post    = 0.20 + 0.40 × extraversion   (initiating content)
//      comment = 0.15 + 0.25 × extraversion   (responding with content)
//      like    = 0.25 + 0.35 × agreeableness  (low-cost reciprocity)
//      follow  = 0.15 + 0.20 × openness       (exploring new accounts)
//
// An explicit PlatformBehavior.Weights entry REPLACES the trait-derived
// weight for that verb (normalized together with the remaining weights).

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultFeedDepth is the public-timeline depth when Params["feed_depth"]
// is absent or unparseable.
const defaultFeedDepth = 20

// SocialPost is one post in the world (snapshot rendering only).
type SocialPost struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Round   int    `json:"round"`
	Content string `json:"content"`
	Likes   int    `json:"likes"`
}

// SocialComment is one comment attached to a post.
type SocialComment struct {
	AgentID string `json:"agent_id"`
	Round   int    `json:"round"`
	Text    string `json:"text"`
}

// SocialEnvironment is the in-memory social world.
type SocialEnvironment struct {
	cfg      EnvConfig
	rnd      *rand.Rand
	feedDept int

	seq           map[string]int // authorID -> next post seq
	posts         []*SocialPost
	byID          map[string]*SocialPost
	comments      map[string][]SocialComment // postID -> comments
	follows       map[string]map[string]bool // agentID -> followed agentIDs
	round         int
	trending      string // trending topic injected for trendingRound
	trendingRound int
	trendCnt      int
}

// NewSocialEnvironment returns an unconfigured world (Initialize next).
func NewSocialEnvironment() *SocialEnvironment {
	return &SocialEnvironment{
		seq:      map[string]int{},
		byID:     map[string]*SocialPost{},
		comments: map[string][]SocialComment{},
		follows:  map[string]map[string]bool{},
	}
}

// Initialize implements SimulationEnvironment (resets all state).
func (e *SocialEnvironment) Initialize(_ context.Context, cfg EnvConfig) error {
	e.cfg = cfg
	e.rnd = rand.New(rand.NewSource(cfg.Seed))
	e.feedDept = defaultFeedDepth
	if d, err := strconv.Atoi(cfg.Params["feed_depth"]); err == nil && d > 0 && d <= 1000 {
		e.feedDept = d
	}
	e.seq = map[string]int{}
	e.posts = nil
	e.byID = map[string]*SocialPost{}
	e.comments = map[string][]SocialComment{}
	e.follows = map[string]map[string]bool{}
	e.round = 0
	e.trending = ""
	e.trendingRound = 0
	e.trendCnt = 0
	return nil
}

// InjectEvent implements EventInjector: kind "trending_topic" records a
// synthetic system post (payload {"topic": ...}) AND pins the "trending"
// fact for exactly the event's scheduled round (see snapshot). Unknown
// kinds are recorded as no-ops (accepted, countable).
func (e *SocialEnvironment) InjectEvent(_ context.Context, ev ActivityEvent) error {
	if ev.Kind != "trending_topic" {
		return nil
	}
	topic := ev.Payload["topic"]
	if topic == "" {
		topic = fmt.Sprintf("trend-r%d", ev.Round)
	}
	e.trending = topic
	e.trendingRound = ev.Round
	e.trendCnt++
	post := &SocialPost{
		ID:      fmt.Sprintf("trend:%d", e.trendCnt),
		Author:  "system",
		Round:   ev.Round,
		Content: "trending: " + topic,
	}
	e.posts = append(e.posts, post)
	e.byID[post.ID] = post
	return nil
}

// Step implements SimulationEnvironment: each agent proposes one action,
// applied in list order. Results align 1:1 with the agents argument.
func (e *SocialEnvironment) Step(ctx context.Context, agents []SimAgent) ([]ActionResult, error) {
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

// now reads the run clock injected via EnvConfig (determinism seam).
func (e *SocialEnvironment) now() time.Time {
	if e.cfg.Clock == nil {
		return RealClock{}.Now()
	}
	return e.cfg.Clock.Now()
}

// apply executes one action against the world (the only mutation path).
func (e *SocialEnvironment) apply(act Action) ActionResult {
	at := e.now()
	switch act.Kind {
	case "idle":
		return ActionResult{Action: act, Accepted: false, Reason: "idle", At: at}
	case "post":
		e.seq[act.AgentID]++
		post := &SocialPost{
			ID:      fmt.Sprintf("%s:%d", act.AgentID, e.seq[act.AgentID]),
			Author:  act.AgentID,
			Round:   e.round,
			Content: act.Payload["content"],
			// deterministic default content (documented): no payload -> "post by <id> r<round>"
		}
		if post.Content == "" {
			post.Content = fmt.Sprintf("post by %s r%d", act.AgentID, e.round)
		}
		e.posts = append(e.posts, post)
		e.byID[post.ID] = post
		return ActionResult{Action: act, Accepted: true, Detail: "posted " + post.ID, At: at}
	case "like":
		post, ok := e.byID[act.Target]
		if !ok {
			return ActionResult{Action: act, Accepted: false, Reason: "unknown_post", At: at}
		}
		post.Likes++
		return ActionResult{Action: act, Accepted: true, Detail: fmt.Sprintf("likes=%d", post.Likes), At: at}
	case "comment":
		post, ok := e.byID[act.Target]
		if !ok {
			return ActionResult{Action: act, Accepted: false, Reason: "unknown_post", At: at}
		}
		text := act.Payload["text"]
		if text == "" {
			text = fmt.Sprintf("re by %s r%d", act.AgentID, e.round)
		}
		e.comments[post.ID] = append(e.comments[post.ID], SocialComment{AgentID: act.AgentID, Round: e.round, Text: text})
		return ActionResult{Action: act, Accepted: true, Detail: fmt.Sprintf("comments=%d", len(e.comments[post.ID])), At: at}
	case "follow":
		if act.Target == act.AgentID {
			return ActionResult{Action: act, Accepted: false, Reason: "self_follow", At: at}
		}
		if !e.knownAgent(act.Target) {
			return ActionResult{Action: act, Accepted: false, Reason: "unknown_agent", At: at}
		}
		if e.follows[act.AgentID] == nil {
			e.follows[act.AgentID] = map[string]bool{}
		}
		if e.follows[act.AgentID][act.Target] {
			return ActionResult{Action: act, Accepted: false, Reason: "already_following", At: at}
		}
		e.follows[act.AgentID][act.Target] = true
		return ActionResult{Action: act, Accepted: true, At: at}
	default:
		return ActionResult{Action: act, Accepted: false, Reason: "unknown_action", At: at}
	}
}

// knownAgent reports whether id is one of the configured personas.
func (e *SocialEnvironment) knownAgent(id string) bool {
	for _, a := range e.cfg.Agents {
		if a.CoreIdentity.ID == id {
			return true
		}
	}
	return false
}

// snapshot builds the EnvState copy (feed newest-first, capped).
func (e *SocialEnvironment) snapshot() EnvState {
	feed := make([]map[string]any, 0, e.feedDept)
	for i := len(e.posts) - 1; i >= 0 && len(feed) < e.feedDept; i-- {
		p := e.posts[i]
		feed = append(feed, map[string]any{
			"id": p.ID, "author": p.Author, "round": p.Round, "likes": p.Likes,
		})
	}
	agentIDs := make([]string, 0, len(e.cfg.Agents))
	followCounts := map[string]int{}
	for _, a := range e.cfg.Agents {
		agentIDs = append(agentIDs, a.CoreIdentity.ID)
		followCounts[a.CoreIdentity.ID] = len(e.follows[a.CoreIdentity.ID])
	}
	sort.Strings(agentIDs)
	commentTotal := 0
	for _, cs := range e.comments {
		commentTotal += len(cs)
	}
	return EnvState{
		Kind:     KindSocial,
		Round:    e.round,
		AgentIDs: agentIDs,
		Facts: map[string]any{
			"feed":          feed,
			"post_count":    len(e.posts),
			"comment_count": commentTotal,
			"like_count":    e.countLikes(),
			"follow_counts": followCounts,
			// the trending fact is true only for the injected round; the
			// trend POST itself stays in the timeline (recency feed).
			"trending": e.trendingNow(),
			"peers":    agentIDs, // genericAgent contract (registry.go)
		},
	}
}

// trendingNow reports the topic while its round is current.
func (e *SocialEnvironment) trendingNow() string {
	if e.trendingRound == e.round {
		return e.trending
	}
	return ""
}

// countLikes totals likes across posts.
func (e *SocialEnvironment) countLikes() int {
	n := 0
	for _, p := range e.posts {
		n += p.Likes
	}
	return n
}

// GetState implements SimulationEnvironment.
func (e *SocialEnvironment) GetState(context.Context) (EnvState, error) { return e.snapshot(), nil }

// Close implements SimulationEnvironment (in-memory: nothing to release).
func (e *SocialEnvironment) Close() error { return nil }

// SocialAgent renders a SimAgentPersona into the social world's policy.
type SocialAgent struct{ persona SimAgentPersona }

// Persona implements SimAgent.
func (a *SocialAgent) Persona() SimAgentPersona { return a.persona }

// Decide implements SimAgent: trait-derived weights (documented on the file
// header), target = uniform draw over the visible feed's post ids.
func (a *SocialAgent) Decide(_ context.Context, view EnvState, rnd *rand.Rand) Action {
	beh := behaviorFor(a.persona, KindSocial)
	verb := pickWeighted(beh, map[string]float64{
		"post":    0.20 + 0.40*trait(a.persona, "extraversion"),
		"comment": 0.15 + 0.25*trait(a.persona, "extraversion"),
		"like":    0.25 + 0.35*trait(a.persona, "agreeableness"),
		"follow":  0.15 + 0.20*trait(a.persona, "openness"),
	}, rnd)
	if verb == "" {
		return Action{Kind: "idle"}
	}
	switch verb {
	case "post":
		return Action{Kind: "post", Payload: map[string]string{
			"content": fmt.Sprintf("%s r%d", strings.TrimSpace(a.persona.CoreIdentity.Name), view.Round),
		}}
	case "like", "comment":
		if id := pickFeedTarget(view, rnd); id != "" {
			return Action{Kind: verb, Target: id}
		}
		// nothing visible to interact with -> post instead (documented fallback)
		return a.postAction(view)
	case "follow":
		if target := pickFollowTarget(view, a.persona.CoreIdentity.ID, rnd); target != "" {
			return Action{Kind: "follow", Target: target}
		}
		return a.postAction(view)
	default:
		return Action{Kind: verb}
	}
}

// postAction is the documented fallback when the chosen interaction verb
// has no viable target (empty feed / no followable agents).
func (a *SocialAgent) postAction(view EnvState) Action {
	return Action{Kind: "post", Payload: map[string]string{
		"content": fmt.Sprintf("%s r%d", strings.TrimSpace(a.persona.CoreIdentity.Name), view.Round),
	}}
}

// pickFeedTarget draws uniformly over the feed snapshot's post ids.
func pickFeedTarget(view EnvState, rnd *rand.Rand) string {
	feed, ok := view.Facts["feed"].([]map[string]any)
	if !ok || len(feed) == 0 {
		return ""
	}
	ids := make([]string, 0, len(feed))
	for _, f := range feed {
		if id, ok := f["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return ids[rnd.Intn(len(ids))]
}

// pickFollowTarget draws uniformly over the OTHER agents (excluding self
// and anyone already followed — re-follows are rejected by the world).
func pickFollowTarget(view EnvState, self string, rnd *rand.Rand) string {
	// Candidates are every OTHER configured agent. The agent does not track
	// its own edges, so a redraw of an already-followed agent yields the
	// world's honest "already_following" rejection — a countable outcome,
	// never a silent success (documented).
	candidates := make([]string, 0, len(view.AgentIDs))
	for _, id := range view.AgentIDs {
		if id != self {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return candidates[rnd.Intn(len(candidates))]
}
