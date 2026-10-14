package indicators

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"
)

type Result struct {
	Symbol string  `json:"symbol"`
	Price  float64 `json:"price"`
	RSI    float64 `json:"rsi"`
	Volume int64   `json:"volume"` // in-progress session; IEX-only on the Alpaca feed, never sent to the model
	Beta   float64 `json:"beta"`
	PE     float64 `json:"pe_ratio"`

	ATR       float64 `json:"atr"`        // ATR(14), in dollars
	AvgVolume float64 `json:"avg_volume"` // mean daily volume over ~60 completed sessions (consolidated via Yahoo for IEX bars); liquidity filter only
	RelVolume float64 `json:"rel_volume"` // last completed session volume / mean of the 60 sessions before it, same bars on both sides; 0 = unknown
	Sector    string  `json:"sector"`     // empty when unavailable (e.g. ETFs)
	Name      string  `json:"name"`
	QuoteType string  `json:"quote_type"` // EQUITY, ETF, ...
	Source    string  `json:"source"`     // alpaca-iex or yfinance (bars)

	Ichimoku   IchimokuResult   `json:"ichimoku"`
	MACD       MACDResult       `json:"macd"`
	Stochastic StochasticResult `json:"stochastic"`
	Keltner    KeltnerResult    `json:"keltner"`
}

type IchimokuResult struct {
	Tenkan float64 `json:"tenkan"`
	Kijun  float64 `json:"kijun"`
	SpanA  float64 `json:"span_a"`
	SpanB  float64 `json:"span_b"`
	Chikou float64 `json:"chikou"`
}

type MACDResult struct {
	MACD      float64 `json:"macd"`
	Signal    float64 `json:"signal"`
	Histogram float64 `json:"histogram"`
}

type StochasticResult struct {
	K float64 `json:"k"`
	D float64 `json:"d"`
}

type KeltnerResult struct {
	Upper  float64 `json:"upper"`
	Middle float64 `json:"middle"`
	Lower  float64 `json:"lower"`
}

const (
	calcTimeout = 120 * time.Second
	scriptPath  = "python/indicators/calculate.py"

	MacroTTL        = time.Hour        // yields, VIX, SPY/TLT trend
	BarsTTL         = 10 * time.Minute // technicals
	FundamentalsTTL = 24 * time.Hour   // beta, P/E, sector, name, average volume
	macroFailureTTL = 10 * time.Minute
	failureTTL      = 2 * time.Minute // negative cache, so one cycle does not hammer a failing source
	logSnippet      = 300
)

// Runner executes the Python worker and returns its raw stdout and stderr.
type Runner func(args ...string) (stdout, stderr []byte, err error)

func execRunner(args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), calcTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", append([]string{scriptPath}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

type barsEntry struct {
	res *Result
	err error
	at  time.Time
}

type fundEntry struct {
	f   *fundamentals
	err error
	at  time.Time
}

type fundamentals struct {
	Beta      float64 `json:"beta"`
	PE        float64 `json:"pe_ratio"`
	AvgVolume float64 `json:"avg_volume"`
	Sector    string  `json:"sector"`
	Name      string  `json:"name"`
	QuoteType string  `json:"quote_type"`
	Error     string  `json:"error"`
}

// Provider fetches indicators with one batched worker call per cycle and
// caches them in memory: technicals for 10 minutes, fundamentals for 24 hours.
type Provider struct {
	run Runner
	now func() time.Time

	mu    sync.Mutex
	bars  map[string]barsEntry
	funds map[string]fundEntry
	macro macroEntry
}

// MacroQuote is the latest close of a macro series and, for SPY and TLT, its
// 200-day simple moving average. A nil field means the value is unavailable.
type MacroQuote struct {
	Close *float64 `json:"close"`
	MA200 *float64 `json:"ma200"`
}

// Macro maps a Yahoo symbol (^IRX, ^FVX, ^TNX, ^TYX, ^VIX, SPY, TLT) to its quote.
// Yields are in percent.
type Macro map[string]MacroQuote

// Value returns the latest close of sym.
func (m Macro) Value(sym string) (float64, bool) {
	q, ok := m[sym]
	if !ok || q.Close == nil {
		return 0, false
	}
	return *q.Close, true
}

// MA200 returns the 200-day moving average of sym.
func (m Macro) MA200(sym string) (float64, bool) {
	q, ok := m[sym]
	if !ok || q.MA200 == nil {
		return 0, false
	}
	return *q.MA200, true
}

type macroEntry struct {
	m   Macro
	err error
	at  time.Time
}

func NewProvider(run Runner) *Provider {
	if run == nil {
		run = execRunner
	}
	return &Provider{run: run, now: time.Now, bars: map[string]barsEntry{}, funds: map[string]fundEntry{}}
}

var defaultProvider = NewProvider(nil)

// Prefetch warms the caches for symbols in at most one technicals call and one
// fundamentals call. Failures are cached briefly and surface from Calculate.
func Prefetch(symbols []string) { defaultProvider.Prefetch(symbols) }

// GetMacro returns cached macro data (refreshed hourly); on failure the error
// is cached briefly so a Yahoo outage does not slow every cycle.
func GetMacro() (Macro, error) { return defaultProvider.Macro() }

// Calculate returns the indicators for symbol, from cache when fresh.
func Calculate(symbol string) (*Result, error) { return defaultProvider.Calculate(symbol) }

func (p *Provider) Prefetch(symbols []string) {
	var needBars, needFund []string
	p.mu.Lock()
	seen := map[string]bool{}
	for _, s := range symbols {
		if seen[s] {
			continue
		}
		seen[s] = true
		if !p.barsFresh(s) {
			needBars = append(needBars, s)
		}
		if !p.fundFresh(s) {
			needFund = append(needFund, s)
		}
	}
	p.mu.Unlock()
	if len(needBars) > 0 {
		p.fetchBars(needBars)
	}
	if len(needFund) > 0 {
		p.fetchFund(needFund)
	}
}

// caller holds p.mu
func (p *Provider) barsFresh(s string) bool {
	e, ok := p.bars[s]
	if !ok {
		return false
	}
	ttl := BarsTTL
	if e.err != nil {
		ttl = failureTTL
	}
	return p.now().Sub(e.at) < ttl
}

func (p *Provider) fundFresh(s string) bool {
	e, ok := p.funds[s]
	if !ok {
		return false
	}
	ttl := FundamentalsTTL
	if e.err != nil {
		ttl = failureTTL
	}
	return p.now().Sub(e.at) < ttl
}

// call runs the worker and decodes its {"SYM": {...}} object. A top-level
// {"error": "..."} or unparseable output fails the whole batch; the first 300
// characters of the raw stdout and stderr are logged.
func (p *Provider) call(mode string, symbols []string) (map[string]json.RawMessage, error) {
	stdout, stderr, runErr := p.run(append([]string{mode}, symbols...)...)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stdout, &raw); err != nil {
		log.Printf("[Indicators] failed to parse indicators (%s %v): stdout=%q stderr=%q", mode, symbols, snippet(stdout), snippet(stderr))
		if runErr != nil {
			return nil, fmt.Errorf("python worker failed: %w", runErr)
		}
		return nil, fmt.Errorf("failed to parse indicators: %w", err)
	}
	if msg, ok := raw["error"]; ok && len(raw) == 1 {
		var reason string
		if json.Unmarshal(msg, &reason) != nil {
			reason = string(msg)
		}
		if len(stderr) > 0 {
			log.Printf("[Indicators] %s %v failed: %s (stderr=%q)", mode, symbols, reason, snippet(stderr))
		}
		return nil, errors.New(reason)
	}
	return raw, nil
}

func (p *Provider) fetchBars(symbols []string) {
	raw, batchErr := p.call("bars", symbols)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, s := range symbols {
		e := barsEntry{at: now}
		switch {
		case batchErr != nil:
			e.err = batchErr
		case raw[s] == nil:
			e.err = errors.New("no result returned for symbol")
		default:
			var r Result
			var errOnly struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(raw[s], &r); err != nil {
				e.err = fmt.Errorf("failed to parse indicators: %w", err)
			} else if _ = json.Unmarshal(raw[s], &errOnly); errOnly.Error != "" {
				e.err = errors.New(errOnly.Error)
			} else {
				r.Symbol = s
				e.res = &r
			}
		}
		p.bars[s] = e
	}
}

func (p *Provider) fetchFund(symbols []string) {
	raw, batchErr := p.call("fundamentals", symbols)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, s := range symbols {
		e := fundEntry{at: now}
		switch {
		case batchErr != nil:
			e.err = batchErr
		case raw[s] == nil:
			e.err = errors.New("no fundamentals returned")
		default:
			var f fundamentals
			if err := json.Unmarshal(raw[s], &f); err != nil {
				e.err = err
			} else if f.Error != "" {
				e.err = errors.New(f.Error)
			} else {
				e.f = &f
			}
		}
		p.funds[s] = e
	}
}

// Calculate merges cached technicals and fundamentals for symbol, fetching
// whatever is missing. Technicals are required; fundamentals are best-effort
// (Yahoo rate-limits cloud IPs), in which case beta/P/E/sector stay empty.
func (p *Provider) Calculate(symbol string) (*Result, error) {
	p.Prefetch([]string{symbol})

	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.bars[symbol]
	if b.res == nil {
		return nil, fmt.Errorf("indicators for %s: %w", symbol, b.err)
	}
	res := *b.res
	if f := p.funds[symbol].f; f != nil {
		res.Beta, res.PE, res.Sector, res.Name, res.QuoteType = f.Beta, f.PE, f.Sector, f.Name, f.QuoteType
		// IEX bar volume is a small slice of consolidated volume, so liquidity
		// comes from Yahoo's average volume whenever the bars are IEX.
		if res.Source != "yfinance" {
			res.AvgVolume = f.AvgVolume
		}
	} else if res.Source != "yfinance" {
		res.AvgVolume = 0 // unknown: IEX volume would understate liquidity
	}
	return &res, nil
}

// Macro returns the macro series, fetched through the Python worker and
// cached for MacroTTL.
func (p *Provider) Macro() (Macro, error) {
	p.mu.Lock()
	e := p.macro
	p.mu.Unlock()
	ttl := MacroTTL
	if e.err != nil {
		ttl = macroFailureTTL
	}
	if !e.at.IsZero() && p.now().Sub(e.at) < ttl {
		return e.m, e.err
	}

	raw, err := p.call("macro", nil)
	e = macroEntry{at: p.now()}
	if err != nil {
		e.err = err
	} else {
		e.m = Macro{}
		for sym, msg := range raw {
			var q MacroQuote
			if json.Unmarshal(msg, &q) == nil {
				e.m[sym] = q
			}
		}
		if len(e.m) == 0 {
			e.err = errors.New("no macro series returned")
		}
	}
	p.mu.Lock()
	p.macro = e
	p.mu.Unlock()
	return e.m, e.err
}

func snippet(b []byte) string {
	if len(b) > logSnippet {
		return string(b[:logSnippet])
	}
	return string(b)
}
