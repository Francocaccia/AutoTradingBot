package stats

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/francocaccia/quantengine/internal/portfolio"
)

// The allocation-matched benchmark answers "what would SPY, AGG and BIL have
// returned with the bot's own equity / bond / cash weights?". It separates what
// the allocation cost (a policy that holds bonds and cash lags an index in a
// rising market) from what stock selection and trading added. It needs the
// weights at each close, which Alpaca does not keep: they are rebuilt from the
// current positions by undoing the filled orders after each date.

// weightsFromHoldings splits equity into sleeve weights (percent) from the
// quantity and price of each symbol. Cash is whatever the securities do not
// explain, so T-bill ETFs and real cash are both in the cash sleeve. Weights are
// kept inside [0, 100] and sum to 100.
func weightsFromHoldings(equity float64, qty, price map[string]float64) Allocation {
	if equity <= 0 {
		return Allocation{}
	}
	var eq, bond float64
	for sym, q := range qty {
		p, ok := price[sym]
		if !ok || q == 0 {
			continue
		}
		switch portfolio.Sleeve(sym) {
		case portfolio.SleeveEquities:
			eq += q * p
		case portfolio.SleeveBonds:
			bond += q * p
		}
	}
	eq, bond = max(eq, 0), max(bond, 0)
	if total := eq + bond; total > equity {
		eq, bond = eq*equity/total, bond*equity/total
	}
	return Allocation{
		Equities: eq / equity * 100,
		Bonds:    bond / equity * 100,
		Cash:     (equity - eq - bond) / equity * 100,
	}
}

// allocMatchedIndex applies the weights held at the previous close to the day's
// SPY, AGG and BIL returns and chains them from 100. The slices share an index;
// weights[i] is the allocation at the close before day i+1.
func allocMatchedIndex(weights []Allocation, spy, agg, bil []float64) []float64 {
	out := make([]float64, len(spy))
	if len(spy) == 0 {
		return out
	}
	out[0] = 100
	for i := 1; i < len(spy); i++ {
		w := weights[i-1]
		r := w.Equities/100*(spy[i]/spy[i-1]-1) + w.Bonds/100*(agg[i]/agg[i-1]-1) + w.Cash/100*(bil[i]/bil[i-1]-1)
		out[i] = out[i-1] * (1 + r)
	}
	return out
}

// SleeveWeights rebuilds the sleeve weights at the close of each of dates, from
// the current positions and the orders filled after that date. equity gives the
// account value at each close.
func (a AlpacaSource) SleeveWeights(equity []Point, dates []string) (map[string]Allocation, error) {
	if len(dates) == 0 {
		return map[string]Allocation{}, nil
	}
	sorted := append([]string(nil), dates...)
	sort.Strings(sorted)
	first := sorted[0]

	posData, err := a.Broker.GetPositions()
	if err != nil {
		return nil, err
	}
	var positions []struct {
		Symbol string `json:"symbol"`
		Qty    string `json:"qty"`
	}
	if err := json.Unmarshal(posData, &positions); err != nil {
		return nil, err
	}
	now := map[string]float64{}
	for _, p := range positions {
		q, _ := strconv.ParseFloat(p.Qty, 64)
		now[p.Symbol] = q
	}
	// Start well before the first date: a stop or take-profit leg is submitted with
	// its entry, possibly weeks before it fills.
	firstDay, _ := time.Parse("2006-01-02", first)
	orders, err := a.FilledOrders(firstDay.AddDate(0, 0, -60).Format("2006-01-02"))
	if err != nil {
		return nil, err
	}

	symbols := map[string]bool{}
	for sym := range now {
		symbols[sym] = true
	}
	for _, o := range orders {
		symbols[o.Symbol] = true
	}
	lookback := func() string {
		t, _ := time.Parse("2006-01-02", first)
		return t.AddDate(0, 0, -10).Format("2006-01-02")
	}()
	closes := map[string][]Point{}
	for sym := range symbols {
		pts, err := a.Closes(sym, lookback)
		if err != nil {
			log.Printf("[Stats] allocation-matched benchmark: no closes for one holding (%v); its value counts as cash", err)
			continue
		}
		closes[sym] = pts
	}
	eqAt := map[string]float64{}
	for _, p := range equity {
		eqAt[p.Date] = p.Value
	}

	out := map[string]Allocation{}
	for _, d := range sorted {
		e, ok := eqAt[d]
		if !ok {
			return nil, fmt.Errorf("no account value for %s", d)
		}
		qty := map[string]float64{}
		for sym, q := range now {
			qty[sym] = q
		}
		for _, o := range orders {
			if o.FilledAt.In(newYork).Format("2006-01-02") <= d {
				continue
			}
			// Undo a fill that happened after the close of d.
			if o.Side == "buy" {
				qty[o.Symbol] -= o.Qty
			} else {
				qty[o.Symbol] += o.Qty
			}
		}
		price := map[string]float64{}
		for sym, pts := range closes {
			for _, p := range pts {
				if p.Date <= d {
					price[sym] = p.Value
				}
			}
		}
		out[d] = weightsFromHoldings(e, qty, price)
	}
	return out, nil
}
