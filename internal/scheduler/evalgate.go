package scheduler

import (
	"fmt"
	"math"
	"time"
)

const (
	priceMoveTrigger = 0.02 // held: re-evaluate inside the cooldown after a >2% move
	rsiLow, rsiHigh  = 30.0, 70.0
	quietRSILow      = 40.0 // non-held symbols with RSI in [40, 60] and no news show no setup
	quietRSIHigh     = 60.0

	// entryReviewHour is the ET hour before which non-held symbols are not
	// evaluated: the first prints of the session are noisy.
	entryReviewHour = 10
)

var eastern = loadET()

func loadET() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.FixedZone("ET", -5*3600)
}

// evalInput is what the gate needs to know about a symbol this cycle.
type evalInput struct {
	Symbol    string
	Held      bool
	Price     float64
	RSI       float64
	Headlines []string // cached headlines that mention the symbol
	Now       time.Time
}

type evalRecord struct {
	at        time.Time
	price     float64
	rsi       float64
	headlines map[string]bool
}

// evalGate limits how often a symbol is sent to Gemini. Signals come from
// daily bars, so asking again every cycle mostly samples model noise.
//
//   - Held symbols: once per cooldown, unless the price moved >2% since the
//     last evaluation, RSI crossed 30/70, or a new headline mentions them.
//     Those exceptions are risk management for an open position.
//   - Non-held symbols (a possible new entry): at most once per trading day,
//     in the first eligible cycle at or after 10:00 ET. The exceptions above
//     never apply: they must not be able to trigger a new entry. Symbols that
//     show no setup (RSI in [40, 60], no news) are skipped without using the
//     day's evaluation.
//
// Not safe for concurrent use (the scheduler is single-threaded).
type evalGate struct {
	cooldown time.Duration
	last     map[string]evalRecord
}

func newEvalGate(cooldown time.Duration) *evalGate {
	return &evalGate{cooldown: cooldown, last: map[string]evalRecord{}}
}

func crossed(prev, cur, level float64) bool { return (prev-level)*(cur-level) < 0 }

func (r evalRecord) hasNews(headlines []string) bool {
	for _, h := range headlines {
		if !r.headlines[h] {
			return true
		}
	}
	return false
}

// Check reports whether symbol should be sent to Gemini now. skip is
// "cooldown", "daily", "before 10:00" or "no setup" when ok is false; why
// names the trigger when ok is true.
func (g *evalGate) Check(in evalInput) (ok bool, skip, why string) {
	rec, seen := g.last[in.Symbol]
	newNews := rec.hasNews(in.Headlines)

	if !in.Held {
		now := in.Now.In(eastern)
		switch {
		case now.Hour() < entryReviewHour:
			return false, "before 10:00", ""
		case seen && rec.at.In(eastern).Format("2006-01-02") == now.Format("2006-01-02"):
			return false, "daily", ""
		case !newNews && in.RSI >= quietRSILow && in.RSI <= quietRSIHigh:
			return false, "no setup", ""
		}
		return true, "", "daily review"
	}

	var trigger string
	switch {
	case !seen:
		trigger = "first evaluation"
	case newNews:
		trigger = "new news"
	case rec.price > 0 && math.Abs(in.Price-rec.price)/rec.price > priceMoveTrigger:
		trigger = fmt.Sprintf("price moved %+.1f%%", (in.Price-rec.price)/rec.price*100)
	case crossed(rec.rsi, in.RSI, rsiLow) || crossed(rec.rsi, in.RSI, rsiHigh):
		trigger = fmt.Sprintf("RSI crossed 30/70 (%.0f -> %.0f)", rec.rsi, in.RSI)
	case in.Now.Sub(rec.at) >= g.cooldown:
		trigger = "cooldown elapsed"
	default:
		return false, "cooldown", ""
	}
	return true, "", trigger
}

// Record notes that symbol was just sent to Gemini.
func (g *evalGate) Record(in evalInput) {
	h := make(map[string]bool, len(in.Headlines))
	for _, s := range in.Headlines {
		h[s] = true
	}
	g.last[in.Symbol] = evalRecord{at: in.Now, price: in.Price, rsi: in.RSI, headlines: h}
}
