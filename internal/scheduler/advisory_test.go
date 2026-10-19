package scheduler

import (
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/indicators"
)

// A weekly-review BUY of a bond ETF as the review queues it (conviction 9 so that
// it is processed before the conviction-8 stock buys).
func reviewBondBuy(sym string) pendingDecision {
	p := bondBuy(sym, "VIX: 27.3")
	p.decision.Conviction = 9
	return p
}

// 2026-10-19: in advisory mode the weekly review's BUY SHY was logged as a real
// buy, submitted, and counted as a trade (today 1/2, week 5/5), which then
// blocked three discretionary buys. In advisory mode nothing of the sort may happen.
func TestAdvisoryReviewBuyIsNotSubmittedAndTakesNoTradeCap(t *testing.T) {
	cfg := baseConfig()
	cfg.IPSBandsMode = "advisory"
	s, o := newTestScheduler(cfg)
	cs := newCycle(
		fill("X", "buy", "market", "2026-10-12 10:00", 1), fill("Y", "buy", "market", "2026-10-13 10:05", 1),
		fill("Z", "buy", "market", "2026-10-14 10:00", 1), fill("W", "buy", "market", "2026-10-14 10:05", 1),
	) // 4 decisions in the week already, none today
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 100_000)
	logs := captureSchedLog(t)
	cs.pending = []pendingDecision{reviewBondBuy("SHY"), buy("MSFT", 8, headline), buy("NVDA", 8, headline)}
	dayBefore, weekBefore := cs.tradesDay, cs.tradesWeek
	s.executeDecisions(cs)

	for _, ord := range o.placed {
		if ord.symbol == "SHY" {
			t.Fatalf("a policy BUY was submitted in advisory mode: %+v", ord)
		}
	}
	// The one discretionary slot left in the week goes to a stock, not to the advisory BUY.
	if len(o.placed) != 1 || o.placed[0].symbol != "MSFT" && o.placed[0].symbol != "NVDA" {
		t.Errorf("orders = %+v", o.placed)
	}
	if cs.tradesDay != dayBefore+1 || cs.tradesWeek != weekBefore+1 {
		t.Errorf("only the real stock order may count: today %d->%d week %d->%d", dayBefore, cs.tradesDay, weekBefore, cs.tradesWeek)
	}
	if cs.positions["SHY"] != nil {
		t.Error("an advisory BUY must not appear in the cycle's positions")
	}
	out := logs.String()
	if !strings.Contains(out, "[SHY] would BUY 50 shares") || !strings.Contains(out, "(advisory, not submitted)") {
		t.Errorf("the advisory BUY is not reported:\n%s", out)
	}
	if strings.Contains(out, "[SHY] BUY ") {
		t.Errorf("an advisory BUY must not be logged as a real one:\n%s", out)
	}
	if !strings.Contains(strings.Join(cs.whyNot, ";"), "advisory: would BUY SHY") {
		t.Errorf("the [Review] line should say it: %v", cs.whyNot)
	}
	if strings.Contains(s.reviewLine(cs, session("2026-10-19", "10:16")), "action=none") && len(cs.did) > 0 {
		t.Error("actions and line disagree")
	}
}

func TestAdvisoryDryRunLeavesNoTraceWhateverTheBuyDoes(t *testing.T) {
	cfg := baseConfig()
	cfg.IPSBandsMode = "advisory"
	s, o := newTestScheduler(cfg)
	cs := newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 100_000)
	hold(cs, "IEF", 5_000)
	cs.pending = []pendingDecision{reviewBondBuy("IEF"), reviewBondBuy("SHY")}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || o.cancelled != 0 {
		t.Errorf("orders %+v cancelled %d", o.placed, o.cancelled)
	}
	if cs.positions["IEF"].MarketVal != 5_000 || cs.positions["IEF"].Qty != 50 || cs.positions["SHY"] != nil {
		t.Errorf("positions were changed: %+v", cs.positions)
	}
	if cs.tradesDay != 0 || cs.tradesWeek != 0 || len(cs.did) != 0 {
		t.Errorf("counters %d/%d did %v", cs.tradesDay, cs.tradesWeek, cs.did)
	}
	if s.dry || s.orders != orderPlacer(o) {
		t.Error("the real broker must be back after the dry run")
	}
}

func TestAdvisoryReviewRotationDoesNotSellAnything(t *testing.T) {
	cfg := baseConfig()
	cfg.IPSBandsMode = "advisory"
	s, o := newTestScheduler(cfg)
	cs := newCycle(fill("OLD", "buy", "market", "2026-10-10 10:00", 25))
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 100_000)
	hold(cs, "OLD", 5_000)
	p := reviewBondBuy("SHY")
	p.decision.Replaces = "OLD"
	cs.pending = []pendingDecision{p}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || o.cancelled != 0 || cs.positions["OLD"] == nil || cs.sold["OLD"] {
		t.Errorf("placed %+v cancelled %d positions %v sold %v", o.placed, o.cancelled, cs.positions, cs.sold)
	}
}

// Outside advisory mode the same review BUY is submitted as before.
func TestGradualReviewBuyIsStillSubmitted(t *testing.T) {
	for _, mode := range []string{"gradual", "off", ""} {
		cfg := baseConfig()
		cfg.IPSBandsMode = mode
		s, o := newTestScheduler(cfg)
		cs := newCycle()
		withRegime(cs, true)
		withAccount(s, cs, 100_000, 100_000)
		cs.pending = []pendingDecision{reviewBondBuy("SHY")}
		s.executeDecisions(cs)
		if ord, ok := o.order("SHY"); !ok || ord.orderKind != broker.KindEntry || cs.tradesDay != 1 {
			t.Errorf("mode %q: order %+v placed=%v today=%d", mode, ord, ok, cs.tradesDay)
		}
	}
}

// Bond ETF buys carry no stop or one at least MIN_POLICY_STOP_PCT away, whoever asked.
func TestBondETFBuyStopIsAtLeastThreePercentAway(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	cs := newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{reviewBondBuy("SHY")}
	cs.pending[0].ind = calmETF() // 3 x 0.8 ATR = 2.4%
	s.executeDecisions(cs)
	ord, ok := o.order("SHY")
	if !ok || ord.kind != "oto" || 100-ord.stop < 3-1e-9 {
		t.Errorf("order = %+v", ord)
	}

	// A farther ATR stop is kept.
	s, o = newTestScheduler(baseConfig())
	cs = newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{bondBuy("SHY", "VIX: 27.3")}
	cs.pending[0].ind = &indicators.Result{Price: 100, ATR: 2}
	s.executeDecisions(cs)
	if ord, _ := o.order("SHY"); ord.stop != 94 {
		t.Errorf("a 6%% ATR stop must stay: %+v", ord)
	}

	// No stop configured: a plain market order.
	cfg := baseConfig()
	cfg.MinPolicyStopPct = -1
	s, o = newTestScheduler(cfg)
	cs = newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{bondBuy("SHY", "VIX: 27.3")}
	s.executeDecisions(cs)
	if ord, ok := o.order("SHY"); !ok || ord.kind != "market" || ord.stop != 0 {
		t.Errorf("no-stop mode: %+v", ord)
	}
}
