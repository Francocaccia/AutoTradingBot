package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/market"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/risk"
)

// calmETF is a 100-dollar ETF whose ATR is 0.8%: its plain 3x ATR stop would be
// 2.4% away, inside an ordinary two-day swing.
func calmETF() *indicators.Result { return &indicators.Result{Price: 100, ATR: 0.8} }

func TestPolicyStopIsNeverCloserThanTheMinimumDistance(t *testing.T) {
	for _, sleeveHold := range []map[string]float64{
		{"AAPL": 28_000},                // bonds first (bond ETF)
		{"AAPL": 20_000, "IEF": 15_000}, // equity sleeve (broad ETF)
	} {
		s, o := policyScheduler("gradual")
		s.fetchBond = func(string) (*indicators.Result, error) { return calmETF(), nil }
		cs := policyCycle(s, 65_000, sleeveHold)
		runPolicy(s, cs)
		if len(o.placed) != 1 || o.placed[0].kind != "oto" {
			t.Fatalf("orders = %+v (%v)", o.placed, cs.whyNot)
		}
		ord := o.placed[0]
		if dist := (100 - ord.stop) / 100 * 100; dist < 3-1e-9 {
			t.Errorf("%s: stop %.2f is %.2f%% away, the floor is 3%%", ord.symbol, ord.stop, dist)
		}
	}

	// An ATR stop that is already farther away than the floor is kept.
	s, o := policyScheduler("gradual")
	s.fetchBond = func(string) (*indicators.Result, error) { return &indicators.Result{Price: 100, ATR: 2.5}, nil }
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].stop != 92.5 {
		t.Errorf("a wider ATR stop must stay: %+v", o.placed)
	}

	// A custom floor.
	cfg := baseConfig()
	cfg.MinPolicyStopPct = 5
	s, o = reviewScheduler(cfg)
	s.fetchBond = func(string) (*indicators.Result, error) { return calmETF(), nil }
	cs = policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].stop != 95 {
		t.Errorf("MIN_POLICY_STOP_PCT=5: %+v", o.placed)
	}
}

// With no stop configured the rebalancing buy is a plain market order: still
// kind=rebalance, still bounded by the tranche.
func TestPolicyTradeWithoutAStopIsAPlainMarketOrder(t *testing.T) {
	cfg := baseConfig()
	cfg.MinPolicyStopPct = -1 // MIN_POLICY_STOP_PCT=0
	s, o := reviewScheduler(cfg)
	s.fetchBond = func(string) (*indicators.Result, error) { return calmETF(), nil }
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].kind != "market" || o.placed[0].orderKind != broker.KindRebalance || o.placed[0].stop != 0 {
		t.Fatalf("orders = %+v (%v)", o.placed, cs.whyNot)
	}
	if o.placed[0].qty != 50 {
		t.Errorf("the tranche still bounds the size: %d", o.placed[0].qty)
	}
}

// The policy tranche is exempt from the re-entry cooldown; what keeps it from a
// stop-and-rebuy loop is the stop distance (at least 3%), not a waiting period.
func TestStoppedPolicyETFMayBeBoughtBackBecauseItsStopIsWide(t *testing.T) {
	s, o := policyScheduler("gradual")
	s.fetchBond = func(string) (*indicators.Result, error) { return calmETF(), nil }
	stopped := fill("AGG", "sell", "stop", "2026-10-14 10:00", 50)
	cs := policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000}, stopped)
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].symbol != "AGG" || 100-o.placed[0].stop < 3-1e-9 {
		t.Errorf("orders = %+v", o.placed)
	}
}

// The equity floor is filled with index ETFs only, up to the index-ETF limit, then
// the next core ETF; the same tranche, caps and ordering as the bond path.
func TestEquityFloorFillsWithIndexETFsAcrossSeveralTrades(t *testing.T) {
	s, o := policyScheduler("gradual")
	// Equities 30%: SPY 10, QQQ 10, one stock 10. Floor 40: the old 10% single-name
	// limit left no ETF eligible and the sleeve stayed 10 points short.
	cs := policyCycle(s, 55_000, map[string]float64{"SPY": 10_000, "QQQ": 10_000, "AAPL": 10_000, "IEF": 15_000})
	runPolicy(s, cs)
	if len(o.placed) != 1 || o.placed[0].symbol != "SPY" || o.placed[0].qty != 50 || o.placed[0].orderKind != broker.KindRebalance {
		t.Fatalf("orders = %+v (%v)", o.placed, cs.whyNot)
	}
	if o.placed[0].stop > 97 {
		t.Errorf("an index ETF policy stop is at least 3%% away, got %.2f", o.placed[0].stop)
	}
	// Only index ETFs of the core list, never a stock, whatever the deficit.
	for _, ord := range o.placed {
		if ord.symbol == "AAPL" {
			t.Error("single stock bought for the floor")
		}
	}
}

func TestEntryBlackoutWindowsAreIndependent(t *testing.T) {
	open := et("2026-10-15", "09:30")
	closeT := et("2026-10-15", "16:00")
	for _, c := range []struct {
		at                string
		openMin, closeMin int
		want              bool
	}{
		{"09:40", 10, 10, true}, {"09:39", 10, 10, false}, {"15:49", 10, 10, true}, {"15:50", 10, 10, false},
		{"09:44", 15, 10, false}, {"09:45", 15, 10, true}, {"15:49", 15, 10, true}, {"15:50", 15, 10, false},
		{"09:31", 0, 0, true}, {"15:59", 0, 0, true},
		{"15:39", 10, 20, true}, {"15:40", 10, 20, false},
	} {
		got := market.EntryWindowOpenAsym(et("2026-10-15", c.at), open, closeT, time.Duration(c.openMin)*time.Minute, time.Duration(c.closeMin)*time.Minute)
		if got != c.want {
			t.Errorf("%s with %d/%d min blackouts: %v, want %v", c.at, c.openMin, c.closeMin, got, c.want)
		}
	}
	if !market.EntryWindowOpen(et("2026-10-15", "09:40"), open, closeT, 10*time.Minute) {
		t.Error("the symmetric helper still works")
	}
}

func TestCheckBuyBlockUsesTheConfiguredBlackouts(t *testing.T) {
	cfg := baseConfig()
	cfg.EntryBlackoutOpen, cfg.EntryBlackoutClose = 15*time.Minute, 10*time.Minute
	s, _ := newTestScheduler(cfg)
	acct := &portfolio.Account{Equity: 100_000, LastEquity: 100_000, Cash: 100_000, BuyingPower: 100_000}
	sess := session("2026-10-15", "09:40")
	sess.Open, sess.Close = et("2026-10-15", "09:30"), et("2026-10-15", "16:00")
	if reason, _ := s.checkBuyBlock(sess, acct); !strings.Contains(reason, "15m0s of the open") {
		t.Errorf("09:40 with a 15-minute blackout: %q", reason)
	}
	sess = session("2026-10-15", "09:46")
	sess.Open, sess.Close = et("2026-10-15", "09:30"), et("2026-10-15", "16:00")
	if reason, _ := s.checkBuyBlock(sess, acct); reason != "" {
		t.Errorf("09:46 should be open: %q", reason)
	}
}

func TestStopMultipleIsAtLeastTwoATRsAndAtLeastTheConfiguredMinimum(t *testing.T) {
	for _, c := range []struct{ cfg, want float64 }{{0, 2}, {1.5, 2}, {2, 2}, {2.5, 2.5}, {3, 3}} {
		if got := risk.EffectiveStopMultiple(c.cfg); got != c.want {
			t.Errorf("EffectiveStopMultiple(%v) = %v, want %v", c.cfg, got, c.want)
		}
	}
	cfg := baseConfig()
	cfg.MinStopATRMult = 3
	s, o := newTestScheduler(cfg)
	cs := newCycle()
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{buy("MSFT", 9, headline)} // price 100, ATR 2.5
	s.executeDecisions(cs)
	if ord, ok := o.order("MSFT"); !ok || ord.stop != 92.5 {
		t.Errorf("MIN_STOP_ATR_MULT=3 puts the stop 3 ATRs (7.5) below: %+v", ord)
	}
}
