// Package analysis computes the portfolio-level numbers the model is shown
// (volatility, beta to SPY, correlation to current holdings) from daily
// closes, and renders the PORTFOLIO block of the prompt. All arithmetic is
// done here in Go; the LLM only reads the results.
package analysis

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/francocaccia/quantengine/internal/marketdata"
)

const (
	// Window is the number of daily returns used for volatility, beta and correlation.
	Window = 60
	// MinOverlap is the fewest common return days needed for a statistic.
	MinOverlap = 30

	barsTTL    = 6 * time.Hour
	failureTTL = 10 * time.Minute
	lookback   = 130 * 24 * time.Hour // enough calendar days for 60 trading-day returns
)

// BarSource supplies daily closes (marketdata.Client in production).
type BarSource interface {
	DailyBars(symbol, start string) ([]marketdata.DailyBar, error)
}

type entry struct {
	rets map[string]float64 // date -> simple daily return
	err  error
	at   time.Time
}

// Analyzer fetches and caches daily returns per symbol.
type Analyzer struct {
	src BarSource
	now func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

func NewAnalyzer(src BarSource) *Analyzer {
	return &Analyzer{src: src, now: time.Now, cache: map[string]entry{}}
}

// Returns gives date -> daily return for symbol, cached for six hours.
func (a *Analyzer) Returns(symbol string) (map[string]float64, error) {
	a.mu.Lock()
	e, ok := a.cache[symbol]
	a.mu.Unlock()
	ttl := barsTTL
	if ok && e.err != nil {
		ttl = failureTTL
	}
	if ok && a.now().Sub(e.at) < ttl {
		return e.rets, e.err
	}
	bars, err := a.src.DailyBars(symbol, a.now().Add(-lookback).Format("2006-01-02"))
	e = entry{at: a.now(), err: err}
	if err == nil {
		e.rets = ReturnsFromBars(bars)
	}
	a.mu.Lock()
	a.cache[symbol] = e
	a.mu.Unlock()
	return e.rets, e.err
}

// ReturnsFromBars converts closes into date -> simple daily return.
func ReturnsFromBars(bars []marketdata.DailyBar) map[string]float64 {
	out := make(map[string]float64, len(bars))
	for i := 1; i < len(bars); i++ {
		if bars[i-1].Close > 0 {
			out[bars[i].Date] = bars[i].Close/bars[i-1].Close - 1
		}
	}
	return out
}

// common returns the most recent `Window` dates present in every series, ascending.
func common(series ...map[string]float64) []string {
	if len(series) == 0 {
		return nil
	}
	var dates []string
	for d := range series[0] {
		in := true
		for _, s := range series[1:] {
			if _, ok := s[d]; !ok {
				in = false
				break
			}
		}
		if in {
			dates = append(dates, d)
		}
	}
	sort.Strings(dates)
	if len(dates) > Window {
		dates = dates[len(dates)-Window:]
	}
	return dates
}

func pick(s map[string]float64, dates []string) []float64 {
	out := make([]float64, len(dates))
	for i, d := range dates {
		out[i] = s[d]
	}
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// Corr is the Pearson correlation of a and b (same length); ok is false for
// too few points or a constant series.
func Corr(a, b []float64) (c float64, ok bool) {
	if len(a) != len(b) || len(a) < 2 {
		return 0, false
	}
	ma, mb := mean(a), mean(b)
	var cov, va, vb float64
	for i := range a {
		cov += (a[i] - ma) * (b[i] - mb)
		va += (a[i] - ma) * (a[i] - ma)
		vb += (b[i] - mb) * (b[i] - mb)
	}
	if va == 0 || vb == 0 {
		return 0, false
	}
	return cov / math.Sqrt(va*vb), true
}

// AnnualVolPct is the annualised standard deviation of daily returns, in percent.
func AnnualVolPct(rets []float64) float64 {
	if len(rets) < 2 {
		return 0
	}
	m := mean(rets)
	var ss float64
	for _, r := range rets {
		ss += (r - m) * (r - m)
	}
	return math.Sqrt(ss/float64(len(rets)-1)) * math.Sqrt(252) * 100
}

// Beta is cov(a, market) / var(market); ok is false for a constant market series.
func Beta(a, market []float64) (b float64, ok bool) {
	if len(a) != len(market) || len(a) < 2 {
		return 0, false
	}
	ma, mm := mean(a), mean(market)
	var cov, vm float64
	for i := range a {
		cov += (a[i] - ma) * (market[i] - mm)
		vm += (market[i] - mm) * (market[i] - mm)
	}
	if vm == 0 {
		return 0, false
	}
	return cov / vm, true
}

// PortfolioRisk estimates the annualised volatility (%) and beta to SPY of the
// current holdings, weighting each symbol's 60-day returns by weights (any
// scale; cash is simply not included, so risk is of the invested part scaled
// by `invested`, the fraction of equity that is invested, 0..1).
func (a *Analyzer) PortfolioRisk(weights map[string]float64, invested float64) (volPct, beta float64, ok bool) {
	spy, err := a.Returns("SPY")
	if err != nil || len(weights) == 0 {
		return 0, 0, false
	}
	series := []map[string]float64{spy}
	var syms []string
	var total float64
	for sym, w := range weights {
		if w <= 0 {
			continue
		}
		r, err := a.Returns(sym)
		if err != nil {
			continue // skip a symbol without data; the rest still describe most of the book
		}
		series = append(series, r)
		syms = append(syms, sym)
		total += w
	}
	if len(syms) == 0 || total == 0 {
		return 0, 0, false
	}
	dates := common(series...)
	if len(dates) < MinOverlap {
		return 0, 0, false
	}
	port := make([]float64, len(dates))
	for i, d := range dates {
		for j, sym := range syms {
			port[i] += weights[sym] / total * series[j+1][d]
		}
		port[i] *= invested
	}
	b, bok := Beta(port, pick(spy, dates))
	return AnnualVolPct(port), b, bok
}

// Correlation describes a candidate's 60-day correlation to what is held.
type Correlation struct {
	Avg    float64 // mean pairwise correlation to the held symbols
	Max    float64
	MaxSym string
	N      int // held symbols compared
}

// CorrelationTo computes the candidate's correlation to each held symbol (the
// candidate itself excluded). ok is false when nothing could be compared.
func (a *Analyzer) CorrelationTo(candidate string, held []string) (Correlation, bool) {
	cr, err := a.Returns(candidate)
	if err != nil {
		return Correlation{}, false
	}
	var out Correlation
	var sum float64
	out.Max = math.Inf(-1)
	for _, h := range held {
		if h == candidate {
			continue
		}
		hr, err := a.Returns(h)
		if err != nil {
			continue
		}
		dates := common(cr, hr)
		if len(dates) < MinOverlap {
			continue
		}
		c, ok := Corr(pick(cr, dates), pick(hr, dates))
		if !ok {
			continue
		}
		sum += c
		out.N++
		if c > out.Max {
			out.Max, out.MaxSym = c, h
		}
	}
	if out.N == 0 {
		return Correlation{}, false
	}
	out.Avg = sum / float64(out.N)
	return out, true
}
