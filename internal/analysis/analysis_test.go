package analysis

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/marketdata"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/regime"
)

func near(a, b, eps float64) bool { return math.Abs(a-b) < eps }

func TestCorrBetaVol(t *testing.T) {
	a := []float64{0.01, -0.02, 0.015, 0.0, -0.01}
	if c, ok := Corr(a, a); !ok || !near(c, 1, 1e-9) {
		t.Errorf("self correlation = %v ok=%v", c, ok)
	}
	neg := make([]float64, len(a))
	for i := range a {
		neg[i] = -a[i]
	}
	if c, _ := Corr(a, neg); !near(c, -1, 1e-9) {
		t.Errorf("anti correlation = %v", c)
	}
	if _, ok := Corr(a, []float64{1, 1, 1, 1, 1}); ok {
		t.Error("constant series has no correlation")
	}
	// Beta of 2x the market is 2.
	twice := make([]float64, len(a))
	for i := range a {
		twice[i] = 2 * a[i]
	}
	if b, ok := Beta(twice, a); !ok || !near(b, 2, 1e-9) {
		t.Errorf("beta = %v", b)
	}
	// Constant daily +-1% alternation: stdev ~1.0%, annualised ~ 15.9% * sqrt(n/(n-1)).
	alt := []float64{0.01, -0.01, 0.01, -0.01, 0.01, -0.01}
	if v := AnnualVolPct(alt); !near(v, 0.01*math.Sqrt(6.0/5.0)*math.Sqrt(252)*100, 1e-6) {
		t.Errorf("vol = %v", v)
	}
}

type fakeBars struct {
	series map[string][]float64 // symbol -> closes (oldest first)
	calls  map[string]int
	err    map[string]error
}

func (f *fakeBars) DailyBars(sym, _ string) ([]marketdata.DailyBar, error) {
	f.calls[sym]++
	if e := f.err[sym]; e != nil {
		return nil, e
	}
	closes, ok := f.series[sym]
	if !ok {
		return nil, errors.New("no data")
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]marketdata.DailyBar, len(closes))
	for i, c := range closes {
		out[i] = marketdata.DailyBar{Date: base.AddDate(0, 0, i).Format("2006-01-02"), Close: c}
	}
	return out, nil
}

// walk builds closes from per-day returns.
func walk(rets []float64) []float64 {
	out := []float64{100}
	for _, r := range rets {
		out = append(out, out[len(out)-1]*(1+r))
	}
	return out
}

func rets(n int, f func(i int) float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = f(i)
	}
	return out
}

func newFake() *fakeBars {
	n := 70
	base := rets(n, func(i int) float64 { return 0.01 * math.Sin(float64(i)*0.7) })
	inverse := rets(n, func(i int) float64 { return -0.01 * math.Sin(float64(i)*0.7) })
	other := rets(n, func(i int) float64 { return 0.01 * math.Cos(float64(i)*1.9) })
	return &fakeBars{
		calls: map[string]int{}, err: map[string]error{},
		series: map[string][]float64{"SPY": walk(base), "TWIN": walk(base), "INV": walk(inverse), "OTHER": walk(other), "SHORT": walk(base[:10])},
	}
}

func TestCorrelationTo(t *testing.T) {
	a := NewAnalyzer(newFake())
	c, ok := a.CorrelationTo("TWIN", []string{"SPY", "INV"})
	if !ok || c.N != 2 || !near(c.Max, 1, 1e-6) || c.MaxSym != "SPY" || !near(c.Avg, 0, 1e-6) {
		t.Errorf("corr = %+v ok=%v (twin of SPY, mirror of INV: avg 0)", c, ok)
	}
	// The candidate is never compared with itself.
	if c, ok = a.CorrelationTo("SPY", []string{"SPY", "TWIN"}); !ok || c.N != 1 || !near(c.Avg, 1, 1e-6) {
		t.Errorf("self excluded: %+v", c)
	}
	// Too little overlap or no data -> unavailable, not a guess.
	if _, ok = a.CorrelationTo("SHORT", []string{"SPY"}); ok {
		t.Error("10 days of history must not produce a correlation")
	}
	if _, ok = a.CorrelationTo("NOPE", []string{"SPY"}); ok {
		t.Error("missing data must be unavailable")
	}
	if _, ok = a.CorrelationTo("SPY", nil); ok {
		t.Error("no holdings -> unavailable")
	}
}

func TestPortfolioRiskAndCache(t *testing.T) {
	fb := newFake()
	a := NewAnalyzer(fb)
	vol, beta, ok := a.PortfolioRisk(map[string]float64{"TWIN": 1}, 1)
	if !ok || !near(beta, 1, 1e-6) || vol <= 0 {
		t.Fatalf("vol=%v beta=%v ok=%v", vol, beta, ok)
	}
	// Half invested halves both numbers.
	vol2, beta2, _ := a.PortfolioRisk(map[string]float64{"TWIN": 1}, 0.5)
	if !near(beta2, 0.5, 1e-6) || !near(vol2, vol/2, 1e-6) {
		t.Errorf("half invested: vol=%v beta=%v", vol2, beta2)
	}
	// A hedge mix has lower vol than the parts.
	volMix, _, _ := a.PortfolioRisk(map[string]float64{"TWIN": 1, "INV": 1}, 1)
	if volMix >= vol/10 {
		t.Errorf("hedged mix vol = %v, want ~0", volMix)
	}
	if fb.calls["SPY"] != 1 || fb.calls["TWIN"] != 1 {
		t.Errorf("bars must be cached: %v", fb.calls)
	}
	if _, _, ok := a.PortfolioRisk(nil, 1); ok {
		t.Error("empty book -> unavailable")
	}
	fb.err["BAD"] = errors.New("boom")
	if _, _, ok := a.PortfolioRisk(map[string]float64{"BAD": 1}, 1); ok {
		t.Error("no data for any holding -> unavailable")
	}
}

func macroOK() regime.Regime {
	f := func(v float64) *float64 { return &v }
	return regime.Classify(indicators.Macro{
		"^IRX": {Close: f(4)}, "^FVX": {Close: f(4.2)}, "^TNX": {Close: f(4.5)}, "^TYX": {Close: f(4.8)}, "^VIX": {Close: f(27.3)},
		"SPY": {Close: f(520), MA200: f(500)}, "TLT": {Close: f(95), MA200: f(90)},
	})
}

func TestRenderPortfolioBlock(t *testing.T) {
	book := portfolio.Book{Equity: 100_000, Cash: 30_000, Holdings: []portfolio.Holding{
		{Symbol: "AAPL", Value: 10_000, Sector: "Technology"},
		{Symbol: "SPY", Value: 15_000},
		{Symbol: "IEF", Value: 15_000},
		{Symbol: "SGOV", Value: 5_000},
		{Symbol: "XOM", Value: 8_000, Sector: "Energy"},
		{Symbol: "ZZZ", Value: 3_000},
		{Symbol: "TIP", Value: 14_000},
	}}
	bot, spy, b := 1.2, 0.8, 0.5
	in := Inputs{Policy: portfolio.DefaultPolicy(), Book: book, Regime: macroOK(), Vol: 9.8, Beta: 0.62, HaveRisk: true,
		DrawdownPct: 1.2, HaveDD: true, BotReturn: &bot, SPYReturn: &spy, Bench6040: &b}
	cand := &Candidate{Symbol: "NVDA", Sector: "Technology", AddPct: 5, Corr: &Correlation{Avg: 0.55, Max: 0.81, MaxSym: "AAPL", N: 6}, Room: 4000, Bind: "single position limit 10%"}
	txt := Render(in, cand)
	for _, want := range []string{
		"## PORTFOLIO",
		"equities 36.0% (band 40-75%, target 60%)", // AAPL+SPY+XOM+ZZZ = 36k
		"bonds 29.0% (band 15-40%, target 30%)",
		"cash 35.0% (band 5-30%, target 10%)",
		"Cash: 30.0% of equity is actual cash",
		"Top positions: SPY 15.0% | IEF 15.0% | TIP 14.0% | AAPL 10.0% | XOM 8.0%",
		"Broad Market ETF 15.0%", "Unknown 3.0%", "Technology 10.0%",
		"Portfolio volatility (60d, annualized): 9.8% | beta to SPY: 0.62",
		"Drawdown from peak: 1.2%",
		"bot +1.2% | SPY +0.8% | 60/40 SPY/AGG +0.5%",
		"Macro regime: risk-off", "VIX: 27.3",
		"## CANDIDATE FIT: NVDA",
		"Average 60d correlation to current holdings: 0.55 over 6 holdings (highest: AAPL 0.81)",
		"Technology sector 10.0% -> 15.0%",
		"IPS room for this symbol: $4000 (binding: single position limit 10%)",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("block missing %q\n%s", want, txt)
		}
	}
	// Degrades: no risk numbers, no regime, no performance.
	in.HaveRisk, in.HaveDD, in.Regime, in.BotReturn, in.SPYReturn, in.Bench6040 = false, false, regime.Unavailable(), nil, nil, nil
	txt = Render(in, nil)
	for _, want := range []string{"volatility / beta: unavailable", "Macro regime: unavailable", "bot n/a | SPY n/a | 60/40 SPY/AGG n/a"} {
		if !strings.Contains(txt, want) {
			t.Errorf("degraded block missing %q\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "CANDIDATE FIT") {
		t.Error("review block has no candidate section")
	}
}
