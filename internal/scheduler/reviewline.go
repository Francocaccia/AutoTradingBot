package scheduler

import (
	"fmt"
	"log"
	"strings"

	"github.com/francocaccia/quantengine/internal/market"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

// Every cycle ends with exactly one [Review] line, so a quiet cycle is never a
// silent one:
//
//	[Review] ts=2026-10-16 10:15 EDT | regime=risk-off, inverted curve |
//	  equities 28.2% (band 40-75%, below min) | bonds 0.0% (band 15-40%, below min) |
//	  cash 71.8% (band 5-30%, above max) | mode=gradual | action=none |
//	  skip=rebalance: one rebalance trade per day already placed
//
// action is none, or any of rebalance, buy and sell joined with "+"; skip says
// why nothing happened and is "-" when something did.

const noSkip = "-"

// sleeveStatus places a weight against its band.
func sleeveStatus(w, lo, hi float64) string {
	switch {
	case w < lo:
		return "below min"
	case w > hi:
		return "above max"
	}
	return "in band"
}

// allocationText renders the three sleeves against their bands, or says the
// allocation is unavailable.
func (s *Scheduler) allocationText(cs *cycleState) string {
	if cs == nil || cs.account == nil || cs.account.Equity <= 0 {
		return "allocation unavailable"
	}
	w := s.bookFrom(cs, cs.account).SleeveWeights()
	parts := make([]string, 0, 3)
	for _, sleeve := range []string{portfolio.SleeveEquities, portfolio.SleeveBonds, portfolio.SleeveCash} {
		lo, hi := s.policy.Band(sleeve)
		parts = append(parts, fmt.Sprintf("%s %.1f%% (band %.0f-%.0f%%, %s)", sleeve, w[sleeve], lo, hi, sleeveStatus(w[sleeve], lo, hi)))
	}
	return strings.Join(parts, " | ")
}

// reviewLine builds the cycle's [Review] line. sess may be nil (the cycle ended
// before the session was known); the clock then supplies the timestamp.
func (s *Scheduler) reviewLine(cs *cycleState, sess *market.Session) string {
	ts := s.clock()
	if sess != nil && !sess.Now.IsZero() {
		ts = sess.Now
	}
	regimeLabel := "unavailable"
	if cs != nil && cs.pf != nil && cs.pf.regime.Label != "" {
		regimeLabel = cs.pf.regime.Label
	}
	action, skip := "none", noSkip
	if cs != nil && len(cs.did) > 0 {
		action = strings.Join(orderedActions(cs.did), "+")
	} else {
		skip = "no signal to act on"
		if cs != nil && len(cs.whyNot) > 0 {
			skip = strings.Join(cs.whyNot, "; ")
		}
	}
	return fmt.Sprintf("[Review] ts=%s | regime=%s | %s | mode=%s | action=%s | skip=%s",
		ts.In(eastern).Format("2006-01-02 15:04 MST"), regimeLabel, s.allocationText(cs), s.bandsMode(), action, skip)
}

// orderedActions lists the actions in a fixed order: rebalance, buy, sell.
func orderedActions(did []string) []string {
	var out []string
	for _, a := range []string{"rebalance", "buy", "sell"} {
		for _, d := range did {
			if d == a {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// logReviewLine writes the cycle's single [Review] line.
func (s *Scheduler) logReviewLine(cs *cycleState, sess *market.Session) {
	log.Print(s.reviewLine(cs, sess))
}

// logEarlyReview writes the [Review] line of a cycle that ended before the
// account was loaded (scheduler paused, market closed, an upstream call
// failed). The allocation is read best-effort and left out if it cannot be.
func (s *Scheduler) logEarlyReview(sess *market.Session, why string) {
	cs := &cycleState{positions: map[string]*portfolio.Position{}}
	if s.acct != nil && s.pos != nil && why != "account unavailable" && why != "positions unavailable" {
		acct, err := s.acct.GetAccount()
		if err == nil && acct != nil {
			if list, perr := s.pos.GetAllPositions(); perr == nil {
				cs.account = acct
				for i := range list {
					p := list[i]
					cs.positions[p.Symbol] = &p
				}
			}
		}
	}
	cs.why(why)
	s.logReviewLine(cs, sess)
}
