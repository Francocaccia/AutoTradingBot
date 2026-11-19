package scheduler

import (
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

// 2026-11-18: 27 trading days in, the book was 20.5% equities, 5.1% bonds and
// 74.4% cash against bands of 40-75, 15-40 and 5-30, and the bot had never placed a
// single policy trade: the floors were only ever logged. These tests pin the
// behaviour that is meant to prevent a repeat.

func TestNewDefaultsReachTheFloorsInAboutAWeek(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	if s.rebalanceTranchePct() != 10 || s.rebalanceMaxWeek() != 5 {
		t.Errorf("tranche %v%%, %d a week", s.rebalanceTranchePct(), s.rebalanceMaxWeek())
	}
}

// Floors first (the bigger shortfall first), then cash above its maximum is spent.
func TestTheLiveBookIsBuiltToThePolicyInAFewTrades(t *testing.T) {
	cfg := baseConfig()
	cfg.IPSBandsMode = "gradual"
	s, o := reviewScheduler(cfg)
	s.llm = nil
	// equities 20.5%, bonds 5.1%, cash 74.4%, as on the day of the report.
	holdings := map[string]float64{"AAPL": 20_500, "SHY": 5_100}
	cash := 74_400.0
	price := 100.0

	var bought []string
	for day := 1; day <= 12; day++ {
		cs := policyCycle(s, cash, holdings)
		runPolicy(s, cs)
		if len(o.placed) == 0 {
			break
		}
		ord := o.placed[len(o.placed)-1]
		if ord.orderKind != broker.KindRebalance || ord.symbol == "AAPL" {
			t.Fatalf("day %d: %+v", day, ord)
		}
		spent := float64(ord.qty) * price
		holdings[ord.symbol] += spent
		cash -= spent
		bought = append(bought, ord.symbol)
		o.placed, o.seen = nil, nil // the next day
	}
	w := portfolio.Book{Equity: 100_000, Cash: cash, Holdings: holdingsOf(holdings)}.SleeveWeights()
	t.Logf("trades in order: %v; final equities %.1f bonds %.1f cash %.1f", bought, w[portfolio.SleeveEquities], w[portfolio.SleeveBonds], w[portfolio.SleeveCash])
	if w[portfolio.SleeveEquities] < 40 || w[portfolio.SleeveBonds] < 15 || w[portfolio.SleeveCash] > 30+1e-6 {
		t.Errorf("after %v: equities %.1f bonds %.1f cash %.1f", bought, w[portfolio.SleeveEquities], w[portfolio.SleeveBonds], w[portfolio.SleeveCash])
	}
	if w[portfolio.SleeveEquities] > 75 || w[portfolio.SleeveBonds] > 40 || w[portfolio.SleeveCash] < 5 {
		t.Errorf("overshot a band: %+v", w)
	}
	if len(bought) > 7 {
		t.Errorf("took %d trades (%v), a week was the aim", len(bought), bought)
	}
	// Once inside every band nothing more is bought.
	cs := policyCycle(s, cash, holdings)
	runPolicy(s, cs)
	if len(o.placed) != 0 {
		t.Errorf("a book inside every band still traded: %+v", o.placed)
	}
}

func holdingsOf(m map[string]float64) []portfolio.Holding {
	var out []portfolio.Holding
	for sym, v := range m {
		out = append(out, portfolio.Holding{Symbol: sym, Value: v})
	}
	return out
}

// Floors met (45/20/35): cash is still 5 points over its maximum, so the sleeve
// furthest under its target takes it, and no more than the excess.
func TestCashAboveItsMaximumIsSpentOnceTheFloorsAreMet(t *testing.T) {
	s, o := policyScheduler("gradual")
	s.cfg.RebalanceTranchePct = 10
	cs := policyCycle(s, 35_000, map[string]float64{"SPY": 45_000, "IEF": 20_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].orderKind != broker.KindRebalance {
		t.Fatalf("orders %+v (%v)", o.placed, cs.whyNot)
	}
	if spent := float64(o.placed[0].qty) * 100; spent > 5_000+1e-6 {
		t.Errorf("spent %.0f, the excess is 5000", spent)
	}

	// Cash exactly at its maximum: done.
	s, o = policyScheduler("gradual")
	cs = policyCycle(s, 30_000, map[string]float64{"SPY": 50_000, "IEF": 20_000})
	runPolicy(s, cs)
	if len(o.placed) != 0 || len(cs.whyNot) != 0 {
		t.Errorf("cash at its maximum: %+v %v", o.placed, cs.whyNot)
	}

	// A floor still unmet takes priority over the cash excess.
	s, o = policyScheduler("gradual")
	cs = policyCycle(s, 45_000, map[string]float64{"SPY": 50_000, "SHY": 5_000}) // bonds 5% vs a 15% floor
	runPolicy(s, cs)
	if len(o.placed) != 1 || portfolio.Sleeve(o.placed[0].symbol) != portfolio.SleeveBonds {
		t.Errorf("the bond floor comes first: %+v", o.placed)
	}
}

func TestAdvisoryModeShoutsThatThePolicyIsNotEnforced(t *testing.T) {
	s, o := policyScheduler("advisory")
	logs := captureSchedLog(t)
	cs := policyCycle(s, 74_400, map[string]float64{"AAPL": 20_500, "SHY": 5_100})
	runPolicy(s, cs)
	if len(o.placed) != 0 {
		t.Fatalf("advisory placed %+v", o.placed)
	}
	if !strings.Contains(logs.String(), "WARNING: IPS_BANDS_MODE=advisory, so the policy is NOT being enforced") {
		t.Errorf("no warning:\n%s", logs)
	}
}

// ---- policy first ----

func policyFirstScheduler(mode string, policyFirst bool) (*Scheduler, *fakeOrders) {
	cfg := baseConfig()
	cfg.IPSBandsMode, cfg.PolicyFirst = mode, policyFirst
	return newTestScheduler(cfg)
}

func TestStockBuysWaitWhileThePolicyIsUnmet(t *testing.T) {
	s, o := policyFirstScheduler("gradual", true)
	cs := newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 74_400)
	hold(cs, "AAPL", 20_500)
	hold(cs, "SHY", 5_100)
	cs.pending = []pendingDecision{buy("MSFT", 9, headline)}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["policy-first"] != 1 {
		t.Fatalf("orders %+v drops %v", o.placed, cs.dropReasons)
	}
	why := s.policyFirstReason(cs)
	for _, want := range []string{"equities 20.5% is under its 40% floor", "bonds 5.1% is under its 15% floor", "cash 74.4% is over its 30% maximum"} {
		if !strings.Contains(why, want) {
			t.Errorf("reason lacks %q: %s", want, why)
		}
	}
	// Index ETFs and bond ETFs are the core itself: not held back.
	s, o = policyFirstScheduler("gradual", true)
	cs = newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 74_400)
	cs.pending = []pendingDecision{buy("QQQ", 9, headline), bondBuy("SHY", "VIX: 27.3")}
	s.executeDecisions(cs)
	if _, ok := o.order("QQQ"); !ok {
		t.Errorf("an index ETF is not a single-stock pick: %v", cs.dropReasons)
	}
}

func TestStockBuysAreFreeOnceThePolicyIsMetOrWhenTheGateIsOffOrNotGradual(t *testing.T) {
	book := func(s *Scheduler, cash float64, held map[string]float64) *cycleState {
		cs := newCycle()
		withRegime(cs, true)
		withAccount(s, cs, 100_000, cash)
		for sym, v := range held {
			hold(cs, sym, v)
		}
		cs.pending = []pendingDecision{buy("MSFT", 9, headline)}
		return cs
	}
	inBand := map[string]float64{"SPY": 45_000, "IEF": 25_000}

	s, o := policyFirstScheduler("gradual", true)
	cs := book(s, 30_000, inBand) // every sleeve inside its band
	s.executeDecisions(cs)
	if _, ok := o.order("MSFT"); !ok {
		t.Errorf("policy met, the stock pick may go: %v", cs.dropReasons)
	}
	for name, c := range map[string]struct {
		mode  string
		first bool
	}{"gate off": {"gradual", false}, "advisory": {"advisory", true}, "off": {"off", true}} {
		s, o := policyFirstScheduler(c.mode, c.first)
		cs := book(s, 100_000, nil)
		s.executeDecisions(cs)
		if _, ok := o.order("MSFT"); !ok || cs.dropReasons["policy-first"] != 0 {
			t.Errorf("%s: orders %+v drops %v", name, o.placed, cs.dropReasons)
		}
	}
}

// ---- stop-out cooldown ----

func TestAStoppedOutNameIsNotBoughtBackForAWhile(t *testing.T) {
	cfg := baseConfig()
	cfg.StopReentryCooldownDays = 5
	s, o := newTestScheduler(cfg)
	// Stopped out 4 trading days ago: past the ordinary 3-day cooldown, inside the 5-day stop-out one.
	cs := newCycle(fill("MSFT", "sell", "stop", "2026-10-09 10:30", 9))
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{buy("MSFT", 9, headline)}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["stop-cooldown"] != 1 {
		t.Errorf("orders %+v drops %v", o.placed, cs.dropReasons)
	}

	// Stopped out longer ago than the cooldown: allowed.
	s, o = newTestScheduler(cfg)
	cs = newCycle(fill("MSFT", "sell", "stop", "2026-10-06 10:30", 9))
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{buy("MSFT", 9, headline)}
	s.executeDecisions(cs)
	if _, ok := o.order("MSFT"); !ok {
		t.Errorf("past the cooldown: %v", cs.dropReasons)
	}

	// A discretionary sale (not a stop) only has the ordinary short cooldown.
	s, o = newTestScheduler(cfg)
	cs = newCycle(fill("MSFT", "sell", "market", "2026-10-09 10:30", 9)) // 4 trading days ago
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{buy("MSFT", 9, headline)}
	s.executeDecisions(cs)
	if _, ok := o.order("MSFT"); !ok {
		t.Errorf("a market sale is not a stop-out: %v", cs.dropReasons)
	}

	// Disabled.
	cfg.StopReentryCooldownDays = 0
	s, o = newTestScheduler(cfg)
	cs = newCycle(fill("MSFT", "sell", "stop", "2026-10-09 10:30", 9))
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{buy("MSFT", 9, headline)}
	s.executeDecisions(cs)
	if cs.dropReasons["stop-cooldown"] != 0 {
		t.Errorf("0 disables the stop-out cooldown: %v", cs.dropReasons)
	}
}

// The policy tranche is not an idea being retried: the stop-out cooldown is for the model's buys.
func TestPolicyTradesIgnoreTheStopOutCooldown(t *testing.T) {
	s, o := policyScheduler("gradual")
	s.cfg.StopReentryCooldownDays = 20
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000}, fill("AGG", "sell", "stop", "2026-10-14 10:30", 50))
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].symbol != "AGG" {
		t.Errorf("orders %+v (%v)", o.placed, cs.whyNot)
	}
}

// ---- satellite cap ----

func TestSatelliteCapLimitsAllStocksTogetherButNotIndexETFs(t *testing.T) {
	p := portfolio.DefaultPolicy()
	b := portfolio.Book{Equity: 100_000, Cash: 60_000, Holdings: []portfolio.Holding{
		{Symbol: "AAPL", Value: 5_000, Sector: "Tech"}, {Symbol: "AMZN", Value: 5_000, Sector: "Retail"},
		{Symbol: "GOOG", Value: 4_000, Sector: "Comm"}, {Symbol: "SPY", Value: 20_000},
	}}
	room, bind := p.Room(b, "MSFT", "Software") // 14k held in stocks of a 15k cap
	if room != 1_000 || !strings.Contains(bind, "single stocks and sector ETFs combined limit 15%") {
		t.Errorf("stock room %.0f binding %q", room, bind)
	}
	if room, _ := p.Room(b, "QQQ", ""); room != 20_000 { // 20% index-ETF limit, nothing held
		t.Errorf("index ETF room %.0f", room)
	}
	p.MaxSatellitePct = 0
	if room, _ := p.Room(b, "MSFT", "Software"); room < 2_000 {
		t.Errorf("a zero cap means no cap: %.0f", room)
	}
}
