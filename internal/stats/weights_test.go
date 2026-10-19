package stats

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/marketdata"
)

func TestWeightsFromHoldingsSplitsSleevesAndKeepsThemInRange(t *testing.T) {
	price := map[string]float64{"AAPL": 100, "SPY": 500, "AGG": 100, "SGOV": 100, "TLT": 90}
	w := weightsFromHoldings(100_000, map[string]float64{"AAPL": 100, "SPY": 20, "AGG": 150, "SGOV": 50}, price)
	// 10k + 10k equities, 15k bonds, the T-bill ETF and the rest are cash.
	if !near(w.Equities, 20) || !near(w.Bonds, 15) || !near(w.Cash, 65) {
		t.Errorf("weights = %+v", w)
	}
	if sum := w.Equities + w.Bonds + w.Cash; !near(sum, 100) {
		t.Errorf("weights sum to %v", sum)
	}
	// More securities than equity (stale prices): scaled, cash never negative.
	w = weightsFromHoldings(10_000, map[string]float64{"AAPL": 100, "AGG": 50}, price)
	if w.Cash < -1e-9 || !near(w.Equities+w.Bonds+w.Cash, 100) {
		t.Errorf("over-invested: %+v", w)
	}
	// A short position and a symbol without a price do not count as holdings.
	w = weightsFromHoldings(100_000, map[string]float64{"AAPL": -50, "XYZ": 10}, price)
	if !near(w.Cash, 100) {
		t.Errorf("shorts and unpriced symbols: %+v", w)
	}
	if (weightsFromHoldings(0, nil, nil) != Allocation{}) {
		t.Error("no equity, no weights")
	}
}

func TestAllocMatchedIndexAppliesTheEarlierWeightsToTheDaysReturns(t *testing.T) {
	spy := []float64{100, 110, 99}
	agg := []float64{50, 50, 51}
	bil := []float64{91, 91.1, 91.2}
	weights := []Allocation{{Equities: 50, Bonds: 30, Cash: 20}, {Equities: 0, Bonds: 0, Cash: 100}}
	got := allocMatchedIndex(weights, spy, agg, bil)
	// Day 1: 0.5*10% + 0.3*0% + 0.2*BIL; day 2 is all cash.
	d1 := 100 * (1 + 0.5*0.10 + 0.2*(91.1/91-1))
	d2 := d1 * (1 + (91.2/91.1 - 1))
	if !near(got[0], 100) || !near(got[1], d1) || !near(got[2], d2) {
		t.Errorf("index = %v, want %v %v", got, d1, d2)
	}
	// Fully in SPY it is SPY; at 60/40 it equals the 60/40 blend.
	full := allocMatchedIndex([]Allocation{{Equities: 100}, {Equities: 100}}, spy, agg, bil)
	if !near(full[2], 99) {
		t.Errorf("100%% equities should track SPY: %v", full)
	}
	mix := allocMatchedIndex([]Allocation{{Equities: 60, Bonds: 40}, {Equities: 60, Bonds: 40}}, spy, agg, bil)
	want := indexTo100(blend6040(spy, agg))
	if !near(mix[1], want[1]) || !near(mix[2], want[2]) {
		t.Errorf("60/40 weights = %v, blend = %v", mix, want)
	}
}

func allocFake() *fakeSource {
	f := historyFake()
	f.defaultWeights = Allocation{Equities: 50, Bonds: 30, Cash: 20}
	return f
}

func TestHistoryHasAnAllocationMatchedBenchmark(t *testing.T) {
	f := allocFake()
	_, rows := getHistory(t, New(f, "2026-10-13"), "")
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	// Weights 50/30/20 at every close: day 1 = 0.5*1% + 0.3*1% + 0.2*BIL.
	d1 := 100 * (1 + 0.5*0.01 + 0.3*0.01 + 0.2*(91.01/91-1))
	if got := rows[0]["alloc_matched"].(float64); !near(got, round2(d1)) {
		t.Errorf("day 1 alloc_matched = %v, want %v", got, round2(d1))
	}
	for _, r := range rows {
		if r["alloc_matched"] == nil {
			t.Errorf("missing alloc_matched: %v", r)
		}
	}
}

func TestAllocMatchedUsesTheWeightsHeldAtThePreviousClose(t *testing.T) {
	f := allocFake()
	f.weights = map[string]Allocation{
		"2026-10-12": {Equities: 100}, // baseline: all equities for day 1 (SPY +1%)
		"2026-10-13": {Cash: 100},     // day 2: all cash
		"2026-10-14": {Bonds: 100},    // day 3: all bonds
	}
	_, rows := getHistory(t, New(f, "2026-10-13"), "")
	want1 := 100 * 1.01
	want2 := want1 * (91.02 / 91.01)
	want3 := want2 * (100.0 / 100.0)
	for i, w := range []float64{want1, want2, want3} {
		if got := rows[i]["alloc_matched"].(float64); !near(got, round2(w)) {
			t.Errorf("day %d = %v, want %v", i+1, got, round2(w))
		}
	}
}

func TestAllocMatchedIsNullWhenWeightsOrBILAreUnavailable(t *testing.T) {
	f := allocFake()
	f.weightsErr = fmt.Errorf("positions down")
	_, rows := getHistory(t, New(f, "2026-10-13"), "")
	if len(rows) != 3 || rows[0]["alloc_matched"] != nil || rows[0]["spy"] == nil {
		t.Errorf("weights unavailable must only null this series: %v", rows)
	}
	g := allocFake()
	g.bil = nil
	_, rows = getHistory(t, New(g, "2026-10-13"), "")
	if len(rows) != 3 || rows[0]["alloc_matched"] != nil {
		t.Errorf("no BIL, no cash leg, no benchmark: %v", rows)
	}
}

func TestFinishedClosesAreNotRebuiltOnEveryRefresh(t *testing.T) {
	f := allocFake()
	s := New(f, "2026-10-13")
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, newYork)
	s.now = func() time.Time { return clock }
	getHistory(t, s, "")
	clock = clock.Add(cacheTTL + time.Second)
	getHistory(t, s, "")
	if f.weightCalls != 1 {
		t.Errorf("weights rebuilt %d times, want once", f.weightCalls)
	}
}

func TestBaselineDate(t *testing.T) {
	pts := []Point{{"2026-10-09", 1}, {"2026-10-12", 1}, {"2026-10-13", 1}}
	if got := baselineDate(pts, "2026-10-13"); got != "2026-10-12" {
		t.Errorf("baseline = %q", got)
	}
	if got := baselineDate(pts[2:], "2026-10-13"); got != "2026-10-13" {
		t.Errorf("no earlier point: %q", got)
	}
	if baselineDate(nil, "2026-10-13") != "" {
		t.Error("no points, no baseline")
	}
}

// The weights at a close are the current positions with every later fill undone.
func TestSleeveWeightsAreRebuiltFromPositionsAndLaterFills(t *testing.T) {
	trading := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/positions":
			w.Write([]byte(`[{"symbol":"AAPL","qty":"10"},{"symbol":"AGG","qty":"20"}]`))
		case "/v2/orders":
			w.Write([]byte(`[
			 {"id":"1","symbol":"AAPL","side":"buy","type":"market","status":"filled","filled_qty":"6","filled_avg_price":"100","filled_at":"2026-10-14T14:00:00Z","submitted_at":"2026-10-14T14:00:00Z"},
			 {"id":"2","symbol":"AGG","side":"buy","type":"market","status":"filled","filled_qty":"20","filled_avg_price":"100","filled_at":"2026-10-15T14:00:00Z","submitted_at":"2026-10-15T14:00:00Z"},
			 {"id":"3","symbol":"TSLA","side":"sell","type":"stop","status":"filled","filled_qty":"3","filled_avg_price":"100","filled_at":"2026-10-15T15:00:00Z","submitted_at":"2026-10-01T14:00:00Z"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer trading.Close()
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sym := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/stocks/"), "/")[0]
		_ = sym // every symbol closes at 100 on every day
		w.Write([]byte(`{"bars":[{"c":100,"t":"2026-10-12T04:00:00Z"},{"c":100,"t":"2026-10-13T04:00:00Z"},{"c":100,"t":"2026-10-14T04:00:00Z"},{"c":100,"t":"2026-10-15T04:00:00Z"}]}`))
	}))
	defer data.Close()
	md := marketdata.NewClient("k", "s")
	md.SetBaseURL(data.URL)
	src := AlpacaSource{Broker: broker.NewAlpacaClient("k", "s", trading.URL), MarketData: md}

	equity := []Point{{"2026-10-13", 10_000}, {"2026-10-14", 10_000}, {"2026-10-15", 10_000}}
	w, err := src.SleeveWeights(equity, []string{"2026-10-13", "2026-10-14", "2026-10-15"})
	if err != nil {
		t.Fatal(err)
	}
	// 10-13 close: AAPL 10-6 = 4 (400) and TSLA 3 shares (300, stopped out only on 10-15), no AGG yet.
	if a := w["2026-10-13"]; !near(a.Equities, 7) || !near(a.Bonds, 0) || !near(a.Cash, 93) {
		t.Errorf("10-13 = %+v", a)
	}
	// 10-14 close: AAPL 10 (1000) and TSLA 3 (300).
	if a := w["2026-10-14"]; !near(a.Equities, 13) || !near(a.Bonds, 0) {
		t.Errorf("10-14 = %+v", a)
	}
	// 10-15 close: AAPL 10, AGG 20 (2000), TSLA gone.
	if a := w["2026-10-15"]; !near(a.Equities, 10) || !near(a.Bonds, 20) || !near(a.Cash, 70) {
		t.Errorf("10-15 = %+v", a)
	}
	if _, err := src.SleeveWeights(equity, []string{"2026-10-20"}); err == nil {
		t.Error("a date without an account value must be an error, not a guess")
	}
}
