package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/trades"
	"github.com/francocaccia/quantengine/internal/universe"
)

type orderRec struct {
	symbol string
	side   broker.Side
	qty    int
}

type fakeOrders struct{ placed []orderRec }

func (f *fakeOrders) PlaceBracketOrder(symbol string, qty int, side broker.Side, _, _ float64) error {
	f.placed = append(f.placed, orderRec{symbol, side, qty})
	return nil
}
func (f *fakeOrders) PlaceMarketOrder(symbol string, qty int, side broker.Side) error {
	f.placed = append(f.placed, orderRec{symbol, side, qty})
	return nil
}
func (f *fakeOrders) CancelOpenOrders(string) (int, error) { return 0, nil }

func (f *fakeOrders) symbols(side broker.Side) []string {
	var out []string
	for _, o := range f.placed {
		if o.side == side {
			out = append(out, o.symbol)
		}
	}
	return out
}

func (f *fakeOrders) qty(symbol string) int {
	for _, o := range f.placed {
		if o.symbol == symbol {
			return o.qty
		}
	}
	return 0
}

type fakeAcct struct{ a portfolio.Account }

func (f fakeAcct) GetAccount() (*portfolio.Account, error) { a := f.a; return &a, nil }

var (
	// Thursday 2026-10-15, 11:00 ET.
	testNow  = time.Date(2026, 10, 15, 11, 0, 0, 0, time.FixedZone("ET", -4*3600))
	testDays = []string{"2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-12", "2026-10-13", "2026-10-14", "2026-10-15"}
)

const headline = "ACME wins $2B Pentagon contract for drone engines"

func baseConfig() Config {
	return Config{MaxPositionPct: 10, MaxOpenPositions: 8, MaxRiskPct: 1, MinBuyConviction: 8,
		MinHoldDays: 3, ReentryCooldownDays: 3, MaxTradesPerDay: 2, MaxTradesPerWeek: 5}
}

func newTestScheduler(cfg Config) (*Scheduler, *fakeOrders) {
	o := &fakeOrders{}
	return &Scheduler{
		cfg: cfg, sectors: map[string]string{}, gate: newEvalGate(0), orders: o,
		acct:          fakeAcct{portfolio.Account{Equity: 100_000, Cash: 100_000, BuyingPower: 100_000}},
		lastDecisions: map[string]gemini.LastDecision{},
	}, o
}

func newCycle(fills ...trades.Fill) *cycleState {
	cs := &cycleState{
		positions: map[string]*portfolio.Position{}, sectorValue: map[string]float64{}, warnedNoSec: map[string]bool{},
		now: testNow, history: trades.NewSnapshot(fills, testDays, testNow),
	}
	cs.tradesDay, cs.tradesWeek = cs.history.DecisionCounts()
	return cs
}

func fill(sym, side, typ, at string, qty float64) trades.Fill {
	tm, err := time.ParseInLocation("2006-01-02 15:04", at, testNow.Location())
	if err != nil {
		panic(err)
	}
	return trades.Fill{Symbol: sym, Side: side, Type: typ, At: tm, Qty: qty}
}

// ind is a 100-dollar stock with a 2.5 ATR: stop 95, take-profit 110.
func ind() *indicators.Result { return &indicators.Result{Price: 100, ATR: 2.5} }

func buy(sym string, conviction int, catalyst string) pendingDecision {
	return pendingDecision{symbol: sym, ind: ind(), headlines: []string{headline, "Fed holds rates steady"},
		decision: &gemini.Decision{Action: "BUY", Conviction: conviction, Catalyst: catalyst, Thesis: "t"}}
}

func sell(cs *cycleState, sym string) pendingDecision {
	pos := &portfolio.Position{Symbol: sym, Qty: 25, AvgPrice: 90, MarketVal: 2500}
	cs.positions[sym] = pos
	return pendingDecision{symbol: sym, ind: ind(), position: pos, decision: &gemini.Decision{Action: "SELL", Conviction: 6, Thesis: "broken"}}
}

func TestCatalystMatches(t *testing.T) {
	hl := []string{headline, "Fed holds rates steady"}
	tests := []struct {
		name, catalyst string
		want           bool
	}{
		{"exact", headline, true},
		{"case-insensitive", strings.ToUpper(headline), true},
		{"partial quote", "wins $2B Pentagon contract", true},
		{"quoted by the model", `"` + headline + `"`, true},
		{"with source prefix", "[Benzinga] " + headline, true},
		{"not in the news", "ACME announces buyback", false},
		{"none", "none", false},
		{"None with spaces", "  None ", false},
		{"empty", "", false},
		{"fragment too short to mean anything", "the", false},
		{"invented headline", "NVDA beats earnings estimates", false},
	}
	for _, tc := range tests {
		if got := catalystMatches(tc.catalyst, hl); got != tc.want {
			t.Errorf("%s: catalystMatches(%q) = %v, want %v", tc.name, tc.catalyst, got, tc.want)
		}
	}
	if catalystMatches(headline, nil) {
		t.Error("no headlines shown means nothing can match")
	}
}

func TestBuyNeedsConvictionAndVerifiedCatalyst(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	cs.pending = []pendingDecision{
		buy("LOWC", 7, headline),                    // conviction below 8
		buy("NOCAT", 9, "none"),                     // no catalyst
		buy("FAKE", 9, "NVDA beats earnings again"), // not a shown headline
		buy("OKAY", 8, headline),
	}
	s.executeDecisions(cs)
	if got := o.symbols(broker.Buy); len(got) != 1 || got[0] != "OKAY" {
		t.Fatalf("only OKAY may be bought, got %v", got)
	}
	if o.qty("OKAY") != 50 {
		t.Errorf("conviction 8 sizes at 5%% of 100k at $100 = 50 shares, got %d", o.qty("OKAY"))
	}
	if cs.blocked != 3 {
		t.Errorf("blocked = %d, want 3", cs.blocked)
	}
	if cs.tradesDay != 1 || cs.tradesWeek != 1 {
		t.Errorf("trade counters = %d/%d, want 1/1", cs.tradesDay, cs.tradesWeek)
	}
}

func TestSizingLadderShiftedUp(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxTradesPerDay, cfg.MaxTradesPerWeek = 10, 10
	s, o := newTestScheduler(cfg)
	cs := newCycle()
	cs.pending = []pendingDecision{buy("C8", 8, headline), buy("C9", 9, headline), buy("C10", 10, headline)}
	s.executeDecisions(cs)
	for sym, want := range map[string]int{"C8": 50, "C9": 70, "C10": 100} {
		if got := o.qty(sym); got != want {
			t.Errorf("%s qty = %d, want %d", sym, got, want)
		}
	}
}

func TestMinHoldBlocksYoungSells(t *testing.T) {
	young := fill("YNG", "buy", "market", "2026-10-14 10:30", 25) // 1 trading day old
	edge := fill("EDG", "buy", "market", "2026-10-12 10:30", 25)  // 3 trading days old
	old := fill("OLD", "buy", "market", "2026-10-06 10:30", 25)   // well past
	tests := []struct {
		name     string
		symbol   string
		fills    []trades.Fill
		breaker  bool
		wantSold bool
	}{
		{"1 day old is held", "YNG", []trades.Fill{young}, false, false},
		{"exactly the minimum may be sold", "EDG", []trades.Fill{edge}, false, true},
		{"old position may be sold", "OLD", []trades.Fill{old}, false, true},
		{"legacy position: unknown age, no restriction", "LEG", nil, false, true},
		{"circuit breaker overrides the holding period", "YNG", []trades.Fill{young}, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, o := newTestScheduler(baseConfig())
			cs := newCycle(tc.fills...)
			cs.breaker = tc.breaker
			cs.pending = []pendingDecision{sell(cs, tc.symbol)}
			s.executeDecisions(cs)
			if sold := len(o.symbols(broker.Sell)) == 1; sold != tc.wantSold {
				t.Errorf("sold = %v, want %v (orders %+v)", sold, tc.wantSold, o.placed)
			}
		})
	}
}

func TestReentryCooldown(t *testing.T) {
	tests := []struct {
		name    string
		fills   []trades.Fill
		wantBuy bool
	}{
		{"sold yesterday", []trades.Fill{fill("AAA", "sell", "market", "2026-10-14 10:30", 10)}, false},
		{"sold 2 trading days ago", []trades.Fill{fill("AAA", "sell", "market", "2026-10-13 10:30", 10)}, false},
		{"stopped out yesterday counts too", []trades.Fill{fill("AAA", "sell", "stop", "2026-10-14 10:30", 10)}, false},
		{"sold 3 trading days ago", []trades.Fill{fill("AAA", "sell", "market", "2026-10-12 10:30", 10)}, true},
		{"another symbol sold yesterday", []trades.Fill{fill("ZZZ", "sell", "market", "2026-10-14 10:30", 10)}, true},
		{"never sold", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, o := newTestScheduler(baseConfig())
			cs := newCycle(tc.fills...)
			cs.pending = []pendingDecision{buy("AAA", 9, headline)}
			s.executeDecisions(cs)
			if bought := len(o.symbols(broker.Buy)) == 1; bought != tc.wantBuy {
				t.Errorf("bought = %v, want %v", bought, tc.wantBuy)
			}
		})
	}
}

// 3 BUY decisions plus 1 risk-reducing SELL with a daily cap of 2: the SELL and
// the highest-conviction BUY run.
func TestTradeCapPriorityOrdering(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxTradesPerDay = 2
	s, o := newTestScheduler(cfg)
	cs := newCycle(fill("OLD", "buy", "market", "2026-10-06 10:30", 25))
	cs.pending = []pendingDecision{
		buy("LOW", 8, headline),
		buy("TOP", 10, headline),
		buy("MID", 9, headline),
		sell(cs, "OLD"),
	}
	s.executeDecisions(cs)

	if got := o.symbols(broker.Sell); len(got) != 1 || got[0] != "OLD" {
		t.Errorf("the SELL must run, got %v", got)
	}
	if got := o.symbols(broker.Buy); len(got) != 1 || got[0] != "TOP" {
		t.Errorf("only the highest-conviction BUY may run, got %v", got)
	}
	if o.placed[0].side != broker.Sell {
		t.Errorf("risk-reducing SELLs go first, order was %+v", o.placed)
	}
	if cs.tradesDay != 2 || cs.blocked != 2 {
		t.Errorf("tradesDay=%d blocked=%d, want 2 and 2", cs.tradesDay, cs.blocked)
	}
}

func TestTradeCapCountsHistoryAndWeek(t *testing.T) {
	// One bot decision already filled today; stop/limit legs do not count.
	s, o := newTestScheduler(baseConfig())
	cs := newCycle(
		fill("DONE", "buy", "market", "2026-10-15 10:05", 5),
		fill("DONE", "sell", "stop", "2026-10-15 10:30", 5),
	)
	cs.pending = []pendingDecision{buy("A", 10, headline), buy("B", 9, headline)}
	s.executeDecisions(cs)
	if got := o.symbols(broker.Buy); len(got) != 1 || got[0] != "A" {
		t.Errorf("daily cap 2 with 1 already filled leaves room for one: %v", got)
	}

	// Weekly cap: 5 decisions over the rolling window block everything.
	s, o = newTestScheduler(baseConfig())
	cs = newCycle(
		fill("W1", "buy", "market", "2026-10-09 10:05", 1),
		fill("W2", "buy", "market", "2026-10-12 10:05", 1),
		fill("W3", "buy", "market", "2026-10-13 10:05", 1),
		fill("W4", "buy", "market", "2026-10-14 10:05", 1),
		fill("W5", "sell", "market", "2026-10-14 11:05", 1),
	)
	cs.pending = []pendingDecision{buy("A", 10, headline)}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.tradesWeek != 5 {
		t.Errorf("weekly cap 5/5 must block: orders %+v, week %d", o.placed, cs.tradesWeek)
	}
	// An order from outside the 5-day window no longer counts.
	cs = newCycle(fill("W0", "buy", "market", "2026-10-08 10:05", 1))
	if cs.tradesWeek != 0 {
		t.Errorf("fill 6 trading days back must not count, got %d", cs.tradesWeek)
	}
}

func TestBreakerExitIsNotCappedOrCounted(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxTradesPerDay = 1
	s, o := newTestScheduler(cfg)
	cs := newCycle(fill("X", "buy", "market", "2026-10-15 10:05", 1)) // day cap already reached
	cs.breaker = true
	cs.pending = []pendingDecision{sell(cs, "OLD")}
	s.executeDecisions(cs)
	if len(o.symbols(broker.Sell)) != 1 || cs.tradesDay != 1 {
		t.Errorf("breaker exit must run uncounted: orders %+v, tradesDay %d", o.placed, cs.tradesDay)
	}
}

func TestMissingHistoryBlocksBuysButNotSells(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	cs.history = nil
	cs.pending = []pendingDecision{buy("A", 10, headline), sell(cs, "OLD")}
	s.executeDecisions(cs)
	if len(o.symbols(broker.Buy)) != 0 || len(o.symbols(broker.Sell)) != 1 {
		t.Errorf("want SELL only, got %+v", o.placed)
	}
}

func TestHoldAndBuyBlockAreNotExecuted(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	cs.buyBlock = "daily loss breaker"
	cs.pending = []pendingDecision{
		buy("A", 10, headline),
		{symbol: "H", ind: ind(), decision: &gemini.Decision{Action: "HOLD", Conviction: 5}},
	}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.counts.hold != 1 || cs.counts.buy != 1 {
		t.Errorf("orders %+v counts %+v", o.placed, cs.counts)
	}
}

func TestMaxRiskCapsQuantityAtExecution(t *testing.T) {
	cases := []struct {
		name        string
		price, atr  float64
		equity      float64
		maxRisk     float64
		conviction  int
		wantQty     int
		wantNoOrder bool
	}{
		// CIFR 2026-10-13: price 11.13, ATR 1.285 -> stop 8.56 (23% away).
		{"CIFR-like wide stop", 11.13, 1.285, 102_000, 1.0, 9, 396, false},
		{"tight stop unchanged", 100, 1.0, 100_000, 1.0, 9, 70, false},
		{"under one share is skipped", 11.13, 1.285, 102_000, 0.001, 9, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.MaxRiskPct = tc.maxRisk
			s, o := newTestScheduler(cfg)
			s.acct = fakeAcct{portfolio.Account{Equity: tc.equity, Cash: tc.equity, BuyingPower: tc.equity}}
			cs := newCycle()
			p := buy("CIFR", tc.conviction, headline)
			p.ind = &indicators.Result{Price: tc.price, ATR: tc.atr}
			cs.pending = []pendingDecision{p}
			s.executeDecisions(cs)
			if tc.wantNoOrder {
				if len(o.placed) != 0 || cs.tradesDay != 0 {
					t.Errorf("want no order and no turnover used, got %+v (day %d)", o.placed, cs.tradesDay)
				}
				return
			}
			if got := o.qty("CIFR"); got != tc.wantQty {
				t.Errorf("qty = %d, want %d", got, tc.wantQty)
			}
		})
	}
}

func TestCandidateWithoutHeadlineIsSkipped(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	s.newsCache = []news.Article{{Headline: "ACME wins contract", Symbols: []string{"ACME"}}}
	if s.noHeadlineCandidate(universe.Item{Symbol: "ACME"}) {
		t.Error("a candidate with a headline naming it is evaluated")
	}
	if !s.noHeadlineCandidate(universe.Item{Symbol: "ZZZZ"}) {
		t.Error("a candidate no headline mentions must be skipped")
	}
	if s.noHeadlineCandidate(universe.Item{Symbol: "AAPL", Core: true}) {
		t.Error("core symbols are not subject to the candidate news requirement")
	}
}
