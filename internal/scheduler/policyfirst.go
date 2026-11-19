package scheduler

import (
	"fmt"
	"strings"

	"github.com/francocaccia/quantengine/internal/portfolio"
)

// policyFirstReason says why the model's single-stock BUYs must wait ("" = they
// may go ahead). The policy comes first: while an equity or bond sleeve is under
// its floor, or cash is over its maximum, the bot builds the index-ETF core (see
// planRebalance) and does not add stock picks on top of a book that is still
// mostly cash. It only applies in gradual mode: advisory and off modes do not
// build the core, so holding the stock picks back would freeze the bot.
func (s *Scheduler) policyFirstReason(cs *cycleState) string {
	if !s.cfg.PolicyFirst || s.bandsMode() != bandsGradual || cs.account == nil || cs.account.Equity <= 0 {
		return ""
	}
	w := s.bookFrom(cs, cs.account).SleeveWeights()
	var parts []string
	for _, sleeve := range []string{portfolio.SleeveEquities, portfolio.SleeveBonds} {
		if lo, _ := s.policy.Band(sleeve); lo-w[sleeve] >= minDeficitPts-1e-9 {
			parts = append(parts, fmt.Sprintf("%s %.1f%% is under its %.0f%% floor", sleeve, w[sleeve], lo))
		}
	}
	if _, hi := s.policy.Band(portfolio.SleeveCash); w[portfolio.SleeveCash]-hi >= minDeficitPts-1e-9 {
		parts = append(parts, fmt.Sprintf("cash %.1f%% is over its %.0f%% maximum", w[portfolio.SleeveCash], hi))
	}
	if len(parts) == 0 {
		return ""
	}
	return "policy first: " + strings.Join(parts, ", ") + "; single-stock BUYs wait until the index-ETF core is built"
}
