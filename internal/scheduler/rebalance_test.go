package scheduler

import (
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/trades"
	"github.com/francocaccia/quantengine/internal/universe"
)

const richBlock = "## PORTFOLIO\n" +
	"Sleeves vs IPS bands (% of equity): equities 31.0% (band 40-75%, target 60%) | bonds 0.0% (band 15-40%, target 30%) | cash 69.0% (band 5-30%, target 10%)\n" +
	"Cash: 69.0% of equity is actual cash (T-bill ETFs count toward the cash sleeve)\n" +
	"Macro regime: risk-off, inverted curve\n" +
	"Yields: 13w 5.20% | 5y 4.40% | 10y 4.50% | 30y 4.70%\n" +
	"VIX: 27.3\n"

func TestMacroCatalystAcceptsOneItemAndRejectsConcatenations(t *testing.T) {
	tests := []struct {
		name, catalyst string
		want           bool
	}{
		{"full line", "VIX: 27.3", true},
		{"full line, case and spaces", "  vix:   27.3 ", true},
		{"full line in quotes", `"VIX: 27.3"`, true},
		{"full regime line", "Macro regime: risk-off, inverted curve", true},
		{"regime label alone", "risk-off, inverted curve", true},
		{"one label: value token of a line", "10y 4.50%", true},
		{"first token of the sleeves line", "equities 31.0% (band 40-75%, target 60%)", true},
		{"cash token", "cash 69.0% (band 5-30%, target 10%)", true},
		{"cash line", "Cash: 69.0% of equity is actual cash (T-bill ETFs count toward the cash sleeve)", true},
		{"two lines glued with a space", "VIX: 27.3 Macro regime: risk-off, inverted curve", false},
		{"two lines glued with a separator", "VIX: 27.3 | Macro regime: risk-off", false},
		{"two phrases from different lines", "risk-off and VIX 27.3", false},
		{"two tokens of one line", "13w 5.20% | 5y 4.40%", false},
		{"bare substring", "VIX", false},
		{"substring of a token", "69.0%", false},
		{"section header", "## PORTFOLIO", false},
		{"none", "none", false},
		{"empty", "", false},
		{"placeholder", "Macro regime: unavailable", false},
	}
	for _, tc := range tests {
		if got := macroCatalystMatches(tc.catalyst, richBlock); got != tc.want {
			t.Errorf("%s: macroCatalystMatches(%q) = %v, want %v", tc.name, tc.catalyst, got, tc.want)
		}
	}
	if !macroCatalystMatches("Macro regime: unavailable", "Macro regime: unavailable\n") == false {
		t.Error("placeholder must never match even when printed")
	}
}

// reviewScheduler is a scheduler whose core list includes SPY and QQQ.
func reviewScheduler(cfg Config) (*Scheduler, *fakeOrders) {
	cfg.Universe = universe.New([]string{"SPY", "QQQ", "AAPL"}, 0, nil, nil, nil)
	s, o := newTestScheduler(cfg)
	s.fetchBond = func(string) (*indicators.Result, error) { return ind(), nil }
	return s, o
}

func rebalanceCycle(s *Scheduler) *cycleState {
	cs := newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 69_000)
	hold(cs, "AAPL", 31_000) // equities 31%, cash 69%: cash is 39 points over its 30% max
	return cs
}

func reviewBuy(sym string, conviction int, catalyst string) gemini.ReviewAction {
	return gemini.ReviewAction{Action: "BUY", Symbol: sym, Conviction: conviction, Role: "core", Catalyst: catalyst,
		Rationale: "raises the equity sleeve toward its band"}
}

func TestReviewMayBuyBondSleeveCoreEquityETFsAndCashParking(t *testing.T) {
	s, _ := reviewScheduler(baseConfig())
	for sym, want := range map[string]bool{
		"SPY": true, "QQQ": true, // core equity ETFs
		"SHY": true, "IEF": true, "AGG": true, // bond sleeve
		"SGOV": true, "BIL": true, // cash parking
		"AAPL": false, // a core stock, but not an ETF
		"NVDA": false, "XYZ": false,
	} {
		if got := s.reviewMayBuy(sym); got != want {
			t.Errorf("reviewMayBuy(%s) = %v, want %v", sym, got, want)
		}
	}
	s.cfg.Bonds = portfolio.DisabledBondUniverse()
	if s.reviewMayBuy("SHY") || s.reviewMayBuy("SGOV") {
		t.Error("BOND_MODE=off still disables bond purchases")
	}
	if !s.reviewMayBuy("SPY") {
		t.Error("core equity ETFs do not depend on the bond universe")
	}
}

func TestReviewBuyStillRequiresRationaleAndKeepsEveryGuard(t *testing.T) {
	s, o := reviewScheduler(baseConfig())
	cs := rebalanceCycle(s)
	noRationale := reviewBuy("SPY", 9, "x")
	noRationale.Rationale = ""
	lowConviction := reviewBuy("QQQ", 5, "x")
	s.applyReview("weekly review", &gemini.Review{Actions: []gemini.ReviewAction{noRationale, lowConviction}}, cs, richBlock)
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["role/rationale"] != 1 || cs.dropReasons["conviction"] != 1 {
		t.Errorf("orders %+v drops %v", o.placed, cs.dropReasons)
	}

	// Daily cap: two decisions already filled today.
	s, o = reviewScheduler(baseConfig())
	cs = newCycle(fill("X", "buy", "market", "2026-10-15 10:00", 1), fill("Y", "buy", "market", "2026-10-15 10:05", 1))
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 69_000)
	hold(cs, "AAPL", 31_000)
	s.applyReview("weekly review", &gemini.Review{Actions: []gemini.ReviewAction{reviewBuy("SPY", 9, "VIX: 27.3")}}, cs, richBlock)
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["cap"] != 1 {
		t.Errorf("daily cap must hold back a review BUY: %+v %v", o.placed, cs.dropReasons)
	}
}

func TestReviewBuyWithoutBreachNeedsOneItemFromThePortfolioBlock(t *testing.T) {
	inBand := func() (*Scheduler, *fakeOrders, *cycleState) {
		s, o := reviewScheduler(baseConfig())
		cs := newCycle()
		withRegime(cs, true)
		withAccount(s, cs, 100_000, 30_000)
		hold(cs, "SPY", 55_000)
		hold(cs, "IEF", 15_000) // 55/15/30: every sleeve inside its band
		return s, o, cs
	}

	s, o, cs := inBand()
	s.applyReview("weekly review", &gemini.Review{Actions: []gemini.ReviewAction{reviewBuy("QQQ", 9, "VIX: 27.3")}}, cs, richBlock)
	if len(cs.pending) != 1 {
		t.Fatalf("an in-band weekly BUY is queued: %+v", cs.pending)
	}
	s.executeDecisions(cs)
	if _, ok := o.order("QQQ"); !ok {
		t.Errorf("one verbatim PORTFOLIO line is accepted: drops %v", cs.dropReasons)
	}

	s, o, cs = inBand()
	s.applyReview("weekly review", &gemini.Review{Actions: []gemini.ReviewAction{reviewBuy("QQQ", 9, "VIX: 27.3 Macro regime: risk-off, inverted curve")}}, cs, richBlock)
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["catalyst"] != 1 {
		t.Errorf("a concatenation must be rejected: %+v %v", o.placed, cs.dropReasons)
	}
}

func TestReviewBuyOfASleeveAtItsTargetIsRejected(t *testing.T) {
	s, o := reviewScheduler(baseConfig())
	cs := newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 38_000)
	hold(cs, "AAPL", 62_000) // equities 62% >= the 60% target
	s.applyReview("weekly review", &gemini.Review{Actions: []gemini.ReviewAction{reviewBuy("SPY", 9, "VIX: 27.3")}}, cs, richBlock)
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["sleeve-target"] != 1 {
		t.Errorf("the sleeve is not under target: %+v %v", o.placed, cs.dropReasons)
	}
}

func TestReviewRunsAtMostOncePerDayAndNeverWhileGeminiIsPaused(t *testing.T) {
	// Monday, the weekly review already ran today: not again.
	s, _ := newTestScheduler(baseConfig())
	cs := rebalanceCycle(s)
	s.lastReviewDay = "2026-10-12"
	if r, why := s.reviewDecision(cs, session("2026-10-12", "11:00")); r != "" || !strings.Contains(why, "already ran") {
		t.Errorf("second review the same day: %q (%q)", r, why)
	}

	// Paused: no call, the day is not marked as done, and the [Review] line says why.
	s, _ = newTestScheduler(baseConfig())
	cs = rebalanceCycle(s)
	s.geminiPaused = func() (bool, string) { return true, "daily quota exhausted" }
	s.llm = &fakeLLM{}
	s.maybeReview(cs, session("2026-10-12", "11:00"))
	if s.lastReviewDay != "" || len(cs.pending) != 0 {
		t.Errorf("a paused client must skip the review without consuming the day: %q %+v", s.lastReviewDay, cs.pending)
	}
	if len(cs.whyNot) != 1 || !strings.Contains(cs.whyNot[0], "Gemini is paused (daily quota exhausted)") {
		t.Errorf("reason = %v", cs.whyNot)
	}
}

func TestStartupBreakdownListsWhatWasCounted(t *testing.T) {
	parent := func(sym, at, id string) trades.Fill {
		f := fill(sym, "buy", "market", at, 10)
		f.ClientOrderID, f.OrderClass = id, "bracket"
		return f
	}
	leg := fill("AAA", "sell", "stop", "2026-10-14 11:00", 10)
	leg.ParentID, leg.OrderClass = "p1", "bracket"
	snap := trades.NewSnapshot([]trades.Fill{
		parent("AAA", "2026-10-14 10:00", "atb-abc123"),
		parent("BBB", "2026-10-13 10:00", ""), // legacy, before the start date
		leg,
		fill("CCC", "sell", "market", "2026-10-15 10:30", 5),
	}, testDays, testNow)
	snap.SetCountStart("2026-10-14")
	out := strings.Join(breakdownLines(snap), "\n")
	for _, want := range []string{"counted today=1", "=2;", "GUARDS_START_DATE=2026-10-14", "AAA", "buy", "atb-abc123",
		"CCC", "(legacy, no atb- prefix)", "bracket/OTO leg x1", "before GUARDS_START_DATE x1"} {
		if !strings.Contains(out, want) {
			t.Errorf("breakdown missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "BBB") {
		t.Errorf("a trade before GUARDS_START_DATE must not be listed as counted:\n%s", out)
	}
}

func TestGuardsStartDateResetsTheCountersInTheCycle(t *testing.T) {
	// Six decisions on 10-13 (the "13/5" scenario), nothing since: with the start
	// date after them the caps read 0/0 and a BUY is allowed.
	var fills []trades.Fill
	for _, sym := range []string{"A", "B", "C"} {
		fills = append(fills, fill(sym, "buy", "market", "2026-10-13 10:00", 1), fill(sym, "sell", "market", "2026-10-13 15:00", 1))
	}
	snap := trades.NewSnapshot(fills, testDays, testNow)
	if _, week := snap.DecisionCounts(); week != 6 {
		t.Fatalf("setup: week = %d", week)
	}
	snap.SetCountStart("2026-10-14")
	if day, week := snap.DecisionCounts(); day != 0 || week != 0 {
		t.Errorf("counts after GUARDS_START_DATE = %d/%d", day, week)
	}
}
