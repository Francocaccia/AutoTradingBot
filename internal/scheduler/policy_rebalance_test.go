package scheduler

import (
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/regime"
	"github.com/francocaccia/quantengine/internal/trades"
)

// policyScheduler is a scheduler on the default (gradual) policy whose core list
// is SPY, QQQ and AAPL; every ETF is a 100-dollar instrument with a 2.5 ATR.
func policyScheduler(mode string) (*Scheduler, *fakeOrders) {
	cfg := baseConfig()
	cfg.IPSBandsMode = mode
	s, o := reviewScheduler(cfg)
	s.llm = nil
	return s, o
}

// policyCycle is a Thursday 2026-10-15 11:00 ET cycle on a 100k account.
func policyCycle(s *Scheduler, cash float64, held map[string]float64, fills ...trades.Fill) *cycleState {
	cs := newCycle(fills...)
	withRegime(cs, true)
	withAccount(s, cs, 100_000, cash)
	for sym, v := range held {
		hold(cs, sym, v)
	}
	return cs
}

// rebFill is a filled rebalancing buy, identified by its client_order_id.
func rebFill(sym, at string) trades.Fill {
	f := fill(sym, "buy", "market", at, 10)
	f.OrderClass = "oto"
	f.ClientOrderID = "atb-" + strings.ReplaceAll(at[:10], "-", "") + "-" + sym + "-buy-rebalance"
	return f
}

func runPolicy(s *Scheduler, cs *cycleState) {
	s.planRebalance(cs, session("2026-10-15", "11:00"))
	s.executeDecisions(cs)
}

// The exact situation of the audit: 28% equities, no bonds, 72% cash.
func TestGradualRebalanceBuysABondETFTrancheFirst(t *testing.T) {
	s, o := policyScheduler("")
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 14_000, "NVDA": 14_000})
	runPolicy(s, cs)

	if len(o.placed) != 1 {
		t.Fatalf("orders = %+v (why: %v)", o.placed, cs.whyNot)
	}
	ord := o.placed[0]
	// Bonds are 15 points under their minimum, equities 12: bonds first.
	if ord.symbol != "AGG" || ord.side != broker.Buy || ord.kind != "oto" || ord.orderKind != broker.KindRebalance {
		t.Errorf("order = %+v, want an OTO rebalance BUY of AGG", ord)
	}
	// 5% of 100k at 100 per share; a stop is attached (3x ATR below).
	if ord.qty != 50 || ord.stop != 92.5 {
		t.Errorf("qty %d stop %v, want 50 and 92.50", ord.qty, ord.stop)
	}
	if got := strings.Join(cs.did, ","); got != "rebalance" {
		t.Errorf("actions = %q", got)
	}
	if cs.tradesDay != 1 || cs.tradesWeek != 1 || cs.rebalDay != 1 || cs.rebalWeek != 1 {
		t.Errorf("counters trades %d/%d rebalance %d/%d", cs.tradesDay, cs.tradesWeek, cs.rebalDay, cs.rebalWeek)
	}
	line := s.reviewLine(cs, session("2026-10-15", "11:00"))
	if !strings.Contains(line, "action=rebalance") || !strings.Contains(line, "skip=-") {
		t.Errorf("review line: %s", line)
	}
}

func TestRebalanceIsExemptFromConvictionAndCatalystButNotFromItsBounds(t *testing.T) {
	s, o := policyScheduler("gradual")
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	cs.stopGemini = true // no model at all this cycle
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].orderKind != broker.KindRebalance {
		t.Fatalf("a policy trade needs neither a model nor a catalyst: %+v (%v)", o.placed, cs.whyNot)
	}
	if cs.blocked != 0 || len(cs.dropReasons) != 0 {
		t.Errorf("no guard of the model path applies: %v", cs.dropReasons)
	}
}

func TestTrancheIsAtMostFivePercentAndAtMostTheDeficit(t *testing.T) {
	// Bonds 14.0%: one point below the minimum, so the trade is 1% of equity.
	s, o := policyScheduler("gradual")
	cs := policyCycle(s, 31_000, map[string]float64{"SPY": 55_000, "IEF": 14_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].qty != 10 {
		t.Errorf("deficit-limited tranche: %+v", o.placed)
	}

	// A custom tranche size is honoured.
	cfg := baseConfig()
	cfg.RebalanceTranchePct = 2
	s, o = reviewScheduler(cfg)
	cs = policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].qty != 20 {
		t.Errorf("REBALANCE_TRANCHE_PCT=2: %+v", o.placed)
	}

	// A sliver under a point is left alone, silently.
	s, o = policyScheduler("gradual")
	cs = policyCycle(s, 31_500, map[string]float64{"SPY": 55_000, "IEF": 14_500})
	runPolicy(s, cs)
	if len(o.placed) != 0 || len(cs.whyNot) != 0 {
		t.Errorf("a 0.5 point deficit: %+v %v", o.placed, cs.whyNot)
	}
}

func TestEquitySleeveIsFilledWithCoreETFsNeverStocks(t *testing.T) {
	s, o := policyScheduler("gradual")
	// Bonds in band, equities 20%: SPY, QQQ or nothing — never AAPL, which is in the core list but is a stock.
	cs := policyCycle(s, 65_000, map[string]float64{"AAPL": 20_000, "IEF": 15_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].symbol != "SPY" || o.placed[0].orderKind != broker.KindRebalance {
		t.Fatalf("orders = %+v (%v)", o.placed, cs.whyNot)
	}

	// SPY already at the 20% index-ETF limit: the next core ETF, QQQ.
	s, o = policyScheduler("gradual")
	cs = policyCycle(s, 55_000, map[string]float64{"AAPL": 10_000, "SPY": 20_000, "IEF": 15_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].symbol != "QQQ" {
		t.Fatalf("orders = %+v (%v)", o.placed, cs.whyNot)
	}

	// No ETF has market data: the sleeve stays short rather than buying a stock.
	s, o = policyScheduler("gradual")
	s.fetchBond = func(string) (*indicators.Result, error) { return nil, errTest }
	s.fetchRaw = nil
	cs = policyCycle(s, 65_000, map[string]float64{"AAPL": 20_000, "IEF": 15_000})
	runPolicy(s, cs)
	if len(o.placed) != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "no market data") {
		t.Errorf("orders %+v why %v", o.placed, cs.whyNot)
	}
	for _, ord := range o.placed {
		if ord.symbol == "AAPL" {
			t.Error("a single stock must never fill a band")
		}
	}
}

func TestCoreETFFailsOpenWhenOnlyValidationFails(t *testing.T) {
	s, o := policyScheduler("gradual")
	logs := captureSchedLog(t)
	s.fetchBond = func(string) (*indicators.Result, error) { return nil, errTest } // universe validation down
	s.fetchRaw = func(string) (*indicators.Result, error) { return ind(), nil }    // raw market data is fine
	cs := policyCycle(s, 65_000, map[string]float64{"AAPL": 20_000, "IEF": 15_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].symbol != "SPY" {
		t.Fatalf("a data outage must not stop the policy for a core ETF: %+v (%v)", o.placed, cs.whyNot)
	}
	if !strings.Contains(logs.String(), "WARN SPY") {
		t.Errorf("failing open must be a warning:\n%s", logs)
	}
}

func TestBondSleeveUsesOnlyEnabledNonCashBondETFs(t *testing.T) {
	s, _ := policyScheduler("gradual")
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	if got := strings.Join(s.bondCandidates(cs), ","); got != "AGG,SHY" {
		t.Errorf("default candidates = %q", got)
	}
	// Without macro data only the ETFs that are safe to buy blind remain.
	withRegime(cs, false)
	if got := strings.Join(s.bondCandidates(cs), ","); got != "SHY" {
		t.Errorf("no macro: %q", got)
	}
	// Rising rates put the shortest duration first.
	cs.pf = &pfContext{regime: regime.Regime{Available: true, Label: "x", RatesRising: true}}
	if got := strings.Join(s.bondCandidates(cs), ","); got != "SHY,AGG" {
		t.Errorf("rates rising: %q", got)
	}
	// A configured list is used, but T-bill ETFs (cash sleeve), unknown symbols and disabled ETFs are not.
	s.cfg.RebalanceBondETFs = []string{"SGOV", "XYZ", "IEF", "AGG", "TLT"}
	s.cfg.Bonds = portfolio.NewBondUniverse([]string{"IEF", "AGG", "SGOV"})
	cs.pf = &pfContext{regime: regime.Regime{Available: true, Label: "x"}}
	if got := strings.Join(s.bondCandidates(cs), ","); got != "IEF,AGG" {
		t.Errorf("configured list: %q", got)
	}
	// BOND_MODE=off leaves nothing to buy and says so.
	s.cfg.Bonds = portfolio.DisabledBondUniverse()
	if got := s.bondCandidates(cs); len(got) != 0 {
		t.Errorf("bond mode off: %v", got)
	}
}

func TestAtMostOneRebalanceTradePerDay(t *testing.T) {
	s, o := policyScheduler("gradual")
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000}, rebFill("AGG", "2026-10-15 10:15"))
	if cs.history == nil {
		t.Fatal("setup")
	}
	runPolicy(s, cs)
	if len(o.placed) != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "one trade per day already made") {
		t.Errorf("orders %+v why %v", o.placed, cs.whyNot)
	}
}

func TestWeeklyRebalanceLimit(t *testing.T) {
	s, o := policyScheduler("gradual")
	// Two earlier tranches in the last five trading days, none today.
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000},
		rebFill("AGG", "2026-10-13 10:15"), rebFill("SPY", "2026-10-14 10:15"))
	runPolicy(s, cs)
	if len(o.placed) != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "weekly limit reached (2 of 2") {
		t.Errorf("orders %+v why %v", o.placed, cs.whyNot)
	}

	// One earlier tranche: one more is allowed (limit 2), and then the day is used up.
	s, o = policyScheduler("gradual")
	cs = policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000}, rebFill("AGG", "2026-10-13 10:15"))
	runPolicy(s, cs)
	if len(o.placed) != 1 {
		t.Fatalf("second tranche of the week: %+v (%v)", o.placed, cs.whyNot)
	}
	if cs.rebalDay != 1 || cs.rebalWeek != 2 {
		t.Errorf("counters %d/%d", cs.rebalDay, cs.rebalWeek)
	}
	// The next cycle reads the fill back from the order history: a tranche was
	// made today, so no second one is planned.
	cs2 := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000},
		rebFill("AGG", "2026-10-13 10:15"), rebFill("SHY", "2026-10-15 11:00"))
	s.planRebalance(cs2, session("2026-10-15", "11:15"))
	if cs2.rebal.plan != nil || !strings.Contains(strings.Join(cs2.whyNot, ";"), "one trade per day already made") {
		t.Errorf("a second tranche the same day must not be planned: %v", cs2.whyNot)
	}

	// A custom weekly limit.
	cfg := baseConfig()
	cfg.RebalanceMaxTradesPerWeek = 1
	s, o = reviewScheduler(cfg)
	cs = policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000}, rebFill("AGG", "2026-10-13 10:15"))
	runPolicy(s, cs)
	if len(o.placed) != 0 {
		t.Errorf("REBALANCE_MAX_TRADES_PER_WEEK=1: %+v", o.placed)
	}
}

func TestRebalanceRespectsTheSharedTradeCaps(t *testing.T) {
	s, o := policyScheduler("gradual")
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000},
		fill("X", "buy", "market", "2026-10-15 10:00", 1), fill("Y", "buy", "market", "2026-10-15 10:05", 1)) // 2/2 today
	runPolicy(s, cs)
	if len(o.placed) != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "trade cap reached (today 2/2") {
		t.Errorf("orders %+v why %v", o.placed, cs.whyNot)
	}
}

// A discretionary BUY must not use the last daily slot before the policy trade.
func TestRebalanceGoesBeforeDiscretionaryBuys(t *testing.T) {
	s, o := policyScheduler("gradual")
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000},
		fill("X", "buy", "market", "2026-10-15 10:00", 1)) // 1 of 2 slots used
	cs.pending = []pendingDecision{buy("MSFT", 9, headline)}
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].orderKind != broker.KindRebalance {
		t.Fatalf("the policy tranche takes the last slot: %+v", o.placed)
	}
	if cs.dropReasons["cap"] != 1 {
		t.Errorf("the discretionary BUY is the one the cap refuses: %v", cs.dropReasons)
	}
}

func TestRebalanceRespectsCashFloorAndMaxRisk(t *testing.T) {
	// Real cash is at the 2% floor: nothing can be bought.
	s, o := policyScheduler("gradual")
	cs := policyCycle(s, 2_000, map[string]float64{"AAPL": 28_000, "TSLA": 70_000})
	runPolicy(s, cs)
	if len(o.placed) != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "policy limit") {
		t.Errorf("orders %+v why %v", o.placed, cs.whyNot)
	}

	// Only 3k above the floor: the trade shrinks to what the floor allows.
	s, o = policyScheduler("gradual")
	cs = policyCycle(s, 5_000, map[string]float64{"AAPL": 28_000, "TSLA": 67_000})
	runPolicy(s, cs)
	if len(o.placed) != 0 {
		// cash sleeve band minimum (5%) leaves no room either
		t.Errorf("the cash sleeve minimum and the real cash floor both bind: %+v", o.placed)
	}

	// A tight max-risk cap: the stop is 7.5 per share, so a loss budget of 0.004% of
	// equity (4 dollars) allows no share.
	cfg := baseConfig()
	cfg.MaxRiskPct = 0.004
	s, o = reviewScheduler(cfg)
	cs = policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s, cs)
	if len(o.placed) != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "allows less than one share") {
		t.Errorf("orders %+v why %v", o.placed, cs.whyNot)
	}

	// A looser one trims the quantity: 0.2% of 100k = 200 / 7.5 per share = 26 shares.
	cfg.MaxRiskPct = 0.2
	s, o = reviewScheduler(cfg)
	cs = policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].qty != 26 {
		t.Errorf("risk-capped tranche: %+v (%v)", o.placed, cs.whyNot)
	}
}

func TestRebalanceKeepsTheReentryCooldown(t *testing.T) {
	s, o := policyScheduler("gradual")
	sold := fill("AGG", "sell", "market", "2026-10-14 10:30", 10) // sold yesterday: 1 trading day ago
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000}, sold)
	runPolicy(s, cs)
	// AGG is in cooldown, so the next bond ETF in the list is used.
	if len(o.placed) != 1 || o.placed[0].symbol != "SHY" {
		t.Fatalf("orders %+v (%v)", o.placed, cs.whyNot)
	}

	// Every bond candidate in cooldown and no other sleeve short: no trade, with the reason.
	s, o = policyScheduler("gradual")
	cs = policyCycle(s, 45_000, map[string]float64{"SPY": 55_000},
		fill("AGG", "sell", "market", "2026-10-14 10:30", 10), fill("SHY", "sell", "market", "2026-10-14 10:35", 10))
	runPolicy(s, cs)
	if len(o.placed) != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "re-entry cooldown") {
		t.Errorf("orders %+v why %v", o.placed, cs.whyNot)
	}
}

func TestSleeveAboveItsMaximumIsNeverSoldDown(t *testing.T) {
	s, o := policyScheduler("gradual")
	logs := captureSchedLog(t)
	// Equities 80% (max 75): no purchase into the sleeve and no forced SELL.
	cs := policyCycle(s, 20_000, map[string]float64{"SPY": 40_000, "QQQ": 40_000})
	runPolicy(s, cs)
	for _, ord := range o.placed {
		if ord.side == broker.Sell {
			t.Errorf("rebalancing must not force a SELL: %+v", ord)
		}
		if ord.symbol == "SPY" || ord.symbol == "QQQ" {
			t.Errorf("no purchases into a sleeve above its maximum: %+v", ord)
		}
	}
	if !strings.Contains(logs.String(), "equities 80.0% is above its 75% maximum: not selling") {
		t.Errorf("the breach must be logged:\n%s", logs)
	}

	// The policy's own room refuses an equity BUY over the band maximum, in any mode.
	room, bind := s.policy.Room(s.bookFrom(cs, cs.account), "VOO", "")
	if room != 0 || !strings.Contains(bind, "equity band maximum") {
		t.Errorf("room %.0f binding %q", room, bind)
	}
}

func TestCashAboveItsMaximumResolvesByTheOtherSleevesFilling(t *testing.T) {
	// Cash 72% alone never triggers a trade; the other sleeves' deficits do.
	s, o := policyScheduler("gradual")
	cs := policyCycle(s, 72_000, map[string]float64{"SPY": 55_000, "IEF": 15_000}) // 55/15/30 in band, cash is the rest
	withAccount(s, cs, 100_000, 30_000)
	runPolicy(s, cs)
	if len(o.placed) != 0 {
		t.Errorf("nothing to fill: %+v", o.placed)
	}
}

func TestAdvisoryModeLogsWhatItWouldDoAndPlacesNothing(t *testing.T) {
	s, o := policyScheduler("advisory")
	logs := captureSchedLog(t)
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s, cs)
	if len(o.placed) != 0 || cs.rebal.plan != nil || len(cs.did) != 0 {
		t.Errorf("advisory placed something: %+v %v", o.placed, cs.did)
	}
	if !strings.Contains(logs.String(), "advisory (IPS_BANDS_MODE=advisory), no order placed: would BUY 50 AGG") {
		t.Errorf("the intended trade is not logged:\n%s", logs)
	}
	if line := s.reviewLine(cs, session("2026-10-15", "11:00")); !strings.Contains(line, "mode=advisory") || !strings.Contains(line, "would BUY 50 AGG") || !strings.Contains(line, "action=none") {
		t.Errorf("review line: %s", line)
	}
}

func TestOffModeDoesNothing(t *testing.T) {
	s, o := policyScheduler("off")
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s, cs)
	if len(o.placed) != 0 || len(cs.did) != 0 {
		t.Errorf("off placed something: %+v", o.placed)
	}
}

func TestRebalanceWaitsForTenAMAndHonoursBuyBlocks(t *testing.T) {
	s, o := policyScheduler("gradual")
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	s.planRebalance(cs, session("2026-10-15", "09:45"))
	s.executeDecisions(cs)
	if len(o.placed) != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "waits for 10:00 ET") {
		t.Errorf("before 10:00: %+v %v", o.placed, cs.whyNot)
	}

	s, o = policyScheduler("gradual")
	cs = policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	cs.buyBlock = "DRAWDOWN HALT: 10.0% below peak"
	runPolicy(s, cs)
	if len(o.placed) != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "BUYs blocked") {
		t.Errorf("circuit breaker: %+v %v", o.placed, cs.whyNot)
	}

	s, o = policyScheduler("gradual")
	cs = policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	cs.history = nil
	runPolicy(s, cs)
	if len(o.placed) != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "order history unavailable") {
		t.Errorf("no history: %+v %v", o.placed, cs.whyNot)
	}
}

func TestRebalanceIsNotCountedAgainstTheOpenPositionLimit(t *testing.T) {
	s, o := policyScheduler("gradual")
	held := map[string]float64{}
	for _, sym := range []string{"A", "B", "C", "D", "E", "F", "G", "H"} {
		held[sym] = 3_500 // eight positions: the MAX_OPEN_POSITIONS limit, 28% of equity
	}
	cs := policyCycle(s, 72_000, held)
	runPolicy(s, cs)
	if len(o.placed) != 1 {
		t.Errorf("a core ETF tranche is not a new idea to be limited: %+v (%v)", o.placed, cs.whyNot)
	}
}

// A restart must not place the same tranche twice: the broker refuses a second
// order for the same symbol and side on the same day.
func TestRepeatedRebalanceAfterARestartIsRefusedAsDuplicate(t *testing.T) {
	s, o := policyScheduler("gradual")
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 {
		t.Fatalf("setup: %+v", o.placed)
	}

	// A new process knows nothing of the order, the history snapshot predates the
	// fill, and the broker still has it.
	s2, _ := policyScheduler("gradual")
	s2.orders = o
	cs2 := policyCycle(s2, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s2, cs2)
	if len(o.placed) != 1 {
		t.Errorf("the same order was placed twice: %+v", o.placed)
	}
	if !strings.Contains(strings.Join(cs2.whyNot, ";"), "already ordered today") || len(cs2.did) != 0 {
		t.Errorf("why %v did %v", cs2.whyNot, cs2.did)
	}
}

func TestDuplicateDiscretionaryBuyIsADroppedDecisionNotAnError(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	o.seen = map[string]bool{"MSFT buy": true}
	cs := newCycle()
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{buy("MSFT", 9, headline)}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["duplicate"] != 1 || len(cs.did) != 0 {
		t.Errorf("placed %+v drops %v did %v", o.placed, cs.dropReasons, cs.did)
	}
	if cs.tradesDay != 0 {
		t.Errorf("a refused duplicate must not count as a trade: %d", cs.tradesDay)
	}
}

// The exit is checked before the position's stop orders are cancelled: an exit
// that cannot go out must leave the position protected.
func TestDuplicateExitLeavesTheStopOrdersAlone(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	o.seen = map[string]bool{"OLD sell": true}
	cs := newCycle()
	p := sell(cs, "OLD")
	p.fromReview = true // exempt from the holding period
	cs.pending = []pendingDecision{p}
	s.executeDecisions(cs)
	if o.cancelled != 0 || len(o.placed) != 0 || cs.dropReasons["duplicate"] != 1 {
		t.Errorf("cancelled %d placed %+v drops %v", o.cancelled, o.placed, cs.dropReasons)
	}

	s, o = newTestScheduler(baseConfig())
	cs = newCycle()
	p = sell(cs, "OLD")
	p.fromReview = true
	cs.pending = []pendingDecision{p}
	s.executeDecisions(cs)
	if o.cancelled != 1 || len(o.placed) != 1 || o.placed[0].orderKind != broker.KindExit || strings.Join(cs.did, ",") != "sell" {
		t.Errorf("normal exit: cancelled %d placed %+v did %v", o.cancelled, o.placed, cs.did)
	}
}

func TestOrdinaryBuysCarryTheEntryKind(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{buy("MSFT", 9, headline)}
	s.executeDecisions(cs)
	if ord, ok := o.order("MSFT"); !ok || ord.orderKind != broker.KindEntry || strings.Join(cs.did, ",") != "buy" {
		t.Errorf("order %+v did %v", ord, cs.did)
	}
}
