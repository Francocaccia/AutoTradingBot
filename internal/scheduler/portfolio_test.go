package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/francocaccia/quantengine/internal/analysis"
	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/market"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/regime"
)

const block = "## PORTFOLIO\nMacro regime: risk-off, inverted curve\nVIX: 27.3\nSleeves vs IPS bands (% of equity): cash 41.0% (band 5-30%, target 10%)\n"

// withAccount sets the same account on the cycle and the scheduler's fresh-account source.
func withAccount(s *Scheduler, cs *cycleState, equity, cash float64) {
	acct := portfolio.Account{Equity: equity, Cash: cash, BuyingPower: cash}
	s.acct = fakeAcct{acct}
	a := acct
	cs.account = &a
}

func withRegime(cs *cycleState, available bool) {
	r := regime.Unavailable()
	if available {
		r = regime.Regime{Available: true, Label: "risk-off, inverted curve"}
	}
	cs.pf = &pfContext{regime: r}
}

// hold adds an existing position worth value dollars.
func hold(cs *cycleState, sym string, value float64) {
	cs.positions[sym] = &portfolio.Position{Symbol: sym, Qty: int(value / 100), AvgPrice: 100, MarketVal: value}
}

func bondInd() *indicators.Result { return &indicators.Result{Price: 100, ATR: 0.5} }

func bondBuy(sym, catalyst string) pendingDecision {
	return pendingDecision{symbol: sym, ind: bondInd(), portfolioText: block, fromReview: true,
		decision: &gemini.Decision{Action: "BUY", Conviction: 8, Role: "income", Catalyst: catalyst,
			Rationale: "risk-off regime with an inverted curve favours short duration"}}
}

func TestBuyNeedsRoleAndRationale(t *testing.T) {
	for name, mod := range map[string]func(d *gemini.Decision){
		"missing role":      func(d *gemini.Decision) { d.Role = "" },
		"unknown role":      func(d *gemini.Decision) { d.Role = "moonshot" },
		"missing rationale": func(d *gemini.Decision) { d.Rationale = "  " },
	} {
		s, o := newTestScheduler(baseConfig())
		cs := newCycle()
		p := buy("AAA", 9, headline)
		mod(p.decision)
		cs.pending = []pendingDecision{p}
		s.executeDecisions(cs)
		if len(o.placed) != 0 || cs.dropReasons["role/rationale"] != 1 {
			t.Errorf("%s: orders %+v drops %v", name, o.placed, cs.dropReasons)
		}
	}
	// SELLs need them too, except as a circuit-breaker exit.
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	p := sell(cs, "OLD")
	p.decision.Role = ""
	cs.pending = []pendingDecision{p}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["role/rationale"] != 1 {
		t.Errorf("SELL without role: %+v %v", o.placed, cs.dropReasons)
	}
	s, o = newTestScheduler(baseConfig())
	cs = newCycle()
	cs.breaker = true
	p = sell(cs, "OLD")
	p.decision.Role, p.decision.Rationale = "", ""
	cs.pending = []pendingDecision{p}
	s.executeDecisions(cs)
	if len(o.symbols(broker.Sell)) != 1 {
		t.Errorf("a breaker exit must not wait for metadata: %+v", o.placed)
	}
}

func TestIPSRejectsEquityBuyOverTheBand(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	// 75% of a 100k account is already in equities.
	for _, h := range []struct {
		sym string
		val float64
	}{{"A", 14_000}, {"B", 14_000}, {"C", 14_000}, {"D", 14_000}, {"E", 14_000}, {"SPY", 5_000}} {
		hold(cs, h.sym, h.val)
		s.sectors[h.sym] = "Sector-" + h.sym
	}
	withAccount(s, cs, 100_000, 25_000)
	cs.pending = []pendingDecision{buy("NEWCO", 10, headline)}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["ips"] != 1 {
		t.Errorf("equity band full: orders %+v drops %v", o.placed, cs.dropReasons)
	}
}

func TestIPSTrimsToSingleNameLimit(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxPositionPct = 20 // looser than the IPS single-position limit
	s, o := newTestScheduler(cfg)
	s.policy.MaxStockPct = 6 // 6% => 60 shares at $100, below the 10% conviction target
	cs := newCycle()
	cs.pending = []pendingDecision{buy("AAA", 10, headline)}
	s.executeDecisions(cs)
	if got := o.qty("AAA"); got != 60 {
		t.Errorf("qty = %d, want 60 (trimmed by the IPS single-position limit)", got)
	}
}

func TestBondBuyMacroCatalystAndRegime(t *testing.T) {
	tests := []struct {
		name      string
		sym       string
		catalyst  string
		available bool
		wantOrder bool
		wantKey   string
	}{
		{"quoted label", "SHY", "risk-off, inverted curve", true, true, ""},
		{"quoted datapoint, case-insensitive", "SHY", "vix: 27.3", true, true, ""},
		{"headline-style invented catalyst", "SHY", "Fed signals cuts next month", true, false, "catalyst"},
		{"none", "SHY", "none", true, false, "catalyst"},
		{"too short", "SHY", "vix", true, false, "catalyst"},
		{"long duration allowed with data", "IEF", "VIX: 27.3", true, true, ""},
		{"long duration blocked without macro data", "TLT", "VIX: 27.3", false, false, "macro-unavailable"},
		{"IEF blocked without macro data", "IEF", "VIX: 27.3", false, false, "macro-unavailable"},
		{"SHY still allowed without macro data", "SHY", "cash 41.0% (band 5-30%, target 10%)", false, true, ""},
		{"placeholder text is never a catalyst", "SHY", "Macro regime: unavailable", false, false, "catalyst"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, o := newTestScheduler(baseConfig())
			cs := newCycle()
			withRegime(cs, tc.available)
			withAccount(s, cs, 100_000, 100_000)
			hold(cs, "SPY", 5_000) // keeps cash well above the band, so parking rules are not the issue
			cs.account.Cash = 95_000
			cs.pending = []pendingDecision{bondBuy(tc.sym, tc.catalyst)}
			s.executeDecisions(cs)
			_, ordered := o.order(tc.sym)
			if ordered != tc.wantOrder {
				t.Fatalf("ordered=%v, want %v (drops %v)", ordered, tc.wantOrder, cs.dropReasons)
			}
			if tc.wantKey != "" && cs.dropReasons[tc.wantKey] != 1 {
				t.Errorf("drops = %v, want %q", cs.dropReasons, tc.wantKey)
			}
		})
	}
}

func TestBondBuyIsSizedTowardTheSleeveTargetWithAWiderStop(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{bondBuy("SHY", "VIX: 27.3")}
	s.executeDecisions(cs)
	// 0% in bonds vs a 30% target: the step is capped at 5% of equity = 50 shares at $100.
	ord, ok := o.order("SHY")
	if !ok || ord.qty != 50 || ord.kind != "oto" {
		t.Fatalf("order = %+v ok=%v, want 50 shares as a stop-only (oto) order", ord, ok)
	}
	if ord.stop != 98.5 { // 100 - 3 x 0.5 ATR
		t.Errorf("stop = %v, want 98.5 (3x ATR)", ord.stop)
	}
	if cs.positions["SHY"] == nil {
		t.Error("the cycle's positions must reflect the purchase")
	}

	// Sleeve closer to target: the purchase is only the gap.
	s, o = newTestScheduler(baseConfig())
	cs = newCycle()
	withRegime(cs, true)
	hold(cs, "IEF", 14_000)
	hold(cs, "AGG", 14_000)
	withAccount(s, cs, 100_000, 72_000)
	cs.pending = []pendingDecision{bondBuy("SHY", "VIX: 27.3")}
	s.executeDecisions(cs)
	if got := o.qty("SHY"); got != 20 { // 30% target - 28% = 2% = $2,000
		t.Errorf("qty = %d, want 20 (the 2%% gap to target)", got)
	}

	// At or above target: nothing to do.
	s, o = newTestScheduler(baseConfig())
	cs = newCycle()
	withRegime(cs, true)
	hold(cs, "IEF", 15_000)
	hold(cs, "AGG", 15_000)
	withAccount(s, cs, 100_000, 70_000)
	cs.pending = []pendingDecision{bondBuy("SHY", "VIX: 27.3")}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["sleeve-target"] != 1 {
		t.Errorf("sleeve at target: %+v %v", o.placed, cs.dropReasons)
	}
}

func TestCashLikeETFIsAPlainMarketOrderAndOnlyWhenCashIsAboveItsBand(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 100_000) // 100% cash: far above the 30% max
	cs.pending = []pendingDecision{bondBuy("SGOV", "cash 41.0% (band 5-30%, target 10%)")}
	cs.pending[0].decision.Role = "cash_parking"
	s.executeDecisions(cs)
	ord, ok := o.order("SGOV")
	if !ok || ord.kind != "market" || ord.qty != 50 {
		t.Errorf("order = %+v ok=%v, want a plain 50-share market order", ord, ok)
	}

	// Cash inside its band is not a problem and must not trigger buying.
	s, o = newTestScheduler(baseConfig())
	cs = newCycle()
	withRegime(cs, true)
	hold(cs, "SPY", 5_000)
	hold(cs, "IEF", 15_000)
	hold(cs, "A", 10_000)
	hold(cs, "B", 10_000)
	hold(cs, "C", 10_000)
	withAccount(s, cs, 100_000, 25_000) // 25% cash < 30% max
	cs.pending = []pendingDecision{bondBuy("SGOV", "cash 41.0% (band 5-30%, target 10%)")}
	cs.pending[0].decision.Role = "cash_parking"
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["cash-in-band"] != 1 {
		t.Errorf("cash in band: %+v %v", o.placed, cs.dropReasons)
	}
}

func TestBondETFLimitsCreditAndSingleName(t *testing.T) {
	// Combined credit-risk bonds already at 10%: HYG and EMB are rejected.
	for _, sym := range []string{"HYG", "EMB"} {
		s, o := newTestScheduler(baseConfig())
		cs := newCycle()
		withRegime(cs, true)
		hold(cs, "HYG", 5_000)
		hold(cs, "EMB", 5_000)
		withAccount(s, cs, 100_000, 90_000)
		cs.pending = []pendingDecision{bondBuy(sym, "VIX: 27.3")}
		s.executeDecisions(cs)
		if len(o.placed) != 0 || cs.dropReasons["ips"] != 1 {
			t.Errorf("%s over the credit cap: %+v %v", sym, o.placed, cs.dropReasons)
		}
	}
	// A bond ETF already at 15% cannot grow.
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	withRegime(cs, true)
	hold(cs, "AGG", 15_000)
	withAccount(s, cs, 100_000, 85_000)
	cs.pending = []pendingDecision{bondBuy("AGG", "VIX: 27.3")}
	s.executeDecisions(cs)
	if len(o.placed) != 0 {
		t.Errorf("single bond ETF limit: %+v", o.placed)
	}
}

func TestBondUniverseDisabled(t *testing.T) {
	cfg := baseConfig()
	cfg.Bonds = portfolio.DisabledBondUniverse()
	s, o := newTestScheduler(cfg)
	cs := newCycle()
	withRegime(cs, true)
	cs.pending = []pendingDecision{bondBuy("SHY", "VIX: 27.3")}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["bond-universe"] != 1 {
		t.Errorf("BOND_MODE=off must refuse bond buys: %+v %v", o.placed, cs.dropReasons)
	}
}

func corr(avg float64) *analysis.Correlation {
	return &analysis.Correlation{Avg: avg, Max: avg, MaxSym: "AAPL", N: 3}
}

func TestCorrelationRuleAndReplaces(t *testing.T) {
	tests := []struct {
		name      string
		avg       float64
		replaces  string
		held      []string
		wantBuy   bool
		wantSell  string
		wantDrops string
	}{
		{"low correlation passes", 0.5, "", []string{"AAPL"}, true, "", ""},
		{"exactly 0.8 passes", 0.8, "", []string{"AAPL"}, true, "", ""},
		{"high correlation rejected", 0.9, "", []string{"AAPL"}, false, "", "correlation"},
		{"replaces a symbol that is not held", 0.9, "ZZZ", []string{"AAPL"}, false, "", "correlation"},
		{"replaces itself", 0.9, "MSFT", []string{"AAPL"}, false, "", "correlation"},
		{"valid replaces rotates out the held symbol", 0.9, "AAPL", []string{"AAPL"}, true, "AAPL", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, o := newTestScheduler(baseConfig())
			cs := newCycle(fill("AAPL", "buy", "market", "2026-10-06 10:30", 25)) // old enough to sell
			for _, h := range tc.held {
				hold(cs, h, 5_000)
				s.sectors[h] = "Technology"
			}
			withAccount(s, cs, 100_000, 95_000)
			p := buy("MSFT", 9, headline)
			p.corr = corr(tc.avg)
			p.decision.Replaces = tc.replaces
			cs.pending = []pendingDecision{p}
			s.executeDecisions(cs)
			if bought := len(o.symbols(broker.Buy)) == 1; bought != tc.wantBuy {
				t.Fatalf("bought=%v want %v (orders %+v, drops %v)", bought, tc.wantBuy, o.placed, cs.dropReasons)
			}
			if tc.wantSell != "" {
				if got := o.symbols(broker.Sell); len(got) != 1 || got[0] != tc.wantSell {
					t.Errorf("sells = %v, want [%s]", got, tc.wantSell)
				}
				if o.placed[0].side != broker.Sell {
					t.Errorf("the rotated-out position must be sold first: %+v", o.placed)
				}
				if cs.tradesDay != 2 {
					t.Errorf("a rotation uses two turnover slots, got %d", cs.tradesDay)
				}
			}
			if tc.wantDrops != "" && cs.dropReasons[tc.wantDrops] != 1 {
				t.Errorf("drops = %v, want %q", cs.dropReasons, tc.wantDrops)
			}
		})
	}
}

func TestRotationIsAllOrNothing(t *testing.T) {
	// Daily cap of 1: the pair needs two slots, so neither order is placed.
	cfg := baseConfig()
	cfg.MaxTradesPerDay = 1
	s, o := newTestScheduler(cfg)
	cs := newCycle(fill("AAPL", "buy", "market", "2026-10-06 10:30", 25))
	hold(cs, "AAPL", 5_000)
	withAccount(s, cs, 100_000, 95_000)
	p := buy("MSFT", 9, headline)
	p.corr, p.decision.Replaces = corr(0.9), "AAPL"
	cs.pending = []pendingDecision{p}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["cap"] != 1 {
		t.Errorf("half-rotation: %+v %v", o.placed, cs.dropReasons)
	}

	// The replaced position is too young to sell: the BUY is dropped too.
	s, o = newTestScheduler(baseConfig())
	cs = newCycle(fill("AAPL", "buy", "market", "2026-10-14 10:30", 25))
	hold(cs, "AAPL", 5_000)
	withAccount(s, cs, 100_000, 95_000)
	p = buy("MSFT", 9, headline)
	p.corr, p.decision.Replaces = corr(0.9), "AAPL"
	cs.pending = []pendingDecision{p}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["min-hold"] != 1 {
		t.Errorf("young replaced position: %+v %v", o.placed, cs.dropReasons)
	}
}

func TestDuplicateDecisionsForOneSymbolRunOnce(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	cs.pending = []pendingDecision{buy("AAA", 9, headline), buy("AAA", 10, headline)}
	s.executeDecisions(cs)
	if got := o.symbols(broker.Buy); len(got) != 1 {
		t.Errorf("duplicate symbol bought %d times", len(got))
	}
}

// --- weekly review ---

func session(day, hhmm string) *market.Session {
	tm, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, testNow.Location())
	if err != nil {
		panic(err)
	}
	return &market.Session{Now: tm, Date: day, IsOpen: true, TradingDay: true}
}

func TestReviewRunsMondayAfter1000OrOnBandBreach(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	cs := newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 100_000) // 100% cash: 70 points over its band max
	hold(cs, "X", 0)
	delete(cs.positions, "X")

	// 2026-10-12 is a Monday.
	if r := s.reviewReason(cs, session("2026-10-12", "09:45")); r != "" && !strings.HasPrefix(r, "band breach") {
		t.Errorf("Monday before 10:00: %q", r)
	}
	if r := s.reviewReason(cs, session("2026-10-12", "09:45")); r != "" {
		t.Errorf("nothing runs before 10:00, got %q", r)
	}
	if r := s.reviewReason(cs, session("2026-10-12", "10:00")); r != "weekly review" {
		t.Errorf("Monday 10:00: %q", r)
	}
	s.lastReviewDay = "2026-10-12"
	if r := s.reviewReason(cs, session("2026-10-12", "11:00")); r != "" {
		t.Errorf("already reviewed today: %q", r)
	}
	// Tuesday: only a band breach triggers, and only once that day.
	if r := s.reviewReason(cs, session("2026-10-13", "10:30")); !strings.HasPrefix(r, "band breach") {
		t.Errorf("Tuesday with cash far over its band: %q", r)
	}
	s.lastBandReviewDay = "2026-10-13"
	if r := s.reviewReason(cs, session("2026-10-13", "11:30")); r != "" {
		t.Errorf("one band review per day: %q", r)
	}

	// Cash inside its band is not a reason, and neither is a breach under 5 points.
	s2, _ := newTestScheduler(baseConfig())
	cs2 := newCycle()
	withRegime(cs2, true)
	hold(cs2, "SPY", 55_000)
	hold(cs2, "IEF", 15_000)
	withAccount(s2, cs2, 100_000, 30_000)
	if r := s2.reviewReason(cs2, session("2026-10-13", "10:30")); r != "" {
		t.Errorf("balanced book on a Tuesday: %q", r)
	}
	hold(cs2, "SPY", 55_000)
	withAccount(s2, cs2, 100_000, 28_000) // cash 28%, equities 55%, bonds 15%: all in band
	if r := s2.reviewReason(cs2, session("2026-10-13", "10:30")); r != "" {
		t.Errorf("in-band book: %q", r)
	}
}

func f64(v float64) *float64 { return &v }

func TestReviewTargetsAreClippedOrRejected(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	cs := newCycle()
	r := &gemini.Review{}

	// In-band targets are adopted as given.
	r.Targets.Equities, r.Targets.Bonds, r.Targets.Cash = f64(50), f64(35), f64(15)
	s.applyReview("weekly review", r, cs, block)
	if s.policy.TargetEquity != 50 || s.policy.TargetBond != 35 || s.policy.TargetCash != 15 {
		t.Errorf("targets = %v/%v/%v", s.policy.TargetEquity, s.policy.TargetBond, s.policy.TargetCash)
	}

	// 80/10/10 clips to 75/15/10 and still sums to 100: adopted clipped.
	r.Targets.Equities, r.Targets.Bonds, r.Targets.Cash = f64(80), f64(10), f64(10)
	s.applyReview("weekly review", r, cs, block)
	if s.policy.TargetEquity != 75 || s.policy.TargetBond != 15 || s.policy.TargetCash != 10 {
		t.Errorf("clipped targets = %v/%v/%v", s.policy.TargetEquity, s.policy.TargetBond, s.policy.TargetCash)
	}

	// 90/40/30 clips to 75/40/30 = 145: rejected, previous targets kept.
	r.Targets.Equities, r.Targets.Bonds, r.Targets.Cash = f64(90), f64(40), f64(30)
	s.applyReview("weekly review", r, cs, block)
	if s.policy.TargetEquity != 75 || s.policy.TargetBond != 15 || s.policy.TargetCash != 10 {
		t.Errorf("rejected targets changed the policy: %v/%v/%v", s.policy.TargetEquity, s.policy.TargetBond, s.policy.TargetCash)
	}

	// Missing targets leave the policy alone.
	s.applyReview("weekly review", &gemini.Review{}, cs, block)
	if s.policy.TargetEquity != 75 {
		t.Error("missing targets must not change the policy")
	}
}

func TestReviewActionValidationAndExecution(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	s.fetchBond = func(sym string) (*indicators.Result, error) { return bondInd(), nil }
	cs := newCycle(fill("OLD", "buy", "market", "2026-10-14 10:30", 25)) // opened yesterday
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 75_000)
	hold(cs, "OLD", 5_000)
	hold(cs, "SPY", 20_000)

	r := &gemini.Review{Summary: "rotate"}
	r.Actions = []gemini.ReviewAction{
		{Action: "SELL", Symbol: "OLD", Conviction: 8, Role: "core", Catalyst: "VIX: 27.3", Rationale: "cuts equity risk in a risk-off regime"},
		{Action: "SELL", Symbol: "NOTHELD", Conviction: 8, Role: "core", Rationale: "x"},
		{Action: "BUY", Symbol: "AAPL", Conviction: 8, Role: "growth", Rationale: "not a bond ETF"},
		{Action: "BUY", Symbol: "SHY", Conviction: 8, Role: "income", Catalyst: "VIX: 27.3", Rationale: "short duration"},
	}
	s.applyReview("weekly review", r, cs, block)
	if len(cs.pending) != 1 || cs.pending[0].symbol != "OLD" {
		t.Fatalf("only the first 2 actions are considered and NOTHELD is not held; pending = %+v", cs.pending)
	}

	// The review's SELL is exempt from MIN_HOLD_DAYS (OLD is 1 day old) but counts toward the caps.
	s.executeDecisions(cs)
	if got := o.symbols(broker.Sell); len(got) != 1 || got[0] != "OLD" {
		t.Errorf("rebalancing SELL must bypass the holding period: %+v drops %v", o.placed, cs.dropReasons)
	}
	if cs.tradesDay != 1 || cs.tradesWeek != 2 { // week also holds yesterday's OLD buy
		t.Errorf("review SELL must count toward the caps: %d/%d", cs.tradesDay, cs.tradesWeek)
	}

	// An ordinary (non-review) SELL of the same young position is still held back.
	s, o = newTestScheduler(baseConfig())
	cs = newCycle(fill("OLD", "buy", "market", "2026-10-14 10:30", 25))
	p := sell(cs, "OLD")
	cs.pending = []pendingDecision{p}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["min-hold"] != 1 {
		t.Errorf("non-review SELL: %+v %v", o.placed, cs.dropReasons)
	}
}

func TestReviewBuyGoesThroughEveryGuard(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	s.fetchBond = func(sym string) (*indicators.Result, error) { return bondInd(), nil }
	cs := newCycle(
		fill("X", "buy", "market", "2026-10-15 10:00", 1),
		fill("Y", "buy", "market", "2026-10-15 10:05", 1), // daily cap 2 already used
	)
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 100_000)
	r := &gemini.Review{Actions: []gemini.ReviewAction{
		{Action: "BUY", Symbol: "SGOV", Conviction: 8, Role: "cash_parking", Catalyst: "cash 41.0% (band 5-30%, target 10%)", Rationale: "park idle cash"},
	}}
	s.applyReview("weekly review", r, cs, block)
	if len(cs.pending) != 1 {
		t.Fatalf("pending = %+v", cs.pending)
	}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["cap"] != 1 {
		t.Errorf("turnover caps apply to review actions: %+v %v", o.placed, cs.dropReasons)
	}

	// A BUY of a symbol that is not an enabled bond ETF never becomes a decision.
	s, _ = newTestScheduler(baseConfig())
	cs = newCycle()
	s.fetchBond = func(string) (*indicators.Result, error) { return nil, errTest }
	s.applyReview("weekly review", &gemini.Review{Actions: []gemini.ReviewAction{{Action: "BUY", Symbol: "SHY", Conviction: 8}}}, cs, block)
	if len(cs.pending) != 0 {
		t.Error("a bond ETF whose market data fails must be rejected")
	}
}

type testErr string

func (e testErr) Error() string { return string(e) }

const errTest = testErr("no data")

func TestCycleSummaryDropReasons(t *testing.T) {
	cs := newCycle()
	cs.drop("A", "BUY", "catalyst", "x")
	cs.drop("B", "BUY", "catalyst", "x")
	cs.drop("C", "SELL", "cap", "x")
	if cs.blocked != 3 || cs.dropReasons["catalyst"] != 2 || cs.dropReasons["cap"] != 1 {
		t.Errorf("blocked=%d reasons=%v", cs.blocked, cs.dropReasons)
	}
}

func TestBuildPortfolioDegradesWhenMacroIsMissing(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	s.cfg.Macro = func() (indicators.Macro, error) { return nil, errTest }
	cs := newCycle()
	s.buildPortfolio(cs)
	if cs.pf == nil || cs.pf.regime.Available || cs.pf.regime.Label != "unavailable" {
		t.Fatalf("pf = %+v", cs.pf)
	}
	if cs.regimeAvailable() {
		t.Error("regime must read as unavailable")
	}
	txt := analysis.Render(cs.pf.in, nil)
	if !strings.Contains(txt, "Macro regime: unavailable") || !strings.Contains(txt, "volatility / beta: unavailable") {
		t.Errorf("block does not degrade:\n%s", txt)
	}

	// With complete macro data the label is computed in Go and shown.
	s.cfg.Macro = func() (indicators.Macro, error) {
		q := func(v float64) indicators.MacroQuote { return indicators.MacroQuote{Close: &v} }
		ma := func(v, m float64) indicators.MacroQuote { return indicators.MacroQuote{Close: &v, MA200: &m} }
		return indicators.Macro{"^IRX": q(5.2), "^FVX": q(4.4), "^TNX": q(4.5), "^TYX": q(4.7), "^VIX": q(27), "SPY": ma(480, 500), "TLT": ma(85, 90)}, nil
	}
	cs = newCycle()
	s.buildPortfolio(cs)
	if !cs.regimeAvailable() || cs.pf.regime.Label != "risk-off, rates-rising, inverted curve" {
		t.Errorf("regime = %+v", cs.pf.regime)
	}
}
