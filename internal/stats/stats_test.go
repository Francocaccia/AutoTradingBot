package stats

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestReturnAndDrawdownMath(t *testing.T) {
	series := []float64{100, 110, 99, 105, 120, 90}
	if got := pctChange(series[len(series)-1], series[0]); !near(got, -10) {
		t.Errorf("return = %v, want -10", got)
	}
	// Peak 120 -> trough 90 is -25%; the earlier 110 -> 99 dip is only -10%.
	if got := maxDrawdown(series); !near(got, -25) {
		t.Errorf("max drawdown = %v, want -25", got)
	}
	if got := maxDrawdown([]float64{100, 101, 102}); got != 0 {
		t.Errorf("monotonic drawdown = %v, want 0", got)
	}
	worst, best := minMax(dailyReturns(series))
	if !near(best, 120.0/105*100-100) || !near(worst, 90.0/120*100-100) {
		t.Errorf("best/worst = %v/%v", best, worst)
	}
	if pctChange(5, 0) != 0 {
		t.Error("zero base must not divide")
	}
}

func TestRoundTrips(t *testing.T) {
	orders := []Order{
		{Symbol: "A", Side: "sell", Qty: 5, Price: 1}, // no entry tracked: ignored
		{Symbol: "A", Side: "buy", Qty: 10, Price: 10},
		{Symbol: "B", Side: "buy", Qty: 10, Price: 10},
		{Symbol: "A", Side: "sell", Qty: 10, Price: 12}, // win
		{Symbol: "B", Side: "sell", Qty: 4, Price: 8},
		{Symbol: "B", Side: "sell", Qty: 6, Price: 9}, // loss overall
		{Symbol: "C", Side: "buy", Qty: 1, Price: 5},  // still open
	}
	if wins, total := roundTrips(orders); wins != 1 || total != 2 {
		t.Errorf("wins/total = %d/%d, want 1/2", wins, total)
	}
}

func TestAlignUsesPriorCloseAsBaseline(t *testing.T) {
	bot := []Point{{"2026-10-09", 100}, {"2026-10-13", 102}, {"2026-10-14", 101}}
	spy := []Point{{"2026-10-09", 500}, {"2026-10-13", 505}, {"2026-10-14", 510}}
	b, s, dates := align(bot, spy, "2026-10-13")
	if len(dates) != 2 || b[0] != 100 || s[0] != 500 || b[2] != 101 || s[2] != 510 {
		t.Errorf("align = %v %v %v", b, s, dates)
	}
}

type fakeSource struct {
	equity, spy, agg []Point
	orders           []Order
	open             int
	alloc            Allocation
	allocErr         error
	err              error
	calls            int
}

func (f *fakeSource) EquityHistory(int) ([]Point, error) { f.calls++; return f.equity, f.err }
func (f *fakeSource) Closes(symbol, _ string) ([]Point, error) {
	if symbol == "AGG" {
		return f.agg, f.err
	}
	return f.spy, f.err
}
func (f *fakeSource) Allocation() (Allocation, error)      { return f.alloc, f.allocErr }
func (f *fakeSource) FilledOrders(string) ([]Order, error) { return f.orders, f.err }
func (f *fakeSource) OpenPositions() (int, error)          { return f.open, f.err }

func newFake() *fakeSource {
	return &fakeSource{
		equity: []Point{{"2026-10-12", 123456.78}, {"2026-10-13", 124000}, {"2026-10-14", 122000}},
		spy:    []Point{{"2026-10-12", 500}, {"2026-10-13", 505}, {"2026-10-14", 510}},
		agg:    []Point{{"2026-10-12", 100}, {"2026-10-13", 101}, {"2026-10-14", 100}},
		alloc:  Allocation{Equities: 55.555, Bonds: 25, Cash: 19.445},
		orders: []Order{
			{Symbol: "ZZTOP", Side: "buy", Qty: 777, Price: 31.37, FilledAt: time.Now()},
			{Symbol: "ZZTOP", Side: "sell", Qty: 777, Price: 40.11, FilledAt: time.Now()},
		},
		open: 3,
	}
}

func serve(s *Service, method string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/stats", nil))
	return rec
}

func TestResponseHasOnlyAllowedFieldsAndNoPrivateValues(t *testing.T) {
	s := New(newFake(), "2026-10-13")
	rec := serve(s, http.MethodGet)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	allowed := map[string]bool{
		"start_date": true, "days_tracked": true, "bot_return_pct": true, "spy_return_pct": true,
		"excess_return_pct": true, "max_drawdown_pct": true, "best_day_pct": true, "worst_day_pct": true,
		"num_trades": true, "win_rate_pct": true, "open_positions_count": true, "last_updated": true,
		"benchmark_60_40_return_pct": true, "bot_vs_60_40_pct": true, "max_drawdown_spy_pct": true,
		"max_drawdown_60_40_pct": true, "risk_adjusted": true, "allocation_pct": true,
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for k := range m {
		if !allowed[k] {
			t.Errorf("unexpected field %q", k)
		}
	}
	for k := range allowed {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q", k)
		}
	}
	for _, forbidden := range []string{"equity\"", "cash_balance", "buying_power", "account", "symbol", "qty", "price", "ZZTOP", "123456", "124000", "777", "31.37"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("response contains forbidden %q: %s", forbidden, rec.Body)
		}
	}
	// 100%-baseline checks: bot 123456.78 -> 122000 = -1.18%, SPY 500 -> 510 = +2%.
	if !near(m["bot_return_pct"].(float64), -1.18) || !near(m["spy_return_pct"].(float64), 2) || m["num_trades"].(float64) != 2 || m["win_rate_pct"].(float64) != 100 {
		t.Errorf("values = %v", m)
	}
}

func TestInsufficientDataReturnsNulls(t *testing.T) {
	f := newFake()
	f.equity, f.spy, f.agg = f.equity[:2], f.spy[:2], f.agg[:2] // only one tracked day
	rec := serve(New(f, "2026-10-13"), http.MethodGet)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m["note"] != "insufficient data" || m["bot_return_pct"] != nil || m["num_trades"] != nil || m["open_positions_count"] != nil {
		t.Errorf("got %v", m)
	}
	if _, ok := m["win_rate_pct"]; !ok {
		t.Error("shape must be stable")
	}
}

func TestMethodAndErrorHandling(t *testing.T) {
	s := New(newFake(), "2026-10-13")
	if rec := serve(s, http.MethodPost); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Errorf("POST = %d", rec.Code)
	}
	f := &fakeSource{err: errors.New("alpaca error 403: secret-upstream-body")}
	rec := serve(New(f, "2026-10-13"), http.MethodGet)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "secret") || !strings.Contains(rec.Body.String(), "stats unavailable") {
		t.Errorf("error response = %d %s", rec.Code, rec.Body)
	}
}

func TestCache(t *testing.T) {
	f := newFake()
	s := New(f, "2026-10-13")
	clock := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	s.Get()
	s.Get()
	if f.calls != 1 {
		t.Errorf("upstream calls within TTL = %d, want 1", f.calls)
	}
	clock = clock.Add(cacheTTL + time.Second)
	s.Get()
	if f.calls != 2 {
		t.Errorf("upstream calls after TTL = %d, want 2", f.calls)
	}
}

func TestObservePeakOnlyRisesAndSeedsFromHistory(t *testing.T) {
	s := New(newFake(), "2026-10-13")
	// History since the start peaks at 124000; the pre-start 123456.78 is ignored.
	if got := s.ObservePeak(100); got != 124000 {
		t.Errorf("seeded peak = %v", got)
	}
	if got := s.ObservePeak(130000); got != 130000 {
		t.Errorf("raised peak = %v", got)
	}
	if got := s.ObservePeak(1); got != 130000 {
		t.Errorf("peak fell to %v", got)
	}
}

func TestBlend6040AndRatio(t *testing.T) {
	spy := []float64{100, 110, 99}
	agg := []float64{50, 50, 51}
	mix := blend6040(spy, agg)
	// day 1: 0.6*10% + 0.4*0% = +6%; day 2: 0.6*-10% + 0.4*2% = -5.2%
	if !near(mix[0], 100) || !near(mix[1], 106) || !near(mix[2], 106*(1-0.052)) {
		t.Errorf("mix = %v", mix)
	}
	if ratio([]float64{100, 100, 100}) != nil || ratio([]float64{100}) != nil {
		t.Error("flat or short series have no ratio")
	}
	up := []float64{100, 101, 101.5, 103, 103.2}
	if r := ratio(up); r == nil || *r <= 0 {
		t.Errorf("rising series ratio = %v", r)
	}
}

func TestSixtyFortyAllocationAndRatioGate(t *testing.T) {
	f := newFake()
	rec := serve(New(f, "2026-10-13"), http.MethodGet)
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	// 60/40: day 1 (10-13) SPY +1%, AGG +1% -> +1%; day 2 SPY +0.99%, AGG -0.99% -> ~+0.2%.
	if v, ok := m["benchmark_60_40_return_pct"].(float64); !ok || v < 1.1 || v > 1.3 {
		t.Errorf("60/40 return = %v", m["benchmark_60_40_return_pct"])
	}
	if m["bot_vs_60_40_pct"] == nil || m["max_drawdown_spy_pct"] == nil || m["max_drawdown_60_40_pct"] == nil {
		t.Errorf("missing benchmark fields: %v", m)
	}
	if m["risk_adjusted"] != "insufficient data" {
		t.Errorf("2 days must not produce a ratio: %v", m["risk_adjusted"])
	}
	alloc, _ := m["allocation_pct"].(map[string]any)
	if alloc["equities"].(float64) != 55.56 || alloc["bonds"].(float64) != 25 || alloc["cash"].(float64) != 19.45 {
		t.Errorf("allocation = %v", alloc)
	}
	// Only percentages: nothing else in the allocation object.
	if len(alloc) != 3 {
		t.Errorf("allocation must carry sleeve percentages only: %v", alloc)
	}

	// 25 tracked days: ratios appear.
	g := newFake()
	g.equity, g.spy, g.agg = nil, nil, nil
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 26; i++ {
		d := base.AddDate(0, 0, i).Format("2006-01-02")
		wiggle := float64(i%3) * 0.2
		g.equity = append(g.equity, Point{d, 100000 + float64(i)*100 + wiggle*100})
		g.spy = append(g.spy, Point{d, 500 + float64(i) + wiggle})
		g.agg = append(g.agg, Point{d, 100 + float64(i)*0.05 - wiggle*0.1})
	}
	rec = serve(New(g, "2026-10-13"), http.MethodGet)
	m = nil
	json.Unmarshal(rec.Body.Bytes(), &m)
	ra, ok := m["risk_adjusted"].(map[string]any)
	if !ok || ra["bot"] == nil || ra["spy"] == nil || ra["benchmark_60_40"] == nil {
		t.Errorf("risk_adjusted with 25 days = %v", m["risk_adjusted"])
	}
}

func TestAllocationFailureDoesNotBreakStats(t *testing.T) {
	f := newFake()
	f.allocErr = errors.New("positions: secret-body")
	rec := serve(New(f, "2026-10-13"), http.MethodGet)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m["allocation_pct"] != nil || m["bot_return_pct"] == nil {
		t.Errorf("got %v", m)
	}
}

func TestPerformanceAccessor(t *testing.T) {
	bot, spy, mix := New(newFake(), "2026-10-13").Performance()
	if bot == nil || spy == nil || mix == nil {
		t.Fatalf("performance = %v %v %v", bot, spy, mix)
	}
	if b, s2, m := New(&fakeSource{err: errors.New("x")}, "2026-10-13").Performance(); b != nil || s2 != nil || m != nil {
		t.Error("errors must yield nils")
	}
}
