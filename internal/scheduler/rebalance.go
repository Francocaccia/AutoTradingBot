package scheduler

import (
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/market"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/risk"
)

// IPS_BANDS_MODE values.
const (
	bandsGradual  = "gradual"  // buy core ETFs in tranches toward a sleeve's minimum band
	bandsAdvisory = "advisory" // log the trade that would be made, place nothing
	bandsOff      = "off"      // no band-driven trades at all
)

const (
	defaultRebalanceTranchePct = 5.0
	defaultRebalanceMaxWeek    = 2
	// maxRebalancePerDay is fixed: one policy trade a day, whatever else is allowed.
	maxRebalancePerDay = 1
	// defaultMinPolicyStopPct is the least distance of a policy trade's stop
	// (MIN_POLICY_STOP_PCT). A broad ETF moves 1-2% in a normal day, so a stop
	// closer than this is hit by noise, and the shortfall it leaves is then bought
	// again: a stop-and-rebuy loop that pays the spread each way.
	defaultMinPolicyStopPct = 3.0
	// minDeficitPts is how far below its minimum a sleeve must be before a trade
	// is worth making; a sliver of a point is left to drift.
	minDeficitPts = 1.0
)

// defaultRebalanceBondETFs is the preference order for filling the bond sleeve:
// the broad aggregate first, then short Treasuries. Both come from the curated
// bond table (portfolio.BondInfoFor); nothing here is invented.
var defaultRebalanceBondETFs = []string{"AGG", "SHY"}

// NormalizeBandsMode maps an IPS_BANDS_MODE value to a known mode. ok is false
// (and the default, gradual, returned) for anything else.
func NormalizeBandsMode(v string) (mode string, ok bool) {
	switch m := strings.ToLower(strings.TrimSpace(v)); m {
	case bandsGradual, bandsAdvisory, bandsOff:
		return m, true
	case "":
		return bandsGradual, true
	}
	return bandsGradual, false
}

func (s *Scheduler) bandsMode() string {
	m, _ := NormalizeBandsMode(s.cfg.IPSBandsMode)
	return m
}

func (s *Scheduler) rebalanceTranchePct() float64 {
	if s.cfg.RebalanceTranchePct > 0 {
		return s.cfg.RebalanceTranchePct
	}
	return defaultRebalanceTranchePct
}

// stockStopMultiple is the ATR multiple of a stock stop (see risk.EffectiveStopMultiple).
func (s *Scheduler) stockStopMultiple() float64 {
	return risk.EffectiveStopMultiple(s.cfg.MinStopATRMult)
}

// policyStop returns the stop price of a policy trade bought at price: the ATR
// stop pushed out, if needed, to at least MIN_POLICY_STOP_PCT below price. It is
// 0 when the setting asks for no stop at all (the order is then a plain market
// order). err is set when no stop can be built.
func (s *Scheduler) policyStop(price, atr, atrMultiple float64) (stop float64, err error) {
	minPct := s.cfg.MinPolicyStopPct
	switch {
	case minPct < 0:
		return 0, nil
	case minPct == 0:
		minPct = defaultMinPolicyStopPct
	}
	floor := math.Floor(price*(1-minPct/100)*100) / 100
	if floor <= 0 {
		return 0, fmt.Errorf("stop %.1f%% below %.2f is not positive", minPct, price)
	}
	if atrStop, aerr := risk.StopBelow(price, atr, atrMultiple); aerr == nil && atrStop < floor {
		return atrStop, nil
	}
	return floor, nil
}

func (s *Scheduler) rebalanceMaxWeek() int {
	if s.cfg.RebalanceMaxTradesPerWeek > 0 {
		return s.cfg.RebalanceMaxTradesPerWeek
	}
	return defaultRebalanceMaxWeek
}

// rebalancePlan is one tranche toward a sleeve's minimum band, chosen before the
// cycle's other orders so it is not starved by them.
type rebalancePlan struct {
	sleeve    string
	symbol    string
	qty       int
	price     float64
	stop      float64
	sizePct   float64 // the order's size, % of equity
	reason    string  // why the trade is made, for the log
	deficit   float64 // percentage points the sleeve was below its minimum
	candidate string  // the bond ETF / core ETF family, for the log
}

// rebalanceState is what the rebalancing step decided in one cycle.
type rebalanceState struct {
	plan   *rebalancePlan
	placed bool
}

// sleeveGap is a sleeve below its band minimum.
type sleeveGap struct {
	sleeve  string
	weight  float64
	min     float64
	deficit float64 // percentage points below the minimum
}

// planRebalance looks for a sleeve below its band minimum and, in gradual mode,
// picks one tranche of a core ETF to buy this cycle. It does not use the model,
// the conviction ladder or a news catalyst: the band is the policy, and the
// bounds on the trade are all in code:
//
//   - only core ETFs: the broad equity ETFs of the core list for the equity
//     sleeve, enabled bond ETFs for the bond sleeve, never a single stock;
//   - REBALANCE_TRANCHE_PCT of equity per trade, at most the sleeve's deficit,
//     at most one a day and REBALANCE_MAX_TRADES_PER_WEEK a week;
//   - the investment policy's room (cash floor, band maximums, single-name
//     limit), the per-trade risk cap and the shared daily / weekly trade caps;
//   - the usual BUY block (circuit breakers, the minutes after the open and
//     before the close). The re-entry cooldown does not apply to a policy tranche.
//
// A sleeve above its maximum is never sold down: purchases into it are refused by
// the policy's room, and it is only logged. The cycle's reasons for not trading
// go to the [Review] line.
func (s *Scheduler) planRebalance(cs *cycleState, sess *market.Session) {
	mode := s.bandsMode()
	if cs.history != nil {
		cs.rebalDay, cs.rebalWeek = cs.history.RebalanceCounts()
	}
	if mode == bandsOff || cs.account == nil || cs.account.Equity <= 0 {
		return
	}
	book := s.bookFrom(cs, cs.account)
	w := book.SleeveWeights()

	var gaps []sleeveGap
	for _, sleeve := range []string{portfolio.SleeveEquities, portfolio.SleeveBonds, portfolio.SleeveCash} {
		lo, hi := s.policy.Band(sleeve)
		switch {
		case w[sleeve] > hi:
			if sleeve != portfolio.SleeveCash {
				log.Printf("[Rebalance] %s %.1f%% is above its %.0f%% maximum: not selling, and no purchases into it", sleeve, w[sleeve], hi)
			}
		case sleeve != portfolio.SleeveCash && lo-w[sleeve] >= minDeficitPts-1e-9:
			gaps = append(gaps, sleeveGap{sleeve: sleeve, weight: w[sleeve], min: lo, deficit: lo - w[sleeve]})
		}
	}
	if len(gaps) == 0 {
		return
	}
	sort.SliceStable(gaps, func(i, j int) bool { return gaps[i].deficit > gaps[j].deficit })

	var skip string
	switch {
	case sess.Now.Hour() < entryReviewHour:
		skip = fmt.Sprintf("rebalance: waits for %d:00 ET", entryReviewHour)
	case cs.buyBlock != "":
		skip = "rebalance: BUYs blocked (" + cs.buyBlock + ")"
	case cs.history == nil:
		skip = "rebalance: order history unavailable, caps cannot be checked"
	case cs.rebalDay >= maxRebalancePerDay:
		skip = fmt.Sprintf("rebalance: one trade per day already made (today %d)", cs.rebalDay)
	case cs.rebalWeek >= s.rebalanceMaxWeek():
		skip = fmt.Sprintf("rebalance: weekly limit reached (%d of %d in the last %d trading days)", cs.rebalWeek, s.rebalanceMaxWeek(), 5)
	case !s.capRoom(cs, 1):
		skip = fmt.Sprintf("rebalance: trade cap reached (today %d/%s, week %d/%s)", cs.tradesDay, capLabel(s.cfg.MaxTradesPerDay), cs.tradesWeek, capLabel(s.cfg.MaxTradesPerWeek))
	}
	if skip != "" {
		log.Printf("[Rebalance] %s", gapText(gaps, skip))
		cs.why(gapText(gaps, skip))
		return
	}

	var notes []string
	for _, g := range gaps {
		plan, why := s.proposeTranche(cs, book, g)
		if plan != nil {
			cs.rebal.plan = plan
			break
		}
		notes = append(notes, why)
	}
	if cs.rebal.plan == nil {
		msg := gapText(gaps, "rebalance: no eligible trade ("+strings.Join(notes, "; ")+")")
		log.Printf("[Rebalance] %s", msg)
		cs.why(msg)
		return
	}
	p := cs.rebal.plan
	if mode == bandsAdvisory {
		msg := fmt.Sprintf("would BUY %d %s, %.1f%% of equity (advisory, not submitted): %s", p.qty, p.symbol, p.sizePct, p.reason)
		log.Printf("[Rebalance] kind=rebalance %s", msg)
		cs.why("rebalance " + msg)
		cs.rebal.plan = nil
	}
}

// gapText prefixes a skip reason with the sleeves that are under their minimum.
func gapText(gaps []sleeveGap, reason string) string {
	var parts []string
	for _, g := range gaps {
		parts = append(parts, fmt.Sprintf("%s %.1f%% < %.0f%% min", g.sleeve, g.weight, g.min))
	}
	return strings.Join(parts, ", ") + " → " + reason
}

// bondCandidates lists the bond ETFs the bond sleeve may be filled with, in
// preference order: the configured list, kept to ETFs that are enabled, count as
// bonds (not T-bill cash) and, without macro data, are safe to buy blind. When
// rates are rising the shorter-duration ones come first.
func (s *Scheduler) bondCandidates(cs *cycleState) []string {
	pref := s.cfg.RebalanceBondETFs
	if len(pref) == 0 {
		pref = defaultRebalanceBondETFs
	}
	var out []string
	for _, sym := range pref {
		info, ok := portfolio.BondInfoFor(sym)
		if !ok || info.CashLike || !s.cfg.Bonds.Allowed(sym) {
			continue
		}
		if !cs.regimeAvailable() && !portfolio.SafeWithoutMacro[info.Symbol] {
			continue
		}
		out = append(out, info.Symbol)
	}
	if cs.pf != nil && cs.pf.regime.Available && cs.pf.regime.RatesRising {
		sort.SliceStable(out, func(i, j int) bool {
			a, _ := portfolio.BondInfoFor(out[i])
			b, _ := portfolio.BondInfoFor(out[j])
			return a.Duration < b.Duration
		})
	}
	return out
}

// rebalanceIndicators loads price data for a rebalancing ETF. A core ETF whose
// validation failed (a data outage, not a real rejection of the ETF) is still
// used if its raw indicators load, with a warning: an outage must not stop the
// policy from being followed.
func (s *Scheduler) rebalanceIndicators(sym string) (*indicatorsResult, error) {
	if s.fetchBond != nil {
		ind, err := s.fetchBond(sym)
		if err == nil {
			return ind, nil
		}
		if s.fetchRaw == nil {
			return nil, err
		}
		raw, rerr := s.fetchRaw(sym)
		if rerr != nil {
			return nil, err
		}
		log.Printf("[Rebalance] WARN %s: validation failed (%v); using its raw market data (failing open for a core ETF)", sym, err)
		return raw, nil
	}
	if s.fetchRaw != nil {
		return s.fetchRaw(sym)
	}
	return nil, errors.New("market data unavailable")
}

// indicatorsResult shortens the indicator type in this file.
type indicatorsResult = indicators.Result

// proposeTranche sizes one tranche for the sleeve gap g, trying the sleeve's ETFs
// in order. It returns the plan, or why no ETF qualified.
func (s *Scheduler) proposeTranche(cs *cycleState, book portfolio.Book, g sleeveGap) (*rebalancePlan, string) {
	var candidates []string
	stopMultiple := s.stockStopMultiple()
	switch g.sleeve {
	case portfolio.SleeveEquities:
		candidates = s.reviewEquityETFs()
	case portfolio.SleeveBonds:
		candidates = s.bondCandidates(cs)
		stopMultiple = risk.BondStopATRMultiple
	}
	if len(candidates) == 0 {
		return nil, fmt.Sprintf("%s: no eligible core ETF", g.sleeve)
	}
	acct := cs.account
	var notes []string
	for _, sym := range candidates {
		// The re-entry cooldown is for discretionary BUYs. A policy tranche is a
		// floor being restored by rule, not an idea being re-tried, and the
		// protective stop is kept wide enough that noise does not trigger it.
		ind, err := s.rebalanceIndicators(sym)
		if err != nil || ind == nil || ind.Price <= 0 {
			notes = append(notes, fmt.Sprintf("%s: no market data", sym))
			continue
		}
		s.noteSector(sym, ind)
		room, bind := s.policy.Room(book, sym, ind.Sector)
		tranche := acct.Equity * s.rebalanceTranchePct() / 100
		deficit := g.deficit / 100 * acct.Equity
		value := math.Min(math.Min(tranche, deficit), math.Min(room, acct.Spendable()))
		qty := int(math.Floor(value/ind.Price + 1e-9))
		if qty < 1 {
			why := "size below one share"
			if room < tranche && bind != "" {
				why = "policy limit: " + bind
			}
			notes = append(notes, fmt.Sprintf("%s: %s", sym, why))
			continue
		}
		stop, err := s.policyStop(ind.Price, ind.ATR, stopMultiple)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: no stop level (%v)", sym, err))
			continue
		}
		if stop > 0 {
			capped, stopPct := risk.CapQtyByRisk(qty, ind.Price, stop, acct.Equity, s.cfg.MaxRiskPct)
			if capped < 1 {
				notes = append(notes, fmt.Sprintf("%s: max-risk %.2f%% of equity allows less than one share (stop %.1f%% away)", sym, s.cfg.MaxRiskPct, stopPct))
				continue
			}
			qty = capped
		}
		size := float64(qty) * ind.Price / acct.Equity * 100
		return &rebalancePlan{
			sleeve: g.sleeve, symbol: sym, qty: qty, price: ind.Price, stop: stop, sizePct: size, deficit: g.deficit,
			reason: fmt.Sprintf("%s sleeve %.1f%% is below its %.0f%% minimum (tranche limit %.1f%% of equity)", g.sleeve, g.weight, g.min, s.rebalanceTranchePct()),
		}, ""
	}
	return nil, strings.Join(notes, "; ")
}

// execRebalance places the planned tranche. It runs after the cycle's risk-reducing
// SELLs and before its discretionary BUYs, so the shared daily cap cannot be used
// up by those first, which is how a sleeve stayed under its band for days.
func (s *Scheduler) execRebalance(cs *cycleState) {
	p := cs.rebal.plan
	if p == nil || s.bandsMode() != bandsGradual {
		return
	}
	// The SELLs just executed may have used the last slot.
	if !s.capRoom(cs, 1) {
		cs.why(fmt.Sprintf("rebalance: trade cap reached after this cycle's sells (today %d/%s, week %d/%s)",
			cs.tradesDay, capLabel(s.cfg.MaxTradesPerDay), cs.tradesWeek, capLabel(s.cfg.MaxTradesPerWeek)))
		return
	}
	// Spendable cash may have changed since the plan was made.
	acct, err := s.acct.GetAccount()
	if err != nil {
		log.Printf("[Rebalance] skipped: account fetch failed: %v", err)
		cs.why("rebalance: account fetch failed")
		return
	}
	if cost := float64(p.qty) * p.price; cost > acct.Spendable() {
		cs.why(fmt.Sprintf("rebalance: %s needs more cash than is spendable", p.symbol))
		return
	}
	if p.stop > 0 {
		err = s.orders.PlaceOTOOrder(p.symbol, p.qty, broker.Buy, p.stop, broker.KindRebalance)
	} else {
		err = s.orders.PlaceMarketOrder(p.symbol, p.qty, broker.Buy, broker.KindRebalance)
	}
	switch {
	case errors.Is(err, broker.ErrDuplicateOrder):
		log.Printf("[Rebalance] %s skipped: %v", p.symbol, err)
		cs.why("rebalance: " + p.symbol + " already ordered today")
		return
	case err != nil:
		log.Printf("[Rebalance] %s BUY failed: %v", p.symbol, err)
		cs.why("rebalance: order failed")
		return
	}
	stopText := "no stop"
	if p.stop > 0 {
		stopText = fmt.Sprintf("stop %.2f, %.1f%% below", p.stop, (p.price-p.stop)/p.price*100)
	}
	log.Printf("[Rebalance] kind=rebalance BUY %d %s @ ~$%.2f (%s, %.1f%% of equity): %s", p.qty, p.symbol, p.price, stopText, p.sizePct, p.reason)
	s.recordBuy(cs, p.symbol, cs.positions[p.symbol], p.qty, p.price)
	cs.rebal.placed = true
	cs.tradesDay++
	cs.tradesWeek++
	cs.rebalDay++
	cs.rebalWeek++
	cs.took("rebalance")
	s.logRebalanceProgress(cs, acct, float64(p.qty)*p.price)
}

// logRebalanceProgress reports how far the book still is below its minimums
// after a rebalancing BUY that spent `spent` of cash (acct is the pre-order account).
func (s *Scheduler) logRebalanceProgress(cs *cycleState, acct *portfolio.Account, spent float64) {
	after := *acct
	after.Cash -= spent
	w := s.bookFrom(cs, &after).SleeveWeights()
	var left []string
	for _, sleeve := range []string{portfolio.SleeveEquities, portfolio.SleeveBonds} {
		if lo, _ := s.policy.Band(sleeve); lo-w[sleeve] >= minDeficitPts-1e-9 {
			left = append(left, fmt.Sprintf("%s %.1f%% vs %.0f%% min", sleeve, w[sleeve], lo))
		}
	}
	if len(left) == 0 {
		log.Printf("[Rebalance] every sleeve is back at or above its minimum")
		return
	}
	log.Printf("[Rebalance] still below the minimum: %s (at most %.0f%% of equity per trade, %d a week, one a day)",
		strings.Join(left, ", "), s.rebalanceTranchePct(), s.rebalanceMaxWeek())
}
