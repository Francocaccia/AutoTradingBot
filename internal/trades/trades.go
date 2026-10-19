// Package trades derives the bot's trading history from Alpaca's filled
// orders. Render restarts wipe memory and disk, so position age, re-entry
// cooldowns and turnover counts are all rebuilt from the broker, never from
// local state.
package trades

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
)

// Lookback is how far back order history is read. A position older than this
// (with no order inside the window) has an unknown age.
const Lookback = 45 * 24 * time.Hour

var eastern = loadET()

func loadET() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.FixedZone("ET", -5*3600)
}

// Fill is one filled order.
type Fill struct {
	Symbol string
	Side   string // "buy" or "sell"
	Type   string // order type: market, limit, stop, ...
	At     time.Time
	Qty    float64

	ClientOrderID string // "atb-..." on orders the bot submitted itself
	OrderClass    string // "", simple, bracket, oto, ...
	ParentID      string // set on bracket / OTO child legs
}

// Kind is the kind encoded in the client_order_id (broker.KindEntry, KindExit
// or KindRebalance), "" for legacy ids and orders that are not the bot's.
func (f Fill) Kind() string {
	k, _ := broker.ParseClientOrderID(f.ClientOrderID)
	return k
}

// IsDecision reports whether the fill came from one of the bot's own
// decisions rather than a bracket leg, a stop/limit fill or a manual order.
//
//   - Child legs (ParentID set) never count.
//   - An order whose client_order_id carries broker.ClientOrderPrefix counts.
//   - A legacy order without the prefix counts only when it is a parent order
//     (class "", simple, bracket or oto) shaped like one of the bot's decisions:
//     a market buy, or a plain market sell. Stop and limit fills are exits.
func (f Fill) IsDecision() bool {
	if f.ParentID != "" {
		return false
	}
	if strings.HasPrefix(f.ClientOrderID, broker.ClientOrderPrefix) {
		return true
	}
	switch f.OrderClass {
	case "", "simple":
		return (f.Side == "buy" || f.Side == "sell") && f.Type == "market"
	case "bracket", "oto":
		// The bot only opens bracket / OTO orders (buys); a sell in these
		// classes is an exit leg even if the parent link is missing.
		return f.Side == "buy" && f.Type == "market"
	}
	return false
}

// countKind labels why a fill was or was not counted, for the startup log.
func (f Fill) exclusionReason() string {
	switch {
	case f.ParentID != "":
		return "bracket/OTO leg"
	case f.Type == "stop" || f.Type == "stop_limit" || f.Type == "limit" || f.Type == "trailing_stop":
		return "stop/limit fill"
	default:
		return "not a bot decision"
	}
}

// Source supplies order history and the exchange calendar.
type Source interface {
	// Fills returns filled orders from since onward.
	Fills(since time.Time) ([]Fill, error)
	// TradingDays returns the trading dates (YYYY-MM-DD) between start and end,
	// inclusive. An error makes callers fall back to weekdays.
	TradingDays(start, end time.Time) ([]string, error)
}

// FormatET formats t as an Eastern-time timestamp for logs.
func FormatET(t time.Time) string { return t.In(eastern).Format("2006-01-02 15:04 MST") }

// Date formats t as an Eastern-time calendar date.
func Date(t time.Time) string { return t.In(eastern).Format("2006-01-02") }

// Snapshot is the order history and trading calendar as of one moment.
type Snapshot struct {
	fills     []Fill   // oldest first
	days      []string // trading dates, ascending, ending at the last one <= today
	today     string
	countFrom string // ET date (YYYY-MM-DD); decisions before it do not count toward the caps
}

// SetCountStart makes decisions dated before date (YYYY-MM-DD, Eastern) stop
// counting toward the daily and weekly caps (GUARDS_START_DATE). "" counts all.
func (s *Snapshot) SetCountStart(date string) { s.countFrom = date }

// Load reads history from src as of now. A fill-history failure is returned;
// a calendar failure silently falls back to weekdays.
func Load(src Source, now time.Time) (*Snapshot, error) {
	fills, err := src.Fills(now.Add(-Lookback))
	if err != nil {
		return nil, err
	}
	days, err := src.TradingDays(now.Add(-Lookback), now)
	if err != nil || len(days) == 0 {
		days = Weekdays(now.Add(-Lookback), now)
	}
	return NewSnapshot(fills, days, now), nil
}

// NewSnapshot builds a Snapshot from fills (any order) and ascending trading
// dates. Days after today are ignored.
func NewSnapshot(fills []Fill, days []string, now time.Time) *Snapshot {
	s := &Snapshot{today: Date(now)}
	s.fills = append([]Fill(nil), fills...)
	sort.SliceStable(s.fills, func(i, j int) bool { return s.fills[i].At.Before(s.fills[j].At) })
	for _, d := range days {
		if d <= s.today {
			s.days = append(s.days, d)
		}
	}
	return s
}

// Weekdays lists Monday-Friday dates between start and end, inclusive. It is
// the fallback when Alpaca's calendar is unavailable (holidays are not known).
func Weekdays(start, end time.Time) []string {
	var out []string
	last := Date(end)
	for d := start.In(eastern); ; d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		if ds > last {
			break
		}
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, ds)
		}
	}
	return out
}

// daysAfter counts trading days after date up to and including today.
func (s *Snapshot) daysAfter(date string) int {
	n := 0
	for _, d := range s.days {
		if d > date {
			n++
		}
	}
	return n
}

// PositionAge returns the trading days since the currently open position in
// symbol was opened (0 = opened today). ok is false when no opening fill is
// in the history window, e.g. a legacy position: the age is unknown.
func (s *Snapshot) PositionAge(symbol string) (days int, ok bool) {
	net := 0.0
	var opened time.Time
	for _, f := range s.fills {
		if f.Symbol != symbol {
			continue
		}
		if f.Side == "buy" {
			if net <= 0 {
				opened = f.At
			}
			net += f.Qty
		} else {
			net -= f.Qty
			if net <= 1e-9 {
				net = 0
				opened = time.Time{}
			}
		}
	}
	if opened.IsZero() {
		return 0, false
	}
	return s.daysAfter(Date(opened)), true
}

// SinceLastSell returns the trading days since the most recent sell fill in
// symbol (0 = today); ok is false when there is none in the window. Stop and
// take-profit fills count: re-buying a name right after it was stopped out is
// the same churn.
func (s *Snapshot) SinceLastSell(symbol string) (days int, ok bool) {
	for i := len(s.fills) - 1; i >= 0; i-- {
		if f := s.fills[i]; f.Symbol == symbol && f.Side == "sell" {
			return s.daysAfter(Date(f.At)), true
		}
	}
	return 0, false
}

// WeekWindow is the number of trading days (today included) in the rolling
// weekly turnover window.
const WeekWindow = 5

// weekStart is the first trading day of the rolling WeekWindow.
func (s *Snapshot) weekStart() string {
	if n := len(s.days); n > 0 {
		i := n - WeekWindow
		if i < 0 {
			i = 0
		}
		return s.days[i]
	}
	return s.today
}

// counts reports whether f counts toward the caps and on which windows.
func (s *Snapshot) counts(f Fill) (counted, today, week bool) {
	if !f.IsDecision() {
		return false, false, false
	}
	d := Date(f.At)
	if d < s.countFrom {
		return false, false, false
	}
	today = d == s.today
	week = d >= s.weekStart() && d <= s.today
	return today || week, today, week
}

// DecisionCounts returns how many of the bot's own orders (see IsDecision)
// filled today and within the last WeekWindow trading days, ignoring any
// before the count start (SetCountStart).
func (s *Snapshot) DecisionCounts() (today, week int) {
	for _, f := range s.fills {
		_, t, w := s.counts(f)
		if t {
			today++
		}
		if w {
			week++
		}
	}
	return today, week
}

// RebalanceCounts returns how many of the bot's policy rebalancing orders (kind
// "rebalance", see broker.KindRebalance) filled today and within the last
// WeekWindow trading days. They are also decisions, so DecisionCounts includes them.
func (s *Snapshot) RebalanceCounts() (today, week int) {
	for _, f := range s.fills {
		if f.ParentID != "" || f.Kind() != broker.KindRebalance {
			continue
		}
		d := Date(f.At)
		if d == s.today {
			today++
		}
		if d >= s.weekStart() && d <= s.today {
			week++
		}
	}
	return today, week
}

// Breakdown explains DecisionCounts: the fills that were counted and how many
// others in the same window were left out, by reason.
type Breakdown struct {
	WeekStart string
	CountFrom string
	Counted   []Fill
	Excluded  map[string]int
}

// DecisionBreakdown lists what DecisionCounts counted over the weekly window.
func (s *Snapshot) DecisionBreakdown() Breakdown {
	b := Breakdown{WeekStart: s.weekStart(), CountFrom: s.countFrom, Excluded: map[string]int{}}
	for _, f := range s.fills {
		d := Date(f.At)
		if d < b.WeekStart || d > s.today {
			continue
		}
		if counted, _, _ := s.counts(f); counted {
			b.Counted = append(b.Counted, f)
		} else if f.IsDecision() {
			b.Excluded["before GUARDS_START_DATE"]++
		} else {
			b.Excluded[f.exclusionReason()]++
		}
	}
	return b
}

// AlpacaSource implements Source on the Alpaca paper API.
type AlpacaSource struct {
	Broker *broker.AlpacaClient
}

func (a AlpacaSource) Fills(since time.Time) ([]Fill, error) {
	after := since.UTC().Format(time.RFC3339)
	seen := map[string]bool{}
	var out []Fill
	for page := 0; page < 10; page++ {
		data, err := a.Broker.GetClosedOrders(after)
		if err != nil {
			return nil, err
		}
		var raw []struct {
			ID            string  `json:"id"`
			ClientOrderID string  `json:"client_order_id"`
			OrderClass    string  `json:"order_class"`
			ParentOrderID string  `json:"parent_order_id"`
			Symbol        string  `json:"symbol"`
			Side          string  `json:"side"`
			Type          string  `json:"type"`
			Status        string  `json:"status"`
			FilledQty     string  `json:"filled_qty"`
			FilledAt      *string `json:"filled_at"`
			SubmittedAt   string  `json:"submitted_at"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			if r.Status != "filled" || r.FilledAt == nil {
				continue
			}
			qty, _ := strconv.ParseFloat(r.FilledQty, 64)
			at, err := time.Parse(time.RFC3339Nano, *r.FilledAt)
			if err != nil || qty <= 0 {
				continue
			}
			out = append(out, Fill{Symbol: r.Symbol, Side: r.Side, Type: r.Type, At: at, Qty: qty,
				ClientOrderID: r.ClientOrderID, OrderClass: r.OrderClass, ParentID: r.ParentOrderID})
		}
		if len(raw) < 500 {
			break
		}
		after = raw[len(raw)-1].SubmittedAt
	}
	return out, nil
}

func (a AlpacaSource) TradingDays(start, end time.Time) ([]string, error) {
	data, err := a.Broker.GetCalendarRange(Date(start), Date(end))
	if err != nil {
		return nil, err
	}
	var cal []struct {
		Date string `json:"date"`
	}
	if err := json.Unmarshal(data, &cal); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cal))
	for _, c := range cal {
		out = append(out, c.Date)
	}
	sort.Strings(out)
	return out, nil
}
