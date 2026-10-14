package scheduler

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/francocaccia/quantengine/internal/analysis"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/market"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

// maxReviewActions is how many of the review's recommended actions are considered.
const maxReviewActions = 2

// reviewReason says why a portfolio review should run now ("" = not now). It
// runs on the first eligible cycle on Monday after 10:00 ET, or once a day when
// a sleeve is more than reviewBreachPts outside its band. Cash inside its band
// is never a reason.
func (s *Scheduler) reviewReason(cs *cycleState, sess *market.Session) string {
	reason, _ := s.reviewDecision(cs, sess)
	return reason
}

// reviewDecision is reviewReason plus, when the review is not due, why not.
func (s *Scheduler) reviewDecision(cs *cycleState, sess *market.Session) (reason, notDue string) {
	switch {
	case cs.pf == nil:
		return "", "portfolio context unavailable"
	case sess.Now.Hour() < entryReviewHour:
		return "", fmt.Sprintf("not due: before %d:00 ET", entryReviewHour)
	// At most one review per process per day, whatever triggered it.
	case s.lastReviewDay == sess.Date:
		return "", "already ran today (weekly review)"
	case s.lastBandReviewDay == sess.Date:
		return "", "already ran today (band-breach review)"
	case sess.Now.Weekday() == time.Monday:
		return "weekly review", ""
	}
	pts, desc := s.policy.BandBreach(s.bookFrom(cs, cs.account))
	if pts > reviewBreachPts {
		return fmt.Sprintf("band breach of %.1f points (%s)", pts, desc), ""
	}
	return "", fmt.Sprintf("not due: not Monday and the largest band breach is %.1f points (trigger: more than %.0f)", pts, reviewBreachPts)
}

// reviewSkipLine is the one line logged on every cycle the review does not run:
// the reason, and where cash stands against its band when that is known.
func (s *Scheduler) reviewSkipLine(why string, cashPct float64, haveCash bool) string {
	line := "[Review] not run: " + why
	if haveCash {
		line += fmt.Sprintf(" | cash %.1f%% of equity, band %.0f-%.0f%%", cashPct, s.policy.CashMin, s.policy.CashMax)
	}
	return line
}

// logReviewSkip logs the skip line using the cycle's cash sleeve weight.
func (s *Scheduler) logReviewSkip(cs *cycleState, why string) {
	if cs != nil && cs.account != nil && cs.account.Equity > 0 {
		w := s.bookFrom(cs, cs.account).SleeveWeights()[portfolio.SleeveCash]
		log.Print(s.reviewSkipLine(why, w, true))
		return
	}
	log.Print(s.reviewSkipLine(why, 0, false))
}

// logEarlySkip logs the review skip line for a cycle that ended before the
// account was loaded (market closed, scheduler paused, Gemini paused). Cash is
// read from a best-effort account fetch and left out if that fails.
func (s *Scheduler) logEarlySkip(why string) {
	if s.acct != nil {
		if acct, err := s.acct.GetAccount(); err == nil && acct != nil && acct.Equity > 0 {
			log.Print(s.reviewSkipLine(why, acct.Cash/acct.Equity*100, true))
			return
		}
	}
	log.Print(s.reviewSkipLine(why, 0, false))
}

// maybeReview runs the portfolio review (one Gemini call) when due and queues
// its approved actions as ordinary decisions, which then go through every guard.
// Every cycle in which it does not run logs one line saying why.
func (s *Scheduler) maybeReview(cs *cycleState, sess *market.Session) {
	reason, notDue := s.reviewDecision(cs, sess)
	if reason == "" {
		s.logReviewSkip(cs, notDue)
		return
	}
	if cs.stopGemini {
		s.logReviewSkip(cs, reason+" is due, but Gemini quota/budget ran out earlier this cycle")
		return
	}
	// A review whose orders would all be refused by the turnover caps is not worth a call.
	if !cs.breaker && cs.history != nil && !s.capRoom(cs, 1) {
		s.logReviewSkip(cs, fmt.Sprintf("%s is due, but the trade cap is reached (today %d/%s, week %d/%s)",
			reason, cs.tradesDay, capLabel(s.cfg.MaxTradesPerDay), cs.tradesWeek, capLabel(s.cfg.MaxTradesPerWeek)))
		return
	}
	// Never spend a call (or mark the day as done) while Gemini is paused: the
	// review runs on the first cycle after it resumes.
	pausedFn := s.geminiPaused
	if pausedFn == nil && s.cfg.Gemini != nil {
		pausedFn = s.cfg.Gemini.Paused
	}
	if pausedFn == nil || s.llm == nil {
		s.logReviewSkip(cs, reason+" is due, but no Gemini client is configured")
		return
	}
	if paused, why := pausedFn(); paused {
		s.logReviewSkip(cs, fmt.Sprintf("%s is due, but Gemini is paused (%s)", reason, why))
		return
	}
	// Mark the day before calling so a failing call is not retried every cycle.
	if strings.HasPrefix(reason, "weekly") {
		s.lastReviewDay = sess.Date
	} else {
		s.lastBandReviewDay = sess.Date
	}

	block := analysis.Render(cs.pf.in, nil)
	var options []gemini.BondOption
	for _, b := range s.cfg.Bonds.Infos() {
		if !cs.regimeAvailable() && !portfolio.SafeWithoutMacro[b.Symbol] {
			continue // without macro data only SGOV, BIL and SHY may be bought
		}
		options = append(options, gemini.BondOption{Symbol: b.Symbol, Kind: b.Kind, Duration: b.Duration, Credit: b.Credit, CashLike: b.CashLike})
	}
	prompt := gemini.BuildReviewPrompt(gemini.ReviewInput{Portfolio: block, Reason: reason, Bonds: options, EquityETFs: s.reviewEquityETFs(), Held: heldSymbols(cs)})
	r, err := s.llm.Review(prompt)
	if err != nil {
		if stopOnQuota(cs, err) {
			// No budget for the review: the day is free to retry on a later cycle.
			s.lastReviewDay, s.lastBandReviewDay = "", ""
			s.logReviewSkip(cs, fmt.Sprintf("%s is due, but there is no Gemini budget (%v)", reason, err))
			return
		}
		log.Printf("[Review] %s: Gemini error: %v", reason, err)
		return
	}
	s.applyReview(reason, r, cs, block)
}

// applyReview validates the review's targets against the IPS bands and turns
// at most maxReviewActions approved actions into pending decisions.
func (s *Scheduler) applyReview(reason string, r *gemini.Review, cs *cycleState, block string) {
	targets := "targets unchanged"
	if t := r.Targets; t.Equities != nil && t.Bonds != nil && t.Cash != nil {
		if np, ok := s.policy.ClipTargets(*t.Equities, *t.Bonds, *t.Cash); !ok {
			targets = fmt.Sprintf("targets %.0f/%.0f/%.0f rejected (outside the bands and not summing to 100), kept %.0f/%.0f/%.0f",
				*t.Equities, *t.Bonds, *t.Cash, s.policy.TargetEquity, s.policy.TargetBond, s.policy.TargetCash)
		} else {
			note := ""
			if np.TargetEquity != *t.Equities || np.TargetBond != *t.Bonds || np.TargetCash != *t.Cash {
				note = fmt.Sprintf(" (clipped from %.0f/%.0f/%.0f)", *t.Equities, *t.Bonds, *t.Cash)
			}
			s.policy = np
			targets = fmt.Sprintf("targets equities/bonds/cash %.0f/%.0f/%.0f%s", np.TargetEquity, np.TargetBond, np.TargetCash, note)
		}
	}

	// A book far outside its bands is rebalanced with code-set catalysts, stepwise.
	var breach portfolio.Breach
	if cs.account != nil {
		breach = s.policy.WorstBreach(s.bookFrom(cs, cs.account))
	}
	rebalance := breach.Pts > reviewBreachPts
	if rebalance {
		log.Printf("[Rebalance] %s: %.1f points outside the band; closing it in steps of at most %.0f%% of equity and %s trades a day",
			breach.Catalyst(), breach.Pts, bondStepPct, capLabel(s.cfg.MaxTradesPerDay))
	}

	var queued, rejected []string
	for i, a := range r.Actions {
		if i >= maxReviewActions {
			rejected = append(rejected, fmt.Sprintf("%s %s (over the %d-action limit)", a.Action, a.Symbol, maxReviewActions))
			continue
		}
		p, why := s.reviewAction(a, cs, block)
		if why == "" && rebalance && a.Action == "BUY" {
			// The code states the catalyst; the model's quote is not checked. The
			// portfolio_rationale is still required by the normal guards.
			p.decision.Catalyst, p.rebalance = breach.Catalyst(), true
		}
		if why != "" {
			rejected = append(rejected, fmt.Sprintf("%s %s (%s)", a.Action, a.Symbol, why))
			continue
		}
		cs.pending = append(cs.pending, p)
		queued = append(queued, fmt.Sprintf("%s %s", a.Action, a.Symbol))
	}
	log.Printf("[Review] %s: %s | actions queued: %s | rejected: %s | %s",
		reason, targets, orNone(queued), orNone(rejected), r.Summary)
}

func orNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// reviewEquityETFs are the core equity ETFs (SPY, QQQ) the review may buy to
// raise an equity sleeve that is below its band or target.
func (s *Scheduler) reviewEquityETFs() []string {
	if s.cfg.Universe == nil {
		return nil
	}
	var out []string
	for _, sym := range s.cfg.Universe.Core {
		if portfolio.IsBroadETF(sym) {
			out = append(out, sym)
		}
	}
	return out
}

// reviewMayBuy reports whether the review may propose a BUY of sym: an enabled
// bond or cash-parking ETF, or one of the core equity ETFs.
func (s *Scheduler) reviewMayBuy(sym string) bool {
	if _, ok := portfolio.BondInfoFor(sym); ok {
		return s.cfg.Bonds.Allowed(sym)
	}
	for _, e := range s.reviewEquityETFs() {
		if e == sym {
			return true
		}
	}
	return false
}

// reviewAction validates one recommended action. SELLs must be held; BUYs must
// be an enabled bond / cash-parking ETF or a core equity ETF whose market data
// loads. Whether the purchase fits the IPS (sleeve under target, band maxima,
// caps) and everything else about the action (conviction, catalyst, role,
// caps, cooldown) is checked by the normal guards at execution.
func (s *Scheduler) reviewAction(a gemini.ReviewAction, cs *cycleState, block string) (pendingDecision, string) {
	d := &gemini.Decision{
		Action: a.Action, Conviction: a.Conviction, Role: a.Role, Catalyst: a.Catalyst,
		Rationale: a.Rationale, Replaces: a.Replaces, Thesis: "weekly portfolio review",
	}
	p := pendingDecision{symbol: a.Symbol, decision: d, portfolioText: block, fromReview: true}
	switch a.Action {
	case "SELL":
		pos := cs.positions[a.Symbol]
		if pos == nil {
			return p, "not held"
		}
		p.position = pos
	case "BUY":
		if !s.reviewMayBuy(a.Symbol) {
			return p, "not an allowed review asset (enabled bond / cash-parking ETF or core equity ETF)"
		}
		if s.fetchBond == nil {
			return p, "market data unavailable"
		}
		ind, err := s.fetchBond(a.Symbol)
		if err != nil {
			return p, "market data: " + err.Error()
		}
		s.noteSector(a.Symbol, ind)
		p.ind, p.position = ind, cs.positions[a.Symbol]
		if s.cfg.Analyzer != nil {
			if c, ok := s.cfg.Analyzer.CorrelationTo(a.Symbol, heldSymbols(cs)); ok {
				p.corr = &c
			}
		}
	default:
		return p, "unknown action"
	}
	return p, ""
}
