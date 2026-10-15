package simulation

// market.go — issue #133 market adapter: a deterministic price-time order
// book world with a small vocabulary (buy / sell / bid) and full book +
// ledger state.
//
// WORLD MODEL (in-memory, one instance per run; a documented
// clearing-house microstructure — reservations make every fill exact):
//
//   - symbols: Params["symbols"] (comma list, default "AGENTOS,SYNTH",
//     max 16). Initial mid: Params["price_<SYM>"] (default 100.00).
//   - cash:    Params["starting_cash"] per agent (default 10000).
//   - bid:     resting LIMIT buy. Reserves cash up-front (cash moves into
//     the book at rest time; bids never expire — there is no cancel verb).
//   - sell:    MARKET sell. Requires holdings ≥ qty ("insufficient_holdings");
//     the FULL qty is reserved (holdings move into the book), fills consume
//     the best bids (highest price, FIFO), and any remainder rests on the
//     ask side at the current mid — unmatched supply becomes liquidity.
//   - buy:     MARKET buy. Fills against resting asks (lowest price, FIFO);
//     no asks -> rejected "no_liquidity" (demand does not rest).
//   - fill:    price = resting order's price; buyer cash decreases, seller
//     cash increases, holdings transfer; trades are recorded.
//   - mid:     recomputed per round: (best_bid+best_ask)/2 when both sides
//     exist, else last trade price, else the initial price.
//   - price_shock event: mid *= 1 + pct/100 (payload {"symbol","pct"},
//     negative pct allowed, floor 0.01). Resting orders are NOT repriced.
//
// BEHAVIOR RENDERING FROM THE SAME CORE IDENTITY: verb weights derive from
// CoreIdentity.Traits["risk_appetite"] (r):
//
//      buy  = 0.15 + 0.60 × r          (aggression)
//      sell = 0.15 + 0.40 × (1 - r)    (de-risking)
//      bid  = 0.25                     (passive liquidity provision)
//
// so the SAME identity with an overlay pinning {"buy":1} versus {"sell":1}
// diverges observably (buy-heavy agents meet "no_liquidity" until sellers
// appear; sell-heavy agents rest asks). Explicit Weights replace defaults.

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

// marketDefaults are the documented knobs.
const (
	defaultStartingCash = 10000.0
	defaultMarketPrice  = 100.0
	maxSymbols          = 16
	maxRecentTrades     = 10
)

// marketOrder is one resting order on the book.
type marketOrder struct {
	id      string
	agentID string
	symbol  string
	side    string // "bid" | "ask"
	price   float64
	qty     int
	round   int
	seq     int
}

// marketTrade is one executed fill.
type marketTrade struct {
	Round  int     `json:"round"`
	Symbol string  `json:"symbol"`
	Buyer  string  `json:"buyer"`
	Seller string  `json:"seller"`
	Price  float64 `json:"price"`
	Qty    int     `json:"qty"`
}

// MarketEnvironment is the in-memory market world.
type MarketEnvironment struct {
	cfg      EnvConfig
	rnd      *rand.Rand
	clock    Clock
	symbols  []string
	mid      map[string]float64
	bids     map[string][]*marketOrder
	asks     map[string][]*marketOrder
	cash     map[string]float64
	holdings map[string]map[string]int
	trades   []marketTrade
	seq      int
	round    int
	shocks   int
}

// NewMarketEnvironment returns an unconfigured market world.
func NewMarketEnvironment() *MarketEnvironment {
	return &MarketEnvironment{
		mid:      map[string]float64{},
		bids:     map[string][]*marketOrder{},
		asks:     map[string][]*marketOrder{},
		cash:     map[string]float64{},
		holdings: map[string]map[string]int{},
	}
}

// Initialize implements SimulationEnvironment (resets all state).
func (e *MarketEnvironment) Initialize(_ context.Context, cfg EnvConfig) error {
	e.cfg = cfg
	e.rnd = rand.New(rand.NewSource(cfg.Seed))
	e.clock = cfg.Clock
	e.symbols = nil
	e.mid = map[string]float64{}
	e.bids = map[string][]*marketOrder{}
	e.asks = map[string][]*marketOrder{}
	e.cash = map[string]float64{}
	e.holdings = map[string]map[string]int{}
	e.trades = nil
	e.seq = 0
	e.round = 0
	e.shocks = 0

	cash := defaultStartingCash
	if c, err := strconv.ParseFloat(cfg.Params["starting_cash"], 64); err == nil && c >= 0 {
		cash = c
	}
	for _, sym := range e.symbolList(cfg) {
		price := defaultMarketPrice
		if p, err := strconv.ParseFloat(cfg.Params["price_"+strings.ToLower(sym)], 64); err == nil && p > 0 {
			price = p
		}
		e.symbols = append(e.symbols, sym)
		e.mid[sym] = price
	}
	for _, a := range cfg.Agents {
		id := a.CoreIdentity.ID
		e.cash[id] = cash
		e.holdings[id] = map[string]int{}
	}
	return nil
}

// symbolList parses Params["symbols"] (documented defaults + caps).
func (e *MarketEnvironment) symbolList(cfg EnvConfig) []string {
	raw := strings.TrimSpace(cfg.Params["symbols"])
	if raw == "" {
		return []string{"AGENTOS", "SYNTH"}
	}
	seen := map[string]bool{}
	out := make([]string, 0, maxSymbols)
	for _, s := range strings.Split(raw, ",") {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == maxSymbols {
			break
		}
	}
	if len(out) == 0 {
		return []string{"AGENTOS", "SYNTH"}
	}
	return out
}

// InjectEvent implements EventInjector: kind "price_shock" moves a symbol's
// mid by payload pct (see file header). Unknown kinds are no-ops.
func (e *MarketEnvironment) InjectEvent(_ context.Context, ev ActivityEvent) error {
	if ev.Kind != "price_shock" {
		return nil
	}
	sym := strings.ToUpper(strings.TrimSpace(ev.Payload["symbol"]))
	if _, ok := e.mid[sym]; !ok {
		return nil // unknown symbol: recorded as a countable no-op
	}
	pct, err := strconv.ParseFloat(ev.Payload["pct"], 64)
	if err != nil {
		return nil
	}
	m := e.mid[sym] * (1 + pct/100)
	if m < 0.01 {
		m = 0.01
	}
	e.mid[sym] = m
	e.shocks++
	return nil
}

// Step implements SimulationEnvironment (round-scoped: mid refresh).
func (e *MarketEnvironment) Step(ctx context.Context, agents []SimAgent) ([]ActionResult, error) {
	e.round++
	results := make([]ActionResult, 0, len(agents))
	for _, ag := range agents {
		view := e.snapshot()
		act := ag.Decide(ctx, view, e.rnd)
		act.AgentID = ag.Persona().CoreIdentity.ID
		results = append(results, e.apply(act))
	}
	e.refreshMids()
	return results, nil
}

// now reads the run clock injected via EnvConfig.
func (e *MarketEnvironment) now() time.Time {
	if e.clock == nil {
		return RealClock{}.Now()
	}
	return e.clock.Now()
}

// apply executes one action against the world (the only mutation path).
func (e *MarketEnvironment) apply(act Action) ActionResult {
	at := e.now()
	switch act.Kind {
	case "idle":
		return ActionResult{Action: act, Accepted: false, Reason: "idle", At: at}
	case "buy":
		return e.marketBuy(act, at)
	case "sell":
		return e.marketSell(act, at)
	case "bid":
		return e.limitBid(act, at)
	default:
		return ActionResult{Action: act, Accepted: false, Reason: "unknown_action", At: at}
	}
}

// qtyOf parses the action qty (payload "qty", default 1, cap 100).
func qtyOf(act Action) int {
	q, err := strconv.Atoi(act.Payload["qty"])
	if err != nil || q < 1 {
		return 1
	}
	if q > 100 {
		return 100
	}
	return q
}

// marketBuy fills qty against the lowest asks (FIFO); chunk-wise while
// affordable; rejects when the book has no asks or cash cannot cover one unit.
func (e *MarketEnvironment) marketBuy(act Action, at time.Time) ActionResult {
	sym := strings.ToUpper(strings.TrimSpace(act.Target))
	if _, ok := e.mid[sym]; !ok {
		return ActionResult{Action: act, Accepted: false, Reason: "unknown_symbol", At: at}
	}
	qty := qtyOf(act)
	book := e.asks[sym]
	if len(book) == 0 {
		return ActionResult{Action: act, Accepted: false, Reason: "no_liquidity", At: at}
	}
	filled := 0
	for filled < qty && len(book) > 0 {
		ask := book[0]
		cash := e.cash[act.AgentID]
		if cash < ask.price {
			break // cannot afford the best remaining ask
		}
		take := ask.qty
		if cost := ask.price * float64(take); cost > cash {
			take = int(cash / ask.price) // partial by affordability
			if take < 1 {
				break
			}
		}
		if take > qty-filled {
			take = qty - filled
		}
		e.cash[act.AgentID] -= ask.price * float64(take)
		e.cash[ask.agentID] += ask.price * float64(take)
		e.holdings[act.AgentID][sym] += take
		e.recordTrade(sym, act.AgentID, ask.agentID, ask.price, take)
		filled += take
		ask.qty -= take
		if ask.qty == 0 {
			book = book[1:]
		}
	}
	e.asks[sym] = book
	if filled == 0 {
		return ActionResult{Action: act, Accepted: false, Reason: "insufficient_cash", At: at}
	}
	return ActionResult{Action: act, Accepted: true,
		Detail: fmt.Sprintf("bought %d/%d %s", filled, qty, sym), At: at}
}

// marketSell reserves the full qty (holdings move into the book), fills the
// best bids, and rests the remainder as an ask at the current mid.
func (e *MarketEnvironment) marketSell(act Action, at time.Time) ActionResult {
	sym := strings.ToUpper(strings.TrimSpace(act.Target))
	if _, ok := e.mid[sym]; !ok {
		return ActionResult{Action: act, Accepted: false, Reason: "unknown_symbol", At: at}
	}
	qty := qtyOf(act)
	if e.holdings[act.AgentID][sym] < qty {
		return ActionResult{Action: act, Accepted: false, Reason: "insufficient_holdings", At: at}
	}
	e.holdings[act.AgentID][sym] -= qty // full reservation (clearing-house model)
	filled := 0
	book := e.bids[sym]
	for filled < qty && len(book) > 0 {
		bid := book[0]
		take := bid.qty
		if take > qty-filled {
			take = qty - filled
		}
		e.cash[act.AgentID] += bid.price * float64(take)
		e.cash[bid.agentID] -= bid.price * float64(take) // reserved cash settles
		e.holdings[bid.agentID][sym] += take
		e.recordTrade(sym, bid.agentID, act.AgentID, bid.price, take)
		filled += take
		bid.qty -= take
		if bid.qty == 0 {
			book = book[1:]
		}
	}
	e.bids[sym] = book
	if rest := qty - filled; rest > 0 {
		e.restOrder(sym, "ask", e.mid[sym], rest, act.AgentID, at)
	}
	if filled == 0 {
		return ActionResult{Action: act, Accepted: true,
			Detail: fmt.Sprintf("rested %d %s as ask (no bids)", qty, sym), At: at}
	}
	return ActionResult{Action: act, Accepted: true,
		Detail: fmt.Sprintf("sold %d/%d %s", filled, qty, sym), At: at}
}

// limitBid places a resting limit buy, reserving cash up-front.
func (e *MarketEnvironment) limitBid(act Action, at time.Time) ActionResult {
	sym := strings.ToUpper(strings.TrimSpace(act.Target))
	if _, ok := e.mid[sym]; !ok {
		return ActionResult{Action: act, Accepted: false, Reason: "unknown_symbol", At: at}
	}
	qty := qtyOf(act)
	price, err := strconv.ParseFloat(act.Payload["price"], 64)
	if err != nil || price < 0.01 {
		return ActionResult{Action: act, Accepted: false, Reason: "bad_price", At: at}
	}
	cost := price * float64(qty)
	if e.cash[act.AgentID] < cost {
		return ActionResult{Action: act, Accepted: false, Reason: "insufficient_cash", At: at}
	}
	e.cash[act.AgentID] -= cost // reserved into the book
	e.restOrder(sym, "bid", price, qty, act.AgentID, at)
	return ActionResult{Action: act, Accepted: true,
		Detail: fmt.Sprintf("bid %d %s @ %.2f", qty, sym, price), At: at}
}

// restOrder appends an order to its side (price-time priority applied on sort).
func (e *MarketEnvironment) restOrder(sym, side string, price float64, qty int, agentID string, at time.Time) {
	e.seq++
	o := &marketOrder{
		id:      fmt.Sprintf("%s-%d", side, e.seq),
		agentID: agentID,
		symbol:  sym,
		side:    side,
		price:   price,
		qty:     qty,
		round:   e.round,
		seq:     e.seq,
	}
	if side == "bid" {
		e.bids[sym] = append(e.bids[sym], o)
		e.sortBook(e.bids[sym], true)
	} else {
		e.asks[sym] = append(e.asks[sym], o)
		e.sortBook(e.asks[sym], false)
	}
	_ = at // order timestamps live on the round (transcript carries ActionResult.At)
}

// sortBook orders a side: bids price-desc, asks price-asc; FIFO within price.
func (e *MarketEnvironment) sortBook(book []*marketOrder, bidSide bool) {
	sort.SliceStable(book, func(i, j int) bool {
		if book[i].price != book[j].price {
			if bidSide {
				return book[i].price > book[j].price
			}
			return book[i].price < book[j].price
		}
		return book[i].seq < book[j].seq
	})
}

// recordTrade appends a fill.
func (e *MarketEnvironment) recordTrade(sym, buyer, seller string, price float64, qty int) {
	e.trades = append(e.trades, marketTrade{Round: e.round, Symbol: sym, Buyer: buyer, Seller: seller, Price: price, Qty: qty})
}

// refreshMids recomputes the documented mid after a round's actions.
func (e *MarketEnvironment) refreshMids() {
	for _, sym := range e.symbols {
		var bb, ba float64
		if len(e.bids[sym]) > 0 {
			bb = e.bids[sym][0].price
		}
		if len(e.asks[sym]) > 0 {
			ba = e.asks[sym][0].price
		}
		switch {
		case bb > 0 && ba > 0:
			e.mid[sym] = (bb + ba) / 2
		case len(e.trades) > 0:
			e.mid[sym] = e.trades[len(e.trades)-1].Price
		}
	}
}

// snapshot builds the EnvState copy (book + ledger).
func (e *MarketEnvironment) snapshot() EnvState {
	syms := map[string]any{}
	for _, sym := range e.symbols {
		var bb, ba float64
		if len(e.bids[sym]) > 0 {
			bb = e.bids[sym][0].price
		}
		if len(e.asks[sym]) > 0 {
			ba = e.asks[sym][0].price
		}
		syms[sym] = map[string]float64{"mid": e.mid[sym], "best_bid": bb, "best_ask": ba}
	}
	recent := make([]marketTrade, 0, maxRecentTrades)
	lo := len(e.trades) - maxRecentTrades
	if lo < 0 {
		lo = 0
	}
	recent = append(recent, e.trades[lo:]...)

	agentIDs := make([]string, 0, len(e.cfg.Agents))
	cash := map[string]float64{}
	holdings := map[string]map[string]int{}
	for _, a := range e.cfg.Agents {
		id := a.CoreIdentity.ID
		agentIDs = append(agentIDs, id)
		cash[id] = e.cash[id]
		h := map[string]int{}
		for sym, q := range e.holdings[id] {
			if q != 0 {
				h[sym] = q
			}
		}
		holdings[id] = h
	}
	sort.Strings(agentIDs)
	return EnvState{
		Kind:     KindMarket,
		Round:    e.round,
		AgentIDs: agentIDs,
		Facts: map[string]any{
			"symbols":       syms,
			"resting_bids":  e.countOrders(e.bids),
			"resting_asks":  e.countOrders(e.asks),
			"trade_count":   len(e.trades),
			"recent_trades": recent,
			"cash":          cash,
			"holdings":      holdings,
			"peers":         agentIDs, // genericAgent contract (registry.go)
		},
	}
}

// countOrders totals resting quantity-of-orders on a side.
func (e *MarketEnvironment) countOrders(book map[string][]*marketOrder) int {
	n := 0
	for _, side := range book {
		n += len(side)
	}
	return n
}

// GetState implements SimulationEnvironment.
func (e *MarketEnvironment) GetState(context.Context) (EnvState, error) { return e.snapshot(), nil }

// Close implements SimulationEnvironment (in-memory: nothing to release).
func (e *MarketEnvironment) Close() error { return nil }

// MarketAgent renders a SimAgentPersona into the market world's policy.
type MarketAgent struct{ persona SimAgentPersona }

// Persona implements SimAgent.
func (a *MarketAgent) Persona() SimAgentPersona { return a.persona }

// Decide implements SimAgent: risk_appetite-derived weights (documented on
// the file header), symbol = uniform draw, qty 1..3, bid price = mid
// discounted 0.95–0.99 by a deterministic draw.
func (a *MarketAgent) Decide(_ context.Context, view EnvState, rnd *rand.Rand) Action {
	beh := behaviorFor(a.persona, KindMarket)
	r := trait(a.persona, "risk_appetite")
	verb := pickWeighted(beh, map[string]float64{
		"buy":  0.15 + 0.60*r,
		"sell": 0.15 + 0.40*(1-r),
		"bid":  0.25,
	}, rnd)
	if verb == "" {
		return Action{Kind: "idle"}
	}
	syms := symbolNames(view.Facts["symbols"])
	if len(syms) == 0 {
		return Action{Kind: "idle"}
	}
	sym := syms[rnd.Intn(len(syms))]
	qty := 1 + rnd.Intn(3)
	act := Action{Kind: verb, Target: sym, Payload: map[string]string{"qty": strconv.Itoa(qty)}}
	if verb == "bid" {
		if m, ok := symbolMid(view.Facts["symbols"], sym); ok {
			act.Payload["price"] = fmt.Sprintf("%.2f", m*(0.95+0.04*rnd.Float64()))
		}
	}
	return act
}

// symbolNames extracts the sorted symbol list from the snapshot facts
// (facts["symbols"] is map[string]any of map[string]float64 — the JSON-
// friendly outer type).
func symbolNames(v any) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// symbolMid reads one symbol's mid from the snapshot facts.
func symbolMid(v any, sym string) (float64, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	s, ok := m[sym].(map[string]float64)
	if !ok {
		return 0, false
	}
	return s["mid"], true
}
