// Package stats computes the bot's public track record on demand from Alpaca
// (account equity history, filled orders, SPY bars). Nothing is persisted:
// the response carries percentages and counts only, never dollar amounts,
// symbols, quantities or account identifiers.
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
	insufficient   = "insufficient data"
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

// Order is a filled order; it is used for counting and win/loss only and is
// never serialised.
type Order struct {
	Symbol   string
	Side     string // "buy" or "sell"
	Qty      float64
	Price    float64
	FilledAt time.Time
}

// Allocation is the portfolio split by sleeve, as % of equity. Percentages
// only: never dollars, shares or symbols.
type Allocation struct {
	Equities float64 `json:"equities"`
	Bonds    float64 `json:"bonds"`
	Cash     float64 `json:"cash"`
}

// Source supplies the raw data. AlpacaSource is the production implementation.
type Source interface {
	EquityHistory(days int) ([]Point, error)
	// Closes returns daily closes of a benchmark symbol (SPY, AGG) from start.
	Closes(symbol, start string) ([]Point, error)
	FilledOrders(start string) ([]Order, error)
	OpenPositions() (int, error)
	// Allocation returns the current sleeve split in percent of equity.
	Allocation() (Allocation, error)
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
	NumTrades       *int        `json:"num_trades"`
	WinRatePct      *float64    `json:"win_rate_pct"`
	OpenPositions   *int        `json:"open_positions_count"`
	LastUpdated     string      `json:"last_updated"`
	Note            string      `json:"note,omitempty"`
}

// Service computes and caches Stats, and tracks the in-memory equity peak
// used by the drawdown halt.
type Service struct {
	src   Source
	start string
	now   func() time.Time

	mu       sync.Mutex
	cached   *Stats
	cachedAt time.Time
	failedAt time.Time

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
	return &Service{src: src, start: start, now: time.Now}
}

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

// lookbackStart is where to start fetching SPY bars: early enough to include
// the session before the start date even across a long weekend or holiday.
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

func (s *Service) compute(now time.Time) (Stats, error) {
	out := Stats{StartDate: s.start, LastUpdated: now.UTC().Format(time.RFC3339), RiskAdjusted: insufficient}

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
	out.DaysTracked = len(dates)
	if out.DaysTracked < minTrackedDays {
		out.Note = insufficient
		return out, nil
	}
	spyVals, aggVals := benchVals[0], benchVals[1]
	mix := blend6040(spyVals, aggVals)

	last := len(botVals) - 1
	bot, bench, mixRet := pctChange(botVals[last], botVals[0]), pctChange(spyVals[last], spyVals[0]), pctChange(mix[last], mix[0])
	worst, best := minMax(dailyReturns(botVals))
	out.BotReturnPct, out.SPYReturnPct = ptr(round2(bot)), ptr(round2(bench))
	out.ExcessPct = ptr(round2(bot - bench))
	out.Bench6040Pct, out.BotVs6040Pct = ptr(round2(mixRet)), ptr(round2(bot-mixRet))
	out.MaxDrawdown = ptr(round2(maxDrawdown(botVals)))
	out.MaxDrawdownSPY, out.MaxDrawdown6040 = ptr(round2(maxDrawdown(spyVals))), ptr(round2(maxDrawdown(mix)))
	out.BestDayPct, out.WorstDayPct = ptr(round2(best)), ptr(round2(worst))
	if out.DaysTracked >= minRatioDays {
		out.RiskAdjusted = Ratios{Bot: ratio(botVals), SPY: ratio(spyVals), Bench6040: ratio(mix)}
	}

	orders, err := s.src.FilledOrders(s.start)
	if err != nil {
		return Stats{}, err
	}
	out.NumTrades = ptr(len(orders))
	if wins, total := roundTrips(orders); total > 0 {
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
