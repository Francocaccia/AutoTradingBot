package stats

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fillAt is a filled order on an ET date.
func fillAt(sym, side, typ, date string, qty, price float64) Order {
	at, err := time.ParseInLocation("2006-01-02 15:04", date+" 11:00", newYork)
	if err != nil {
		panic(err)
	}
	return Order{Symbol: sym, Side: side, Type: typ, Qty: qty, Price: price, FilledAt: at}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return m
}

func TestClassifyOrders(t *testing.T) {
	for _, c := range []struct {
		side, typ string
		want      Kind
	}{
		{"buy", "market", KindEntry},
		{"buy", "limit", KindEntry},
		{"sell", "stop", KindStop},
		{"sell", "stop_limit", KindStop},
		{"sell", "trailing_stop", KindStop},
		{"sell", "limit", KindTakeProfit},
		{"sell", "market", KindExit},
	} {
		if got := (Order{Side: c.side, Type: c.typ}).Classify(); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.side, c.typ, got, c.want)
		}
	}
}

// TestNumTradesAreEntriesOnly replays the 14 fills of the first tracked week:
// 7 entries, 7 exits (3 stops, 4 discretionary sells), 3 closed round trips,
// all of them losses.
func TestNumTradesAreEntriesOnly(t *testing.T) {
	orders := []Order{
		{Symbol: "HDV", Side: "sell", Type: "market", Qty: 8, Price: 29, FilledAt: at(t, "2026-10-13")},
		fillAt("V", "sell", "market", "2026-10-13", 1, 390),
		fillAt("VRT", "sell", "market", "2026-10-13", 12, 233),
		fillAt("SPY", "buy", "market", "2026-10-13", 6, 776),
		fillAt("SMCI", "buy", "market", "2026-10-13", 132, 38),
		fillAt("CIFR", "buy", "market", "2026-10-13", 644, 11),
		fillAt("URA", "buy", "market", "2026-10-13", 192, 37),
		fillAt("URA", "sell", "stop", "2026-10-15", 192, 34),
		fillAt("SMCI", "sell", "stop", "2026-10-15", 132, 33),
		fillAt("LRCX", "sell", "market", "2026-10-15", 8, 271),
		fillAt("SPY", "sell", "stop", "2026-10-15", 6, 765),
		fillAt("CCC", "buy", "market", "2026-10-15", 653, 7),
		fillAt("NVDA", "buy", "market", "2026-10-16", 32, 216),
		fillAt("MSFT", "buy", "market", "2026-10-16", 9, 536),
		fillAt("OLD", "buy", "market", "2026-10-12", 1, 1), // before the start date: not counted
	}
	c := countOrders(orders, "2026-10-13")
	if c.entries != 7 || c.exits() != 7 || c.stops != 3 || c.takeProfits != 0 || c.other != 4 {
		t.Errorf("counts = %+v", c)
	}
	if wins, total := roundTrips(sinceStart(orders, "2026-10-13")); wins != 0 || total != 3 {
		t.Errorf("wins/total = %d/%d, want 0/3", wins, total)
	}

	f := newFake()
	f.orders = orders
	m := decode(t, serve(New(f, "2026-10-13"), http.MethodGet))
	if m["num_trades"].(float64) != 7 || m["exits_count"].(float64) != 7 || m["closed_round_trips"].(float64) != 3 || m["win_rate_pct"].(float64) != 0 {
		t.Errorf("trade counters = %v %v %v %v", m["num_trades"], m["exits_count"], m["closed_round_trips"], m["win_rate_pct"])
	}
	eb := m["exits_breakdown"].(map[string]any)
	if eb["stop_loss"].(float64) != 3 || eb["take_profit"].(float64) != 0 || eb["discretionary"].(float64) != 4 {
		t.Errorf("exits_breakdown = %v", eb)
	}
}

func at(t *testing.T, date string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04", date+" 11:00", newYork)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestWinRateIsNullUntilARoundTripCloses(t *testing.T) {
	f := newFake()
	f.orders = []Order{fillAt("AAA", "buy", "market", "2026-10-13", 10, 5)} // still open
	m := decode(t, serve(New(f, "2026-10-13"), http.MethodGet))
	if v, ok := m["win_rate_pct"]; !ok || v != nil {
		t.Errorf("win_rate_pct = %v (present %v), want null", v, ok)
	}
	if m["closed_round_trips"].(float64) != 0 || m["num_trades"].(float64) != 1 || m["exits_count"].(float64) != 0 {
		t.Errorf("counters = %v", m)
	}

	f.orders = append(f.orders,
		fillAt("AAA", "sell", "limit", "2026-10-14", 10, 6), // take-profit: win
		fillAt("BBB", "buy", "market", "2026-10-13", 10, 5),
		fillAt("BBB", "sell", "stop", "2026-10-14", 10, 4), // stop: loss
	)
	m = decode(t, serve(New(f, "2026-10-13"), http.MethodGet))
	if m["win_rate_pct"].(float64) != 50 || m["closed_round_trips"].(float64) != 2 {
		t.Errorf("win rate = %v of %v", m["win_rate_pct"], m["closed_round_trips"])
	}
	if eb := m["exits_breakdown"].(map[string]any); eb["take_profit"].(float64) != 1 || eb["stop_loss"].(float64) != 1 {
		t.Errorf("breakdown = %v", eb)
	}
}

func TestDefinitionsDocumentTheCounters(t *testing.T) {
	m := decode(t, serve(New(newFake(), "2026-10-13"), http.MethodGet))
	defs, ok := m["definitions"].(map[string]any)
	if !ok {
		t.Fatalf("definitions = %v", m["definitions"])
	}
	for _, k := range []string{"num_trades", "exits_count", "win_rate_pct", "closed_round_trips", "beta_vs_spy", "volatility_ann_pct", "tracking_error_vs_spy_pct"} {
		if s, _ := defs[k].(string); len(s) < 20 {
			t.Errorf("definition of %s = %q", k, s)
		}
	}
	// Insufficient data still carries the definitions.
	f := newFake()
	f.equity, f.spy, f.agg = f.equity[:2], f.spy[:2], f.agg[:2]
	if m := decode(t, serve(New(f, "2026-10-13"), http.MethodGet)); m["definitions"] == nil {
		t.Error("definitions missing on the insufficient-data response")
	}
}

// riskSource builds n tracked days (plus the baseline) where the bot's daily
// return is exactly mult times SPY's, so beta = mult and correlation = 1.
func riskSource(n int, mult float64) *fakeSource {
	f := newFake()
	f.equity, f.spy, f.agg, f.qqq, f.bil, f.orders = nil, nil, nil, nil, nil, nil
	base := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	bot, spy, agg := 100000.0, 500.0, 100.0
	for i := 0; i <= n; i++ {
		d := base.AddDate(0, 0, i).Format("2006-01-02")
		if i > 0 {
			r := 0.004 * math.Sin(float64(i)*1.7) // deterministic wiggle, both signs
			spy *= 1 + r
			bot *= 1 + mult*r
			agg *= 1 + 0.0003
		}
		f.equity = append(f.equity, Point{d, bot})
		f.spy = append(f.spy, Point{d, spy})
		f.agg = append(f.agg, Point{d, agg})
	}
	return f
}

func TestRiskStatsAreNullUntilTheirMinimumSample(t *testing.T) {
	for _, c := range []struct {
		days               int
		vol, te, beta, cor bool
	}{
		{5, false, false, false, false},
		{19, false, false, false, false},
		{20, true, true, false, false},
		{29, true, true, false, false},
		{30, true, true, true, true},
	} {
		m := decode(t, serve(New(riskSource(c.days, 2), "2026-10-13"), http.MethodGet))
		if int(m["observations"].(float64)) != c.days {
			t.Errorf("%d days: observations = %v", c.days, m["observations"])
		}
		for k, want := range map[string]bool{
			"volatility_ann_pct": c.vol, "tracking_error_vs_spy_pct": c.te, "beta_vs_spy": c.beta, "correlation_vs_spy": c.cor,
		} {
			v, present := m[k]
			if !present {
				t.Errorf("%d days: %s missing from the payload", c.days, k)
			}
			if (v != nil) != want {
				t.Errorf("%d days: %s = %v, populated=%v want %v", c.days, k, v, v != nil, want)
			}
		}
	}
}

func TestBetaCorrelationVolAndTrackingError(t *testing.T) {
	m := decode(t, serve(New(riskSource(40, 2), "2026-10-13"), http.MethodGet))
	if b := m["beta_vs_spy"].(float64); math.Abs(b-2) > 0.01 {
		t.Errorf("beta = %v, want 2", b)
	}
	if c := m["correlation_vs_spy"].(float64); math.Abs(c-1) > 0.01 {
		t.Errorf("correlation = %v, want 1", c)
	}
	vol, te := m["volatility_ann_pct"].(float64), m["tracking_error_vs_spy_pct"].(float64)
	// The bot moves twice as much as SPY, so the active return equals SPY's own
	// return: tracking error = SPY volatility = half the bot's.
	if vol <= 0 || math.Abs(te-vol/2) > 0.05 {
		t.Errorf("vol = %v, tracking error = %v (want vol/2)", vol, te)
	}

	// A flat bot has no variance: beta and correlation stay null rather than fabricated.
	f := riskSource(40, 0)
	m = decode(t, serve(New(f, "2026-10-13"), http.MethodGet))
	if m["beta_vs_spy"] != nil || m["correlation_vs_spy"] != nil {
		t.Errorf("flat bot: beta/correlation = %v/%v, want null", m["beta_vs_spy"], m["correlation_vs_spy"])
	}
}

func TestRiskAdjustedStillWaitsForTwentyDays(t *testing.T) {
	if m := decode(t, serve(New(riskSource(19, 1), "2026-10-13"), http.MethodGet)); m["risk_adjusted"] != "insufficient data" {
		t.Errorf("19 days: %v", m["risk_adjusted"])
	}
	if m := decode(t, serve(New(riskSource(20, 1), "2026-10-13"), http.MethodGet)); m["risk_adjusted"] == "insufficient data" {
		t.Errorf("20 days: %v", m["risk_adjusted"])
	}
}

func TestPolicyBandsArePublishedAsPercentages(t *testing.T) {
	s := New(newFake(), "2026-10-13")
	s.SetBands(Bands{Equities: Band{40, 75}, Bonds: Band{15, 40}, Cash: Band{5, 30}})
	m := decode(t, serve(s, http.MethodGet))
	b := m["policy_bands_pct"].(map[string]any)
	if eq := b["equities"].(map[string]any); eq["min"].(float64) != 40 || eq["max"].(float64) != 75 {
		t.Errorf("bands = %v", b)
	}
}

func TestStatsMathHelpers(t *testing.T) {
	if got := stdev([]float64{2, 4, 4, 4, 5, 5, 7, 9}); math.Abs(got-2.13809) > 1e-4 {
		t.Errorf("stdev = %v", got)
	}
	if stdev([]float64{1}) != 0 || stdev(nil) != 0 {
		t.Error("stdev of fewer than two values is 0")
	}
	if _, _, ok := betaCorr([]float64{1, 1, 1}, []float64{1, 2, 3}); ok {
		t.Error("a constant series has no beta")
	}
	if _, _, ok := betaCorr([]float64{1, 2}, []float64{1}); ok {
		t.Error("mismatched lengths have no beta")
	}
	if idx := indexTo100([]float64{200, 210, 190}); !near(idx[0], 100) || !near(idx[1], 105) || !near(idx[2], 95) {
		t.Errorf("index = %v", idx)
	}
	if indexTo100([]float64{0, 1}) != nil || indexTo100(nil) != nil {
		t.Error("no positive base, no index")
	}
}

// ---- /stats/history ----

func ts(date, hhmm string) time.Time {
	tm, err := time.ParseInLocation("2006-01-02 15:04", date+" "+hhmm, newYork)
	if err != nil {
		panic(err)
	}
	return tm
}

func historyFake() *fakeSource {
	f := newFake()
	// Baseline 10-12, tracked 10-13 .. 10-15.
	f.equity = []Point{{"2026-10-12", 100000}, {"2026-10-13", 101000}, {"2026-10-14", 99000}, {"2026-10-15", 100500}}
	f.spy = []Point{{"2026-10-12", 500}, {"2026-10-13", 505}, {"2026-10-14", 495}, {"2026-10-15", 500}}
	f.agg = []Point{{"2026-10-12", 100}, {"2026-10-13", 101}, {"2026-10-14", 100}, {"2026-10-15", 100}}
	f.qqq = []Point{{"2026-10-12", 400}, {"2026-10-13", 404}, {"2026-10-14", 396}, {"2026-10-15", 400}}
	f.bil = []Point{{"2026-10-12", 91}, {"2026-10-13", 91.01}, {"2026-10-14", 91.02}, {"2026-10-15", 91.03}}
	f.intraday = []Sample{
		{ts("2026-10-13", "09:30"), 100200}, {ts("2026-10-13", "12:00"), 102000}, {ts("2026-10-13", "15:59"), 101000},
		{ts("2026-10-14", "09:30"), 100900}, {ts("2026-10-14", "11:00"), 98000}, {ts("2026-10-14", "15:59"), 99000},
		{ts("2026-10-15", "09:30"), 99100}, {ts("2026-10-15", "15:59"), 100500},
	}
	return f
}

func getHistory(t *testing.T, s *Service, origin string) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/stats/history", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	s.HistoryHandler().ServeHTTP(rec, req)
	var rows []map[string]any
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatalf("%v: %s", err, rec.Body)
		}
	}
	return rec, rows
}

func TestHistoryIsIndexedToBaselineAndAlignedAcrossBenchmarks(t *testing.T) {
	rec, rows := getHistory(t, New(historyFake(), "2026-10-13"), "")
	if rec.Code != 200 || len(rows) != 3 {
		t.Fatalf("status %d rows %d: %s", rec.Code, len(rows), rec.Body)
	}
	if rows[0]["date"] != "2026-10-13" || rows[2]["date"] != "2026-10-15" {
		t.Errorf("dates = %v .. %v", rows[0]["date"], rows[2]["date"])
	}
	// Day 1: close 101000/100000 = 101, intraday low 100.2 (the open), high 102.
	d1 := rows[0]
	for k, want := range map[string]float64{"o": 100.2, "h": 102, "l": 100.2, "c": 101, "spy": 101, "qqq": 101, "agg": 101} {
		if got := d1[k].(float64); !near(got, want) {
			t.Errorf("day 1 %s = %v, want %v", k, got, want)
		}
	}
	// Day 2: low 98, close 99, SPY 495/500.
	d2 := rows[1]
	if !near(d2["l"].(float64), 98) || !near(d2["c"].(float64), 99) || !near(d2["spy"].(float64), 99) || !near(d2["h"].(float64), 100.9) {
		t.Errorf("day 2 = %v", d2)
	}
	// 60/40 daily rebalanced: day 1 = +0.6*1% + 0.4*1% = +1%.
	if !near(d1["mix_60_40"].(float64), 101) {
		t.Errorf("mix day 1 = %v", d1["mix_60_40"])
	}
	// Cash proxy BIL: 91.01/91.
	if !near(d1["cash"].(float64), 100.01) {
		t.Errorf("cash day 1 = %v", d1["cash"])
	}
	// The last close equals the /stats bot return, so the chart and the numbers agree.
	st := decode(t, serve(New(historyFake(), "2026-10-13"), http.MethodGet))
	if last := rows[2]["c"].(float64); !near(last-100, st["bot_return_pct"].(float64)) {
		t.Errorf("last close %v does not match bot_return_pct %v", last, st["bot_return_pct"])
	}
	if spyLast := rows[2]["spy"].(float64); !near(spyLast-100, st["spy_return_pct"].(float64)) {
		t.Errorf("last spy %v does not match spy_return_pct %v", spyLast, st["spy_return_pct"])
	}
	for _, r := range rows {
		o, h, l, c := r["o"].(float64), r["h"].(float64), r["l"].(float64), r["c"].(float64)
		if h < o || h < c || l > o || l > c {
			t.Errorf("inconsistent candle %v", r)
		}
	}
}

func TestHistoryIsEmptyUntilTwoTradingDays(t *testing.T) {
	f := historyFake()
	f.equity, f.spy, f.agg, f.qqq, f.bil = f.equity[:2], f.spy[:2], f.agg[:2], f.qqq[:2], f.bil[:2]
	rec, rows := getHistory(t, New(f, "2026-10-13"), "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" || len(rows) != 0 {
		t.Errorf("one tracked day: %d %q", rec.Code, rec.Body)
	}
}

func TestHistoryFallsBackToCloseOnlyWithoutIntraday(t *testing.T) {
	f := historyFake()
	f.intraday, f.intradayErr = nil, http.ErrHandlerTimeout
	rec, rows := getHistory(t, New(f, "2026-10-13"), "")
	if rec.Code != 200 || len(rows) != 3 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	for _, r := range rows {
		if r["o"] != r["h"] || r["h"] != r["l"] || r["l"] != r["c"] {
			t.Errorf("close-only row must have o=h=l=c: %v", r)
		}
	}
}

func TestHistoryBenchmarksThatCannotLoadAreNullNotFabricated(t *testing.T) {
	f := historyFake()
	f.qqq, f.bil = nil, nil
	_, rows := getHistory(t, New(f, "2026-10-13"), "")
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	for _, r := range rows {
		if r["qqq"] != nil || r["cash"] != nil || r["spy"] == nil || r["mix_60_40"] == nil {
			t.Errorf("row = %v", r)
		}
	}
}

func TestHistoryDropsDatesAnyBenchmarkIsMissing(t *testing.T) {
	f := historyFake()
	f.qqq = []Point{{"2026-10-12", 400}, {"2026-10-13", 404}, {"2026-10-15", 400}} // no 10-14
	_, rows := getHistory(t, New(f, "2026-10-13"), "")
	if len(rows) != 2 || rows[0]["date"] != "2026-10-13" || rows[1]["date"] != "2026-10-15" {
		t.Errorf("rows must share one set of dates: %v", rows)
	}
}

func TestHistoryIsCachedAndFinishedSessionsAreKept(t *testing.T) {
	f := historyFake()
	s := New(f, "2026-10-13")
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, newYork) // after all three sessions
	s.now = func() time.Time { return clock }
	getHistory(t, s, "")
	getHistory(t, s, "")
	if f.intradayCalls != 1 {
		t.Errorf("intraday calls inside the TTL = %d, want 1", f.intradayCalls)
	}
	clock = clock.Add(cacheTTL + time.Second)
	getHistory(t, s, "")
	if f.intradayCalls != 1 {
		t.Errorf("finished sessions must not be fetched again, calls = %d", f.intradayCalls)
	}
	if f.calls < 2 {
		t.Errorf("daily history should refresh after the TTL, calls = %d", f.calls)
	}

	// Today's session is asked for again on every refresh.
	g := historyFake()
	s2 := New(g, "2026-10-13")
	clock2 := time.Date(2026, 10, 15, 11, 0, 0, 0, newYork)
	s2.now = func() time.Time { return clock2 }
	getHistory(t, s2, "")
	clock2 = clock2.Add(cacheTTL + time.Second)
	getHistory(t, s2, "")
	if g.intradayCalls != 2 || g.intradayRange[0] != "2026-10-15" {
		t.Errorf("calls = %d range = %v, want today only on the refresh", g.intradayCalls, g.intradayRange)
	}
}

func TestHistoryUpstreamErrorIsGeneric(t *testing.T) {
	f := &fakeSource{err: http.ErrAbortHandler}
	rec, _ := getHistory(t, New(f, "2026-10-13"), "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "history unavailable") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestHistoryCORS(t *testing.T) {
	s := New(historyFake(), "2026-10-13")
	s.SetOriginPolicy(OriginPolicy("https://bot.example.org/"))
	for _, c := range []struct {
		origin string
		allow  bool
	}{
		{"https://francocaccia.github.io", true},
		{"https://bot.example.org", true},
		{"http://localhost:3000", true},
		{"http://127.0.0.1:8080", true},
		{"http://[::1]:5173", true},
		{"", false},
		{"https://evil.example.com", false},
		{"https://francocaccia.github.io.evil.com", false},
		{"http://francocaccia.github.io", false},
		{"https://other.github.io", false},
		{"https://localhost.evil.com", false},
		{"null", false},
	} {
		rec, _ := getHistory(t, s, c.origin)
		got := rec.Header().Get("Access-Control-Allow-Origin")
		if c.allow && got != c.origin {
			t.Errorf("origin %q: Access-Control-Allow-Origin = %q, want it echoed", c.origin, got)
		}
		if !c.allow && got != "" {
			t.Errorf("origin %q must not be allowed, got %q", c.origin, got)
		}
		if rec.Header().Get("Vary") == "" {
			t.Errorf("origin %q: missing Vary header", c.origin)
		}
	}
	// Without CORS_EXTRA_ORIGIN the custom domain is not allowed.
	d := New(historyFake(), "2026-10-13")
	if rec, _ := getHistory(t, d, "https://bot.example.org"); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("extra origin allowed without CORS_EXTRA_ORIGIN")
	}

	pre := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/stats/history", nil)
	req.Header.Set("Origin", "https://francocaccia.github.io")
	s.HistoryHandler().ServeHTTP(pre, req)
	if pre.Code != http.StatusNoContent || pre.Header().Get("Access-Control-Allow-Origin") != "https://francocaccia.github.io" {
		t.Errorf("preflight = %d %v", pre.Code, pre.Header())
	}
	post := httptest.NewRecorder()
	s.HistoryHandler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/stats/history", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", post.Code)
	}
}

func TestHistoryExposesOnlyIndexedValues(t *testing.T) {
	rec, rows := getHistory(t, New(historyFake(), "2026-10-13"), "")
	allowed := map[string]bool{"date": true, "o": true, "h": true, "l": true, "c": true, "spy": true, "qqq": true, "agg": true, "mix_60_40": true, "cash": true, "alloc_matched": true}
	for _, r := range rows {
		for k, v := range r {
			if !allowed[k] {
				t.Errorf("unexpected field %q", k)
			}
			if f, ok := v.(float64); ok && k != "date" && (f < 50 || f > 150) {
				t.Errorf("%s = %v is not an index near 100 (a dollar amount?)", k, f)
			}
		}
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"100000", "101000", "99000", "100500", "ZZTOP", "symbol", "qty"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("history contains %q: %s", forbidden, body)
		}
	}
}

func TestRangesFromGroupsBySessionDate(t *testing.T) {
	got := rangesFrom([]Sample{
		{ts("2026-10-13", "15:59"), 101}, {ts("2026-10-13", "09:30"), 100}, {ts("2026-10-13", "12:00"), 103},
		{ts("2026-10-14", "09:30"), 0}, // ignored: no equity reading
		{ts("2026-10-14", "10:00"), 99},
	})
	d1, d2 := got["2026-10-13"], got["2026-10-14"]
	if d1.o != 100 || d1.h != 103 || d1.l != 100 {
		t.Errorf("day 1 = %+v (the open is the earliest sample, not the first listed)", d1)
	}
	if !d2.ok || d2.o != 99 {
		t.Errorf("day 2 = %+v", d2)
	}
}
