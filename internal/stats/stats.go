// Package stats computes the bot's public track record on demand from Alpaca
// (account equity history, filled orders, benchmark bars). Nothing is
// persisted: the responses carry percentages, indexed values and counts only,
// never dollar amounts, symbols, quantities, order ids or account identifiers.
package stats

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
	_ "time/tzdata" // session dates are in America/New_York; slim images may lack zoneinfo
)

const (
	// DefaultStartDate is used when TRACK_START_DATE is unset or invalid.
	DefaultStartDate = "2026-10-13"

	cacheTTL       = 5 * time.Minute
	errorTTL       = 30 * time.Second
	minTrackedDays = 2
	minRatioDays   = 20
	// minVolDays is the fewest daily return observations before volatility and
	// tracking error are reported; minBetaDays the same for beta and correlation.
	minVolDays   = 20
	minBetaDays  = 30
	insufficient = "insufficient data"
)

var (
	newYork          = mustLoad("America/New_York")
	errRecentFailure = errors.New("recent upstream failure")
)

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// Point is one dated value (equity or close).
type Point struct {
	Date  string // YYYY-MM-DD
	Value float64
}

// Sample is one intraday equity reading.
type Sample struct {
	At    time.Time
	Value float64
}

// Kind classifies a filled order for the public counters.
type Kind string

const (
	KindEntry      Kind = "entry"       // a buy: opens or adds to a long position
	KindStop       Kind = "stop"        // a sell filled by a stop-loss leg
	KindTakeProfit Kind = "take_profit" // a sell filled by a take-profit (limit) leg
	KindExit       Kind = "exit"        // any other sell (a decision to close, a manual sale)
)

// Order is a filled order; it is used for counting and win/loss only and is
// never serialised.
type Order struct {
	Symbol   string
	Side     string // "buy" or "sell"
	Type     string // order type: market, limit, stop, ...
	Qty      float64
	Price    float64
	FilledAt time.Time
}

// Classify says what kind of order o was. The bot is long-only: every buy is
// an entry and every sell an exit, told apart by the order type.
func (o Order) Classify() Kind {
	if o.Side == "buy" {
		return KindEntry
	}
	switch o.Type {
	case "stop", "stop_limit", "trailing_stop":
		return KindStop
	case "limit":
		return KindTakeProfit
	}
	return KindExit
}

// Allocation is the portfolio split by sleeve, as % of equity. Percentages
// only: never dollars, shares or symbols.
type Allocation struct {
	Equities float64 `json:"equities"`
	Bonds    float64 `json:"bonds"`
	Cash     float64 `json:"cash"`
}

// Band is one sleeve's policy range, % of equity.
type Band struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Bands are the policy bands of the three sleeves.
type Bands struct {
	Equities Band `json:"equities"`
	Bonds    Band `json:"bonds"`
	Cash     Band `json:"cash"`
}

// Source supplies the raw data. AlpacaSource is the production implementation.
type Source interface {
	EquityHistory(days int) ([]Point, error)
	// IntradayEquity returns intraday equity samples for the ET dates from start
	// to end (YYYY-MM-DD, inclusive). It may return fewer days than asked for.
	IntradayEquity(start, end string) ([]Sample, error)
	// Closes returns daily closes of a benchmark symbol (SPY, AGG) from start.
	Closes(symbol, start string) ([]Point, error)
	FilledOrders(start string) ([]Order, error)
	OpenPositions() (int, error)
	// Allocation returns the current sleeve split in percent of equity.
	Allocation() (Allocation, error)
	// SleeveWeights returns the equity / bond / cash weights (percent) at the close
	// of each date, given the account value at each close.
	SleeveWeights(equity []Point, dates []string) (map[string]Allocation, error)
}

// ExitsBreakdown splits exits_count by how the position was closed.
type ExitsBreakdown struct {
	StopLoss      int `json:"stop_loss"`
	TakeProfit    int `json:"take_profit"`
	Discretionary int `json:"discretionary"`
}

// Stats is the public /stats payload. Pointer fields serialise as null when
// there is not enough data yet.
type Stats struct {
	StartDate    string   `json:"start_date"`
	DaysTracked  int      `json:"days_tracked"`
	BotReturnPct *float64 `json:"bot_return_pct"`
	SPYReturnPct *float64 `json:"spy_return_pct"`
	ExcessPct    *float64 `json:"excess_return_pct"`
	MaxDrawdown  *float64 `json:"max_drawdown_pct"`
	BestDayPct   *float64 `json:"best_day_pct"`
	WorstDayPct  *float64 `json:"worst_day_pct"`

	Bench6040Pct    *float64    `json:"benchmark_60_40_return_pct"` // 60% SPY / 40% AGG, rebalanced daily
	BotVs6040Pct    *float64    `json:"bot_vs_60_40_pct"`
	MaxDrawdownSPY  *float64    `json:"max_drawdown_spy_pct"`
	MaxDrawdown6040 *float64    `json:"max_drawdown_60_40_pct"`
	RiskAdjusted    any         `json:"risk_adjusted"`  // Ratios once minRatioDays exist, else "insufficient data"
	AllocationPct   *Allocation `json:"allocation_pct"` // by sleeve, percent of equity
	PolicyBands     *Bands      `json:"policy_bands_pct"`

	// Market-relative risk, from the daily returns of the bot and SPY. Each is
	// null until its minimum number of observations exists.
	Observations       int      `json:"observations"` // daily return observations used
	BetaVsSPY          *float64 `json:"beta_vs_spy"`
	CorrelationVsSPY   *float64 `json:"correlation_vs_spy"`
	VolatilityAnnPct   *float64 `json:"volatility_ann_pct"`
	TrackingErrorVsSPY *float64 `json:"tracking_error_vs_spy_pct"` // annualised, percentage points

	NumTrades        *int            `json:"num_trades"`  // filled entry (buy) orders
	ExitsCount       *int            `json:"exits_count"` // filled exit (sell) orders of every kind
	ExitsBreakdown   *ExitsBreakdown `json:"exits_breakdown"`
	ClosedRoundTrips *int            `json:"closed_round_trips"` // the denominator of win_rate_pct
	WinRatePct       *float64        `json:"win_rate_pct"`
	OpenPositions    *int            `json:"open_positions_count"`

	LastUpdated string            `json:"last_updated"`
	Note        string            `json:"note,omitempty"`
	Definitions map[string]string `json:"definitions"`
}

// definitions documents every counter that could be misread. It carries no
// numbers, only wording.
var definitions = map[string]string{
	"num_trades":                 "Filled entry orders (buys) since start_date. Stop-loss, take-profit and other sells are not counted here; see exits_count.",
	"exits_count":                "Filled exit orders (sells) since start_date: stop-loss fills, take-profit fills and discretionary sells. exits_breakdown splits them.",
	"closed_round_trips":         "Positions opened and fully closed since start_date, on average cost. Positions that were already open at the start are not counted.",
	"win_rate_pct":               "Closed round trips with a positive realised result divided by closed_round_trips, in percent. null while none has closed.",
	"bot_return_pct":             "Change of the daily portfolio value from the last close before start_date to the latest value, in percent.",
	"max_drawdown_pct":           "Largest peak-to-trough fall of the daily portfolio value since the baseline, in percent (negative).",
	"benchmark_60_40_return_pct": "60% SPY and 40% AGG, rebalanced daily, over the same dates as the bot.",
	"beta_vs_spy":                "Covariance of daily bot and SPY returns over the variance of daily SPY returns. null below 30 observations.",
	"correlation_vs_spy":         "Correlation of daily bot and SPY returns. null below 30 observations.",
	"volatility_ann_pct":         "Standard deviation of daily bot returns times the square root of 252, in percent. null below 20 observations.",
	"tracking_error_vs_spy_pct":  "Standard deviation of the daily return difference between the bot and SPY times the square root of 252, in percentage points. null below 20 observations.",
	"risk_adjusted":              "Mean daily return over its standard deviation, annualised, no risk-free rate. \"insufficient data\" below 20 observations.",
	"policy_bands_pct":           "Allowed range of each sleeve (equities, bonds, cash including T-bill ETFs) in percent of portfolio value.",
}

// Service computes and caches Stats and History, and tracks the in-memory
// equity peak used by the drawdown halt.
type Service struct {
	src   Source
	start string
	now   func() time.Time
	bands *Bands

	mu       sync.Mutex
	cached   *Stats
	cachedAt time.Time
	failedAt time.Time

	histMu       sync.Mutex
	hist         []HistoryDay
	histAt       time.Time
	histFailedAt time.Time
	days         map[string]ohlc       // completed sessions' intraday range, kept for the life of the process
	weights      map[string]Allocation // sleeve weights at a finished close, kept for the life of the process

	allowOrigin func(origin string) bool

	peakMu     sync.Mutex
	peak       float64
	peakSeeded bool
}

// New returns a Service tracking from start (YYYY-MM-DD); an invalid start
// falls back to DefaultStartDate.
func New(src Source, start string) *Service {
	if _, err := time.Parse("2006-01-02", start); err != nil {
		log.Printf("[Stats] invalid TRACK_START_DATE %q, using %s", start, DefaultStartDate)
		start = DefaultStartDate
	}
	return &Service{src: src, start: start, now: time.Now, days: map[string]ohlc{}, weights: map[string]Allocation{}, allowOrigin: OriginPolicy("")}
}

// SetBands publishes the policy bands in /stats (percentages only).
func (s *Service) SetBands(b Bands) { s.bands = &b }

// SetOriginPolicy sets which browser origins may read /stats/history.
func (s *Service) SetOriginPolicy(allow func(origin string) bool) { s.allowOrigin = allow }

// Get returns the stats, recomputing at most once per cacheTTL. After a
// failure it fails fast for errorTTL so an outage cannot be amplified by
// public traffic.
func (s *Service) Get() (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.cached != nil && now.Sub(s.cachedAt) < cacheTTL {
		return *s.cached, nil
	}
	if !s.failedAt.IsZero() && now.Sub(s.failedAt) < errorTTL {
		return Stats{}, errRecentFailure
	}
	st, err := s.compute(now)
	if err != nil {
		s.failedAt = now
		return Stats{}, err
	}
	s.cached, s.cachedAt, s.failedAt = &st, now, time.Time{}
	return st, nil
}

// ObservePeak raises the in-memory equity high-water mark and returns it. On
// first use it is seeded from the equity history since the tracking start, so
// a restart does not forget the peak. It is never written to disk.
func (s *Service) ObservePeak(equity float64) float64 {
	s.peakMu.Lock()
	defer s.peakMu.Unlock()
	if !s.peakSeeded {
		if hist, err := s.src.EquityHistory(s.historyDays(s.now())); err == nil {
			s.peakSeeded = true
			for _, p := range hist {
				if p.Date >= s.start && p.Value > s.peak {
					s.peak = p.Value
				}
			}
		} else {
			log.Printf("[Stats] could not seed equity peak: %v", err)
		}
	}
	if equity > s.peak {
		s.peak = equity
	}
	return s.peak
}

// historyDays is how many days of history to request: everything since the
// start date plus a margin that covers the prior close.
func (s *Service) historyDays(now time.Time) int {
	start, _ := time.ParseInLocation("2006-01-02", s.start, newYork)
	return max(int(now.Sub(start).Hours()/24)+10, 10)
}

// lookbackStart is where to start fetching benchmark bars: early enough to
// include the session before the start date even across a long weekend or holiday.
func (s *Service) lookbackStart() string {
	t, _ := time.Parse("2006-01-02", s.start)
	return t.AddDate(0, 0, -10).Format("2006-01-02")
}

// Ratios are annualised risk-adjusted figures: mean daily return over its
// standard deviation, times sqrt(252), with no risk-free rate.
type Ratios struct {
	Bot       *float64 `json:"bot"`
	SPY       *float64 `json:"spy"`
	Bench6040 *float64 `json:"benchmark_60_40"`
}

// tracked is the bot and its SPY / AGG benchmarks on a common set of dates.
// Index 0 of every slice is the baseline (the last close before the start
// date); dates[i] belongs to index i+1.
type tracked struct {
	dates         []string
	bot, spy, agg []float64
}

func (s *Service) compute(now time.Time) (Stats, error) {
	out := Stats{StartDate: s.start, LastUpdated: now.UTC().Format(time.RFC3339), RiskAdjusted: insufficient,
		PolicyBands: s.bands, Definitions: definitions}

	equity, err := s.src.EquityHistory(s.historyDays(now))
	if err != nil {
		return Stats{}, err
	}
	spy, err := s.src.Closes("SPY", s.lookbackStart())
	if err != nil {
		return Stats{}, err
	}
	agg, err := s.src.Closes("AGG", s.lookbackStart())
	if err != nil {
		return Stats{}, err
	}
	botVals, benchVals, dates := alignAll(equity, [][]Point{spy, agg}, s.start)
	out.DaysTracked, out.Observations = len(dates), len(dates)
	if out.DaysTracked < minTrackedDays {
		out.Note = insufficient
		return out, nil
	}
	spyVals, aggVals := benchVals[0], benchVals[1]
	mix := blend6040(spyVals, aggVals)

	last := len(botVals) - 1
	bot, bench, mixRet := pctChange(botVals[last], botVals[0]), pctChange(spyVals[last], spyVals[0]), pctChange(mix[last], mix[0])
	botRets, spyRets := dailyReturns(botVals), dailyReturns(spyVals)
	worst, best := minMax(botRets)
	out.BotReturnPct, out.SPYReturnPct = ptr(round2(bot)), ptr(round2(bench))
	out.ExcessPct = ptr(round2(bot - bench))
	out.Bench6040Pct, out.BotVs6040Pct = ptr(round2(mixRet)), ptr(round2(bot-mixRet))
	out.MaxDrawdown = ptr(round2(maxDrawdown(botVals)))
	out.MaxDrawdownSPY, out.MaxDrawdown6040 = ptr(round2(maxDrawdown(spyVals))), ptr(round2(maxDrawdown(mix)))
	out.BestDayPct, out.WorstDayPct = ptr(round2(best)), ptr(round2(worst))
	if out.DaysTracked >= minRatioDays {
		out.RiskAdjusted = Ratios{Bot: ratio(botVals), SPY: ratio(spyVals), Bench6040: ratio(mix)}
	}
	if len(botRets) >= minVolDays {
		out.VolatilityAnnPct = ptr(round2(annualise(stdev(botRets))))
		out.TrackingErrorVsSPY = ptr(round2(annualise(stdev(activeReturns(botRets, spyRets)))))
	}
	if len(botRets) >= minBetaDays {
		if beta, corr, ok := betaCorr(botRets, spyRets); ok {
			out.BetaVsSPY, out.CorrelationVsSPY = ptr(round2(beta)), ptr(round2(corr))
		}
	}

	orders, err := s.src.FilledOrders(s.start)
	if err != nil {
		return Stats{}, err
	}
	counts := countOrders(orders, s.start)
	out.NumTrades, out.ExitsCount = ptr(counts.entries), ptr(counts.exits())
	out.ExitsBreakdown = &ExitsBreakdown{StopLoss: counts.stops, TakeProfit: counts.takeProfits, Discretionary: counts.other}
	wins, total := roundTrips(sinceStart(orders, s.start))
	out.ClosedRoundTrips = ptr(total)
	if total > 0 {
		out.WinRatePct = ptr(round2(float64(wins) / float64(total) * 100))
	}
	open, err := s.src.OpenPositions()
	if err != nil {
		return Stats{}, err
	}
	out.OpenPositions = &open
	if alloc, err := s.src.Allocation(); err != nil {
		log.Printf("[Stats] allocation unavailable: %v", err)
	} else {
		alloc.Equities, alloc.Bonds, alloc.Cash = round2(alloc.Equities), round2(alloc.Bonds), round2(alloc.Cash)
		out.AllocationPct = &alloc
	}
	return out, nil
}

// orderCounts tallies filled orders by kind.
type orderCounts struct{ entries, stops, takeProfits, other int }

func (c orderCounts) exits() int { return c.stops + c.takeProfits + c.other }

// sinceStart keeps the orders filled on or after start (an ET date), oldest
// first. The broker query is by submission time, so this makes the cut exact.
func sinceStart(orders []Order, start string) []Order {
	out := make([]Order, 0, len(orders))
	for _, o := range orders {
		if o.FilledAt.IsZero() || o.FilledAt.In(newYork).Format("2006-01-02") >= start {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FilledAt.Before(out[j].FilledAt) })
	return out
}

// countOrders counts the filled orders since start by Order.Classify.
func countOrders(orders []Order, start string) orderCounts {
	var c orderCounts
	for _, o := range sinceStart(orders, start) {
		switch o.Classify() {
		case KindEntry:
			c.entries++
		case KindStop:
			c.stops++
		case KindTakeProfit:
			c.takeProfits++
		default:
			c.other++
		}
	}
	return c
}

// Performance returns the cached since-start returns (percent) of the bot,
// SPY and the 60/40 benchmark, or nils when they are not available yet.
func (s *Service) Performance() (bot, spy, bench6040 *float64) {
	st, err := s.Get()
	if err != nil {
		return nil, nil, nil
	}
	return st.BotReturnPct, st.SPYReturnPct, st.Bench6040Pct
}

func ptr[T any](v T) *T { return &v }

// align returns bot and one benchmark's values on a common set of dates.
func align(bot, bench []Point, start string) (botVals, benchVals []float64, dates []string) {
	b, bs, d := alignAll(bot, [][]Point{bench}, start)
	if b == nil {
		return nil, nil, nil
	}
	return b, bs[0], d
}

// alignAll returns bot values and each benchmark's values on a common set of
// dates (those where every series has a value). Index 0 is the baseline: the
// last session before start (the prior close), or the bot's first data point
// if the account has no earlier history. The remaining entries are the tracked
// sessions, so len(dates) is the number of days.
func alignAll(bot []Point, benches [][]Point, start string) (botVals []float64, benchVals [][]float64, dates []string) {
	sort.SliceStable(bot, func(i, j int) bool { return bot[i].Date < bot[j].Date })

	base := -1
	for i, p := range bot {
		if p.Date < start {
			base = i
		}
	}
	if base < 0 {
		for i, p := range bot {
			if p.Date >= start {
				base = i
				break
			}
		}
	}
	if base < 0 {
		return nil, nil, nil
	}

	at := make([]map[string]float64, len(benches))
	baseVals := make([]float64, len(benches))
	for i, series := range benches {
		sort.SliceStable(series, func(a, b int) bool { return series[a].Date < series[b].Date })
		at[i] = map[string]float64{}
		for _, p := range series {
			at[i][p.Date] = p.Value
			if p.Date <= bot[base].Date {
				baseVals[i] = p.Value
			}
		}
		if baseVals[i] <= 0 {
			return nil, nil, nil
		}
	}

	botVals = []float64{bot[base].Value}
	benchVals = make([][]float64, len(benches))
	for i := range benches {
		benchVals[i] = []float64{baseVals[i]}
	}
	for _, p := range bot[base+1:] {
		if p.Date < start {
			continue
		}
		row := make([]float64, len(benches))
		ok := true
		for i := range benches {
			v, found := at[i][p.Date]
			if !found {
				ok = false
				break
			}
			row[i] = v
		}
		if !ok {
			continue
		}
		botVals = append(botVals, p.Value)
		for i := range benches {
			benchVals[i] = append(benchVals[i], row[i])
		}
		dates = append(dates, p.Date)
	}
	return botVals, benchVals, dates
}

// roundTrips counts closed long round trips (position goes from open to flat)
// and how many of them realised a profit, using average cost. Sells with no
// tracked entry (positions opened before tracking began) are ignored.
func roundTrips(orders []Order) (wins, total int) {
	type pos struct{ qty, cost, pnl float64 }
	book := map[string]*pos{}
	for _, o := range orders {
		p := book[o.Symbol]
		if p == nil {
			p = &pos{}
			book[o.Symbol] = p
		}
		switch o.Side {
		case "buy":
			p.qty += o.Qty
			p.cost += o.Qty * o.Price
		case "sell":
			if p.qty <= 0 {
				continue
			}
			q := min(o.Qty, p.qty)
			avg := p.cost / p.qty
			p.pnl += q * (o.Price - avg)
			p.qty -= q
			p.cost -= q * avg
			if p.qty < 1e-9 {
				total++
				if p.pnl > 0 {
					wins++
				}
				*p = pos{}
			}
		}
	}
	return wins, total
}

// Handler serves GET /stats. Upstream errors are logged, never returned.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		st, err := s.Get()
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			if !errors.Is(err, errRecentFailure) {
				log.Printf("[Stats] compute failed: %v", err)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"stats unavailable"}` + "\n"))
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(st)
	})
}
