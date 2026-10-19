package stats

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// historyBenchmarks are the extra series on top of SPY and AGG (which /stats
// already needs). A missing one serialises as null instead of failing the call.
const (
	benchQQQ = "QQQ"
	benchBIL = "BIL" // cash proxy: 1-3 month T-bills

	// missingDayTTL is how long a completed day without intraday data is left
	// alone before it is asked for again.
	missingDayTTL = time.Hour
)

// HistoryDay is one trading day of the public performance history. Every value
// is indexed to 100 at the baseline (the last close before TRACK_START_DATE),
// so it carries no dollar amounts.
type HistoryDay struct {
	Date string   `json:"date"`
	O    float64  `json:"o"`
	H    float64  `json:"h"`
	L    float64  `json:"l"`
	C    float64  `json:"c"`
	SPY  *float64 `json:"spy"`
	QQQ  *float64 `json:"qqq"`
	AGG  *float64 `json:"agg"`
	Mix  *float64 `json:"mix_60_40"`
	Cash *float64 `json:"cash"`
	// AllocMatched is the bot's own daily equity / bond / cash weights applied to
	// SPY, AGG and BIL returns, indexed to 100: the fair comparison for a
	// portfolio that is required to hold bonds and cash.
	AllocMatched *float64 `json:"alloc_matched"`
}

// ohlc is one session's intraday range of the portfolio value, in the account
// currency (never serialised; only its ratio to the baseline is).
type ohlc struct {
	o, h, l float64
	ok      bool      // false: the session had no intraday samples
	at      time.Time // when a missing session was last looked for
}

// History returns the indexed daily candles of the portfolio and its
// benchmarks, empty until two trading days exist. It is cached like Get.
func (s *Service) History() ([]HistoryDay, error) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	now := s.now()
	if s.hist != nil && now.Sub(s.histAt) < cacheTTL {
		return s.hist, nil
	}
	if !s.histFailedAt.IsZero() && now.Sub(s.histFailedAt) < errorTTL {
		return nil, errRecentFailure
	}
	h, err := s.computeHistory(now)
	if err != nil {
		s.histFailedAt = now
		return nil, err
	}
	s.hist, s.histAt, s.histFailedAt = h, now, time.Time{}
	return h, nil
}

func (s *Service) computeHistory(now time.Time) ([]HistoryDay, error) {
	equity, err := s.src.EquityHistory(s.historyDays(now))
	if err != nil {
		return nil, err
	}
	benches := make([][]Point, 0, 4)
	for _, sym := range []string{"SPY", "AGG"} {
		pts, err := s.src.Closes(sym, s.lookbackStart())
		if err != nil {
			return nil, err
		}
		benches = append(benches, pts)
	}
	// Optional series: left out of the alignment, and null in the output, when
	// they cannot be loaded.
	optional := map[string]int{}
	for _, sym := range []string{benchQQQ, benchBIL} {
		pts, err := s.src.Closes(sym, s.lookbackStart())
		if err != nil || len(pts) == 0 {
			log.Printf("[Stats] history: %s unavailable: %v", sym, err)
			continue
		}
		optional[sym] = len(benches)
		benches = append(benches, pts)
	}

	botVals, benchVals, dates := alignAll(equity, benches, s.start)
	if len(dates) < minTrackedDays {
		return []HistoryDay{}, nil
	}

	raw := s.intradayRanges(dates, now)

	bot := indexTo100(botVals)
	spy, agg := indexTo100(benchVals[0]), indexTo100(benchVals[1])
	mix := indexTo100(blend6040(benchVals[0], benchVals[1]))
	if bot == nil || spy == nil || agg == nil || mix == nil {
		return nil, errors.New("no baseline for the history")
	}
	var qqq, bil []float64
	if i, ok := optional[benchQQQ]; ok {
		qqq = indexTo100(benchVals[i])
	}
	if i, ok := optional[benchBIL]; ok {
		bil = indexTo100(benchVals[i])
	}

	var matched []float64
	if bil != nil {
		matched = s.allocMatched(equity, dates, benchVals, optional[benchBIL])
	}

	base := botVals[0]
	idx := func(v float64) float64 { return round2(v / base * 100) }
	at := func(series []float64, i int) *float64 {
		if series == nil {
			return nil
		}
		return ptr(round2(series[i]))
	}
	out := make([]HistoryDay, 0, len(dates))
	for i, d := range dates {
		c := botVals[i+1]
		o, h, l := c, c, c // close only unless the session has intraday samples
		if r, ok := raw[d]; ok && r.ok {
			o = r.o
			h = max(r.h, c, o)
			l = min(r.l, c, o)
		}
		out = append(out, HistoryDay{
			Date: d, O: idx(o), H: idx(h), L: idx(l), C: idx(c),
			SPY: at(spy, i+1), QQQ: at(qqq, i+1), AGG: at(agg, i+1), Mix: at(mix, i+1), Cash: at(bil, i+1),
			AllocMatched: at(matched, i+1),
		})
	}
	return out, nil
}

// allocMatched builds the allocation-matched index over the tracked dates, or nil
// when the weights cannot be rebuilt (the benchmark is then null, never invented).
// benchVals holds the aligned SPY (0) and AGG (1) closes and bilIdx is BIL's index.
func (s *Service) allocMatched(equity []Point, dates []string, benchVals [][]float64, bilIdx int) []float64 {
	base := baselineDate(equity, s.start)
	if base == "" {
		return nil
	}
	// The weights held at the close before each day: the baseline, then each tracked
	// date but the last.
	need := append([]string{base}, dates[:len(dates)-1]...)
	var missing []string
	for _, d := range need {
		if _, ok := s.weights[d]; !ok {
			missing = append(missing, d)
		}
	}
	if len(missing) > 0 {
		got, err := s.src.SleeveWeights(equity, missing)
		if err != nil {
			log.Printf("[Stats] history: allocation-matched benchmark unavailable: %v", err)
			return nil
		}
		for d, w := range got {
			s.weights[d] = w // a finished close never changes
		}
	}
	ws := make([]Allocation, len(need))
	for i, d := range need {
		w, ok := s.weights[d]
		if !ok {
			return nil
		}
		ws[i] = w
	}
	return indexTo100(allocMatchedIndex(ws, benchVals[0], benchVals[1], benchVals[bilIdx]))
}

// baselineDate is the date of the baseline close, chosen as alignAll does: the
// last point before start, else the first point on or after it.
func baselineDate(bot []Point, start string) string {
	sorted := append([]Point(nil), bot...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Date < sorted[j].Date })
	base := ""
	for _, p := range sorted {
		if p.Date < start {
			base = p.Date
		}
	}
	if base == "" {
		for _, p := range sorted {
			if p.Date >= start {
				return p.Date
			}
		}
	}
	return base
}

// intradayRanges returns each tracked date's intraday range. Sessions that are
// over are fetched once and kept; today's, and any that could not be fetched,
// are asked for again. A failed fetch only degrades those days to close-only.
func (s *Service) intradayRanges(dates []string, now time.Time) map[string]ohlc {
	today := now.In(newYork).Format("2006-01-02")
	out := map[string]ohlc{}
	var need []string
	for _, d := range dates {
		if r, ok := s.days[d]; ok && d < today && (r.ok || now.Sub(r.at) < missingDayTTL) {
			out[d] = r
			continue
		}
		need = append(need, d)
	}
	if len(need) == 0 {
		return out
	}
	samples, err := s.src.IntradayEquity(need[0], need[len(need)-1])
	if err != nil {
		log.Printf("[Stats] history: intraday equity unavailable (%v); %d day(s) shown close-only", err, len(need))
	}
	got := rangesFrom(samples)
	for _, d := range need {
		r, ok := got[d]
		if !ok {
			r = ohlc{at: now}
		}
		out[d] = r
		if d < today && (ok || err == nil) {
			s.days[d] = r // a finished session never changes; a missing one is retried after missingDayTTL
		}
	}
	return out
}

// rangesFrom groups intraday samples by ET session date into open / high / low.
func rangesFrom(samples []Sample) map[string]ohlc {
	out := map[string]ohlc{}
	// Samples are not assumed to be ordered: the first by time is the open.
	first := map[string]time.Time{}
	for _, sm := range samples {
		if sm.Value <= 0 {
			continue
		}
		d := sm.At.In(newYork).Format("2006-01-02")
		r, seen := out[d]
		if !seen {
			out[d] = ohlc{o: sm.Value, h: sm.Value, l: sm.Value, ok: true}
			first[d] = sm.At
			continue
		}
		if sm.At.Before(first[d]) {
			r.o, first[d] = sm.Value, sm.At
		}
		r.h, r.l = max(r.h, sm.Value), min(r.l, sm.Value)
		out[d] = r
	}
	return out
}

// OriginPolicy returns which browser origins may read /stats/history: the
// owner's GitHub Pages site, an optional extra origin (CORS_EXTRA_ORIGIN, for a
// custom domain) and localhost on any port.
func OriginPolicy(extra string) func(origin string) bool {
	extra = normalizeOrigin(extra)
	return func(origin string) bool {
		o := normalizeOrigin(origin)
		if o == "" {
			return false
		}
		if o == "https://francocaccia.github.io" || (extra != "" && o == extra) {
			return true
		}
		u, err := url.Parse(o)
		if err != nil {
			return false
		}
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return u.Scheme == "http" || u.Scheme == "https"
		}
		return false
	}
}

// normalizeOrigin reduces an origin or URL to lower-case scheme://host[:port],
// or "" if it has none.
func normalizeOrigin(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// HistoryHandler serves GET /stats/history as a JSON array. CORS is limited to
// OriginPolicy; other origins get no Access-Control-Allow-Origin header.
func (s *Service) HistoryHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); origin != "" && s.allowOrigin(origin) {
			h.Set("Access-Control-Allow-Origin", origin)
		}
		switch r.Method {
		case http.MethodOptions:
			h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodGet:
		default:
			h.Set("Allow", "GET, OPTIONS")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		days, err := s.History()
		h.Set("Content-Type", "application/json")
		if err != nil {
			if !errors.Is(err, errRecentFailure) {
				log.Printf("[Stats] history failed: %v", err)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"history unavailable"}` + "\n"))
			return
		}
		h.Set("Cache-Control", "public, max-age=300")
		if days == nil {
			days = []HistoryDay{}
		}
		json.NewEncoder(w).Encode(days)
	})
}
