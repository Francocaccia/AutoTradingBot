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

// Source supplies the raw data. AlpacaSource is the production implementation.
type Source interface {
	EquityHistory(days int) ([]Point, error)
	BenchmarkCloses(start string) ([]Point, error)
	FilledOrders(start string) ([]Order, error)
	OpenPositions() (int, error)
}

// Stats is the public /stats payload. Pointer fields serialise as null when
// there is not enough data yet.
type Stats struct {
	StartDate     string   `json:"start_date"`
	DaysTracked   int      `json:"days_tracked"`
	BotReturnPct  *float64 `json:"bot_return_pct"`
	SPYReturnPct  *float64 `json:"spy_return_pct"`
	ExcessPct     *float64 `json:"excess_return_pct"`
	MaxDrawdown   *float64 `json:"max_drawdown_pct"`
	BestDayPct    *float64 `json:"best_day_pct"`
	WorstDayPct   *float64 `json:"worst_day_pct"`
	NumTrades     *int     `json:"num_trades"`
	WinRatePct    *float64 `json:"win_rate_pct"`
	OpenPositions *int     `json:"open_positions_count"`
	LastUpdated   string   `json:"last_updated"`
	Note          string   `json:"note,omitempty"`
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

func (s *Service) compute(now time.Time) (Stats, error) {
	out := Stats{StartDate: s.start, LastUpdated: now.UTC().Format(time.RFC3339)}

	equity, err := s.src.EquityHistory(s.historyDays(now))
	if err != nil {
		return Stats{}, err
	}
	spy, err := s.src.BenchmarkCloses(s.lookbackStart())
	if err != nil {
		return Stats{}, err
	}
	botVals, spyVals, dates := align(equity, spy, s.start)
	out.DaysTracked = len(dates)
	if out.DaysTracked < minTrackedDays {
		out.Note = insufficient
		return out, nil
	}

	last := len(botVals) - 1
	bot, bench := pctChange(botVals[last], botVals[0]), pctChange(spyVals[last], spyVals[0])
	worst, best := minMax(dailyReturns(botVals))
	out.BotReturnPct, out.SPYReturnPct = ptr(round2(bot)), ptr(round2(bench))
	out.ExcessPct = ptr(round2(bot - bench))
	out.MaxDrawdown = ptr(round2(maxDrawdown(botVals)))
	out.BestDayPct, out.WorstDayPct = ptr(round2(best)), ptr(round2(worst))

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
	return out, nil
}

func ptr[T any](v T) *T { return &v }

// align returns bot and SPY values on a common set of dates. Index 0 is the
// baseline: the last session before start (the prior close), or the bot's
// first data point if the account has no earlier history. The remaining
// entries are the tracked sessions, so len(dates) is the number of days.
func align(bot, spy []Point, start string) (botVals, spyVals []float64, dates []string) {
	sort.SliceStable(bot, func(i, j int) bool { return bot[i].Date < bot[j].Date })
	sort.SliceStable(spy, func(i, j int) bool { return spy[i].Date < spy[j].Date })

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
	spyAt := map[string]float64{}
	baseSPY := 0.0
	for _, p := range spy {
		spyAt[p.Date] = p.Value
		if p.Date <= bot[base].Date {
			baseSPY = p.Value
		}
	}
	if baseSPY <= 0 {
		return nil, nil, nil
	}
	botVals, spyVals = []float64{bot[base].Value}, []float64{baseSPY}
	for _, p := range bot[base+1:] {
		if v, ok := spyAt[p.Date]; ok && p.Date >= start {
			botVals, spyVals, dates = append(botVals, p.Value), append(spyVals, v), append(dates, p.Date)
		}
	}
	return botVals, spyVals, dates
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
