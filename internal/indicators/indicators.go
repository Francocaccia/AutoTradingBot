package indicators

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
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
	// AvgDollarVolume is the mean of close*volume over ~60 completed sessions
	// from the same bars as the technicals (Alpaca IEX, a slice of consolidated
	// volume, so it understates liquidity). The liquidity fallback when Yahoo's
	// average volume is unavailable. 0 = unknown.
	AvgDollarVolume float64 `json:"avg_dollar_volume"`
	DayChangePct    float64 `json:"day_change_pct"` // last bar's close vs the previous close, %; 0 = unknown
	RangePct        float64 `json:"range_pct"`      // last bar's (high-low)/close, %; 0 = unknown
	MarketCap       float64 `json:"-"`              // USD, from Yahoo fundamentals; 0 = unknown
	// VolumeSource says where AvgVolume came from: "yahoo", "cache" (last good
	// Yahoo value, up to 24h old), "bars" (consolidated bars) or "" (unknown).
	VolumeSource string  `json:"-"`
	RelVolume    float64 `json:"rel_volume"` // last completed session volume / mean of the 60 sessions before it, same bars on both sides; 0 = unknown
	// StaleFor is how old the technicals are when a refresh failed and the last
	// good ones are served instead (stale-while-error); 0 = fresh.
	StaleFor  time.Duration `json:"-"`
	Sector    string        `json:"sector"` // empty when unavailable (e.g. ETFs)
	Name      string        `json:"name"`
	QuoteType string        `json:"quote_type"` // EQUITY, ETF, ...
	Source    string        `json:"source"`     // alpaca-iex or yfinance (bars)

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
	scriptPath = "python/indicators/calculate.py"

	// Timeouts of one worker run, by mode. The worker also keeps its own deadline
	// (see calculate.py), so these are the last resort against a hung process.
	barsTimeout         = 100 * time.Second
	fundamentalsTimeout = 110 * time.Second
	macroTimeout        = 75 * time.Second

	// Stale-while-error: when a refresh fails the last good value is served for up
	// to this long, with a warning, instead of nothing. Prices are the shortest,
	// because an order is sized from them; yields, the VIX and fundamentals move slowly.
	BarsStaleMax  = 30 * time.Minute
	FundStaleMax  = 72 * time.Hour
	MacroStaleMax = 24 * time.Hour

	MacroTTL        = time.Hour        // yields, VIX, SPY/TLT trend
	BarsTTL         = 10 * time.Minute // technicals
	FundamentalsTTL = 12 * time.Hour   // beta, P/E, sector, name, average volume; half of volumeTTL so a failed refresh still has a usable fallback
	macroFailureTTL = 10 * time.Minute
	failureTTL      = 2 * time.Minute // negative cache, so one cycle does not hammer a failing source
	volumeTTL       = 24 * time.Hour  // last good average volume, used when Yahoo fails
	logSnippet      = 300
)

// Runner executes the Python worker and returns its raw stdout and stderr.
type Runner func(args ...string) (stdout, stderr []byte, err error)

// modeTimeout is how long a worker run of the given mode may take.
func modeTimeout(mode string) time.Duration {
	switch mode {
	case "bars":
		return barsTimeout
	case "macro":
		return macroTimeout
	}
	return fundamentalsTimeout
}

func execRunner(args ...string) ([]byte, []byte, error) {
	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), modeTimeout(mode))
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", append([]string{scriptPath}, args...)...)
	cmd.Env = append(os.Environ(), "WORKER_BUDGET_S="+strconv.Itoa(int(modeTimeout(mode)/time.Second)-15))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

type barsEntry struct {
	res    *Result
	err    error
	at     time.Time // last refresh attempt
	goodAt time.Time // when res was fetched
	stale  bool      // res is the last good value, served because the latest refresh failed
}

type fundEntry struct {
	f      *fundamentals
	err    error
	at     time.Time
	goodAt time.Time
	stale  bool
}

type fundamentals struct {
	Beta      float64 `json:"beta"`
	PE        float64 `json:"pe_ratio"`
	AvgVolume float64 `json:"avg_volume"`
	MarketCap float64 `json:"market_cap"`
	Sector    string  `json:"sector"`
	Name      string  `json:"name"`
	QuoteType string  `json:"quote_type"`
	Error     string  `json:"error"`
}

// Provider fetches indicators with one batched worker call per cycle and
// caches them in memory: technicals for 10 minutes, fundamentals for 24 hours.
type Provider struct {
	run   Runner
	now   func() time.Time
	sleep func(time.Duration) // backoff between fundamentals retries; overridden in tests
	// jitter returns the actual wait for a base backoff delay (base plus up to
	// 50% random, so retries from several bursts do not line up); tests replace it.
	jitter func(time.Duration) time.Duration

	mu      sync.Mutex
	bars    map[string]barsEntry
	funds   map[string]fundEntry
	lastVol map[string]volEntry // last good average volume per symbol
	macro   macroEntry
}

// volEntry is the last non-zero Yahoo average volume seen for a symbol.
type volEntry struct {
	v  float64
	at time.Time
}

// fundRetryDelays are the waits before fundamentals retry 1 and 2 (3 attempts
// in all). Yahoo throttles cloud IPs in bursts, so a short backoff often works.
var fundRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second}

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
	m      Macro
	err    error
	at     time.Time
	goodAt time.Time
	stale  bool
}

// addJitter returns d plus up to 50% of d.
func addJitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int63n(int64(d)/2+1))
}

func NewProvider(run Runner) *Provider {
	if run == nil {
		run = execRunner
	}
	return &Provider{run: run, now: time.Now, sleep: time.Sleep, jitter: addJitter, bars: map[string]barsEntry{}, funds: map[string]fundEntry{}, lastVol: map[string]volEntry{}}
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
	if e.err != nil || e.stale {
		ttl = failureTTL // retry soon: a stale value is only a stop-gap
	}
	return p.now().Sub(e.at) < ttl
}

func (p *Provider) fundFresh(s string) bool {
	e, ok := p.funds[s]
	if !ok {
		return false
	}
	ttl := FundamentalsTTL
	if e.err != nil || e.stale {
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

// callRetryDelays are the waits before retry 1 and 2 of a whole-batch worker
// failure (3 attempts in all).
var callRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second}

// callRetry is call with a short, jittered backoff between attempts. Only a
// failure of the whole run is retried; a symbol the worker reports an error for
// is a real answer.
func (p *Provider) callRetry(mode string, symbols []string) (map[string]json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		raw, err := p.call(mode, symbols)
		if err == nil || attempt >= len(callRetryDelays) {
			return raw, err
		}
		delay := p.jitter(callRetryDelays[attempt])
		log.Printf("[Indicators] %s run failed (%v); retry %d/%d in %s", mode, err, attempt+1, len(callRetryDelays), delay)
		p.sleep(delay)
	}
}

func (p *Provider) fetchBars(symbols []string) {
	raw, batchErr := p.callRetry("bars", symbols)
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
				e.res, e.goodAt = &r, now
			}
		}
		if e.err != nil {
			// Stale-while-error: keep the last good technicals for a little while.
			if prev := p.bars[s]; prev.res != nil && now.Sub(prev.goodAt) < BarsStaleMax {
				log.Printf("[Indicators] WARN %s: refresh failed (%v); serving data from %s ago", s, e.err, now.Sub(prev.goodAt).Round(time.Second))
				e = barsEntry{res: prev.res, at: now, goodAt: prev.goodAt, stale: true}
			}
		}
		p.bars[s] = e
	}
}

// fetchFund loads fundamentals, retrying the symbols that failed with a short
// backoff (Yahoo often throttles shared cloud IPs for a few seconds). A good
// average volume is remembered for volumeTTL as a fallback.
func (p *Provider) fetchFund(symbols []string) {
	results := map[string]fundEntry{}
	remaining := symbols
	for attempt := 0; ; attempt++ {
		raw, batchErr := p.call("fundamentals", remaining)
		now := p.now()
		var failed []string
		var lastErr error
		for _, s := range remaining {
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
					e.f, e.goodAt = &f, now
				}
			}
			if e.err != nil {
				failed = append(failed, s)
				lastErr = e.err
			}
			results[s] = e
		}
		if len(failed) == 0 || attempt >= len(fundRetryDelays) {
			break
		}
		delay := p.jitter(fundRetryDelays[attempt])
		log.Printf("[Indicators] fundamentals failed for %d symbol(s) (%v); retry %d/%d in %s", len(failed), lastErr, attempt+1, len(fundRetryDelays), delay)
		p.sleep(delay)
		remaining = failed
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for s, e := range results {
		if e.err != nil {
			// Stale-while-error: beta, sector, name and the like barely change.
			if prev := p.funds[s]; prev.f != nil && e.at.Sub(prev.goodAt) < FundStaleMax {
				log.Printf("[Indicators] WARN %s: fundamentals refresh failed (%v); serving data from %s ago", s, e.err, e.at.Sub(prev.goodAt).Round(time.Minute))
				e = fundEntry{f: prev.f, at: e.at, goodAt: prev.goodAt, stale: true}
			}
		}
		p.funds[s] = e
		if e.f != nil && e.f.AvgVolume > 0 {
			p.lastVol[s] = volEntry{v: e.f.AvgVolume, at: e.at}
		}
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
	if b.stale {
		res.StaleFor = p.now().Sub(b.goodAt)
	}
	if f := p.funds[symbol].f; f != nil {
		res.Beta, res.PE, res.Sector, res.Name, res.QuoteType, res.MarketCap = f.Beta, f.PE, f.Sector, f.Name, f.QuoteType, f.MarketCap
		// IEX bar volume is a small slice of consolidated volume, so liquidity
		// comes from Yahoo's average volume whenever the bars are IEX.
		if res.Source != "yfinance" {
			res.AvgVolume = f.AvgVolume
			if res.AvgVolume > 0 {
				res.VolumeSource = "yahoo"
				if p.funds[symbol].stale {
					res.VolumeSource = "cache"
				}
			}
		}
	} else if res.Source != "yfinance" {
		res.AvgVolume = 0 // unknown: IEX volume would understate liquidity
	}
	if res.Source == "yfinance" && res.AvgVolume > 0 {
		res.VolumeSource = "bars"
	}
	if res.Source != "yfinance" && res.AvgVolume <= 0 {
		// Yahoo failed or returned nothing: fall back to the last good value (24h).
		if v, ok := p.lastVol[symbol]; ok && p.now().Sub(v.at) < volumeTTL {
			res.AvgVolume = v.v
			res.VolumeSource = "cache"
			log.Printf("[Indicators] %s: Yahoo average volume unavailable, using the cached value from %s ago (%.1fM)", symbol, p.now().Sub(v.at).Round(time.Minute), v.v/1e6)
		}
	}
	return &res, nil
}

// Macro returns the macro series, fetched through the Python worker and
// cached for MacroTTL. A failed refresh is retried with a backoff, and if it
// still fails the last good series is served for up to MacroStaleMax with a
// warning: yields, the VIX and the 200-day averages move slowly, and losing the
// regime to a Yahoo hiccup would restrict bond purchases for no reason.
func (p *Provider) Macro() (Macro, error) {
	p.mu.Lock()
	prev := p.macro
	p.mu.Unlock()
	ttl := MacroTTL
	if prev.err != nil || prev.stale {
		ttl = macroFailureTTL
	}
	if !prev.at.IsZero() && p.now().Sub(prev.at) < ttl {
		return prev.m, prev.err
	}

	raw, err := p.callRetry("macro", nil)
	now := p.now()
	e := macroEntry{at: now}
	if err == nil {
		e.m = Macro{}
		for sym, msg := range raw {
			var q MacroQuote
			if json.Unmarshal(msg, &q) == nil {
				e.m[sym] = q
			}
		}
		if len(e.m) == 0 {
			e.m, err = nil, errors.New("no macro series returned")
		}
	}
	switch {
	case err == nil:
		e.goodAt = now
	case prev.m != nil && now.Sub(prev.goodAt) < MacroStaleMax:
		log.Printf("[Indicators] WARN macro refresh failed (%v); serving the series from %s ago", err, now.Sub(prev.goodAt).Round(time.Minute))
		e = macroEntry{m: prev.m, at: now, goodAt: prev.goodAt, stale: true}
	default:
		e.err = err
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
