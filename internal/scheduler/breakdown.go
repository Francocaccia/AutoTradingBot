package scheduler

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/francocaccia/quantengine/internal/trades"
)

// logDecisionBreakdown logs, once at startup, exactly which filled orders the
// daily/weekly trade counters count, so the numbers can be checked by eye.
func (s *Scheduler) logDecisionBreakdown(now time.Time) {
	if s.cfg.History == nil {
		return
	}
	snap, err := trades.Load(s.cfg.History, now)
	if err != nil {
		log.Printf("[Trades] startup breakdown unavailable: order history failed: %v", err)
		return
	}
	snap.SetCountStart(s.cfg.GuardsStartDate)
	for _, line := range breakdownLines(snap) {
		log.Print(line)
	}
}

// breakdownLines renders the counted decisions (symbol, side, time,
// client_order_id) and what was left out of the counters.
func breakdownLines(snap *trades.Snapshot) []string {
	b := snap.DecisionBreakdown()
	today, week := snap.DecisionCounts()
	from := b.CountFrom
	if from == "" {
		from = "unset"
	}
	lines := []string{fmt.Sprintf("[Trades] startup breakdown: counted today=%d, rolling-5-trading-days (since %s)=%d; GUARDS_START_DATE=%s",
		today, b.WeekStart, week, from)}
	for _, f := range b.Counted {
		id := f.ClientOrderID
		if !strings.HasPrefix(id, "atb-") {
			id = "(legacy, no atb- prefix)"
		}
		lines = append(lines, fmt.Sprintf("[Trades]   counted: %-5s %-4s %s %s", f.Symbol, f.Side, trades.FormatET(f.At), id))
	}
	if len(b.Excluded) > 0 {
		keys := make([]string, 0, len(b.Excluded))
		for k := range b.Excluded {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%s x%d", k, b.Excluded[k])
		}
		lines = append(lines, "[Trades]   not counted: "+strings.Join(parts, ", "))
	}
	return lines
}
