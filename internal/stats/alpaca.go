package stats

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/marketdata"
)

// AlpacaSource implements Source on the Alpaca paper API and IEX market data.
type AlpacaSource struct {
	Broker     *broker.AlpacaClient
	MarketData *marketdata.Client
}

const benchmarkSymbol = "SPY"

// EquityHistory returns end-of-day account equity covering at least the last days days.
func (a AlpacaSource) EquityHistory(days int) ([]Point, error) {
	period := fmt.Sprintf("%dD", days)
	if days > 365 {
		period = fmt.Sprintf("%dA", (days+364)/365)
	}
	data, err := a.Broker.GetPortfolioHistory(period)
	if err != nil {
		return nil, err
	}
	var h struct {
		Timestamp []int64    `json:"timestamp"`
		Equity    []*float64 `json:"equity"`
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	var out []Point
	for i, ts := range h.Timestamp {
		if i >= len(h.Equity) || h.Equity[i] == nil || *h.Equity[i] <= 0 {
			continue
		}
		out = append(out, Point{Date: time.Unix(ts, 0).In(newYork).Format("2006-01-02"), Value: *h.Equity[i]})
	}
	return dedupe(out), nil
}

// BenchmarkCloses returns SPY daily closes from start (YYYY-MM-DD) onward.
func (a AlpacaSource) BenchmarkCloses(start string) ([]Point, error) {
	bars, err := a.MarketData.DailyBars(benchmarkSymbol, start)
	if err != nil {
		return nil, err
	}
	out := make([]Point, 0, len(bars))
	for _, b := range bars {
		out = append(out, Point{Date: b.Date, Value: b.Close})
	}
	return dedupe(out), nil
}

// FilledOrders returns orders that filled on or after start (YYYY-MM-DD).
func (a AlpacaSource) FilledOrders(start string) ([]Order, error) {
	after := start + "T00:00:00Z"
	seen := map[string]bool{}
	var out []Order
	for page := 0; page < 10; page++ {
		data, err := a.Broker.GetClosedOrders(after)
		if err != nil {
			return nil, err
		}
		var raw []struct {
			ID             string  `json:"id"`
			Symbol         string  `json:"symbol"`
			Side           string  `json:"side"`
			Status         string  `json:"status"`
			FilledQty      string  `json:"filled_qty"`
			FilledAvgPrice *string `json:"filled_avg_price"`
			FilledAt       *string `json:"filled_at"`
			SubmittedAt    string  `json:"submitted_at"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			if r.Status != "filled" || r.FilledAt == nil || r.FilledAvgPrice == nil {
				continue
			}
			qty, _ := strconv.ParseFloat(r.FilledQty, 64)
			price, _ := strconv.ParseFloat(*r.FilledAvgPrice, 64)
			at, err := time.Parse(time.RFC3339Nano, *r.FilledAt)
			if err != nil || qty <= 0 || price <= 0 {
				continue
			}
			out = append(out, Order{Symbol: r.Symbol, Side: r.Side, Qty: qty, Price: price, FilledAt: at})
		}
		if len(raw) < 500 {
			break
		}
		after = raw[len(raw)-1].SubmittedAt
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FilledAt.Before(out[j].FilledAt) })
	return out, nil
}

// OpenPositions returns how many positions are currently open.
func (a AlpacaSource) OpenPositions() (int, error) {
	data, err := a.Broker.GetPositions()
	if err != nil {
		return 0, err
	}
	var ps []json.RawMessage
	if err := json.Unmarshal(data, &ps); err != nil {
		return 0, err
	}
	return len(ps), nil
}

// dedupe sorts by date and keeps the last value for each date.
func dedupe(pts []Point) []Point {
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].Date < pts[j].Date })
	var out []Point
	for _, p := range pts {
		if n := len(out); n > 0 && out[n-1].Date == p.Date {
			out[n-1] = p
			continue
		}
		out = append(out, p)
	}
	return out
}
