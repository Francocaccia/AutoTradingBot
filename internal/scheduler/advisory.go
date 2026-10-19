package scheduler

import (
	"fmt"
	"log"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

// In advisory mode (IPS_BANDS_MODE=advisory) nothing driven by the investment
// policy is submitted: not the rebalancing tranche (planRebalance never queues
// one) and not a BUY the weekly review queued. Such a BUY is run through every
// guard and sizing step against a stand-in broker that sends nothing, reported as
// "would BUY ... (advisory, not submitted)", and every side effect on the cycle
// (trade counters, positions, actions) is undone, so it cannot use up the trade
// caps that discretionary orders share.

// dryOrders is an orderPlacer that places nothing and cancels nothing.
type dryOrders struct{}

func (dryOrders) PlaceBracketOrder(string, int, broker.Side, float64, float64, string) error {
	return nil
}
func (dryOrders) PlaceOTOOrder(string, int, broker.Side, float64, string) error { return nil }
func (dryOrders) PlaceMarketOrder(string, int, broker.Side, string) error       { return nil }
func (dryOrders) CancelOpenOrders(string) (int, error)                          { return 0, nil }
func (dryOrders) CheckOrder(string, broker.Side, string) error                  { return nil }

// logBuy logs a placed BUY, or, during an advisory dry run, the BUY that would
// have been placed.
func (s *Scheduler) logBuy(symbol, detail string) {
	if s.dry {
		log.Printf("[%s] would BUY %s (advisory, not submitted)", symbol, detail)
		return
	}
	log.Printf("[%s] BUY %s", symbol, detail)
}

// adviseBuy runs a review BUY as a dry run and restores the cycle's state.
func (s *Scheduler) adviseBuy(p pendingDecision, cs *cycleState) {
	day, week, did := cs.tradesDay, cs.tradesWeek, append([]string(nil), cs.did...)
	saved := make(map[string]portfolio.Position, len(cs.positions))
	for sym, pos := range cs.positions {
		saved[sym] = *pos
	}
	sold := make(map[string]bool, len(cs.sold))
	for sym, v := range cs.sold {
		sold[sym] = v
	}

	real := s.orders
	s.orders, s.dry = dryOrders{}, true
	s.execBuy(p, cs)
	placed := cs.tradesDay > day // a placed order is what raises the counter
	s.orders, s.dry = real, false

	// Undo what a placed order would have changed.
	cs.tradesDay, cs.tradesWeek, cs.did, cs.sold = day, week, did, sold
	for sym := range cs.positions {
		if _, ok := saved[sym]; !ok {
			delete(cs.positions, sym)
		}
	}
	for sym, pos := range saved {
		if cur, ok := cs.positions[sym]; ok {
			*cur = pos
		} else {
			restored := pos
			cs.positions[sym] = &restored
		}
	}
	if placed {
		cs.why(fmt.Sprintf("advisory: would BUY %s (review), not submitted", p.symbol))
	}
}
