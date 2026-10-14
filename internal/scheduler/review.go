package scheduler

import (
	"errors"
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
	if cs.pf == nil || sess.Now.Hour() < entryReviewHour {
		return ""
	}
	// At most one review per process per day, whatever triggered it.
	if s.lastReviewDay == sess.Date || s.lastBandReviewDay == sess.Date {
		return ""
	}
	if sess.Now.Weekday() == time.Monday {
		return "weekly review"
	}
	if pts, desc := s.policy.BandBreach(s.bookFrom(cs, cs.account)); pts > reviewBreachPts {
		return fmt.Sprintf("band breach of %.1f points (%s)", pts, desc)
	}
	return ""
}

// maybeReview runs the portfolio review (one Gemini call) when due and queues
// its approved actions as ordinary decisions, which then go through every guard.
func (s *Scheduler) maybeReview(cs *cycleState, sess *market.Session) {
	if cs.stopGemini {
		return
	}
	reason := s.reviewReason(cs, sess)
	if reason == "" {
		return
	}
	// Never spend a call (or mark the day as done) while Gemini is paused: the
	// review runs on the first cycle after it resumes.
	pausedFn := s.geminiPaused
	if pausedFn == nil && s.cfg.Gemini != nil {
		pausedFn = s.cfg.Gemini.Paused
	}
	if pausedFn == nil {
		return
	}
	if paused, why := pausedFn(); paused {
		log.Printf("[Review] %s skipped: Gemini is paused (%s)", reason, why)
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
	r, err := s.cfg.Gemini.Review(prompt)
	if err != nil {
		if errors.Is(err, gemini.ErrQuotaExhausted) || errors.Is(err, gemini.ErrBudgetExhausted) {
			cs.stopGemini = true
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
