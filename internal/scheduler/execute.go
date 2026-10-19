package scheduler

import (
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/risk"
)

// minMacroCatalystLen is the shortest quote accepted from the PORTFOLIO block.
const minMacroCatalystLen = 6

// catalystMatches reports whether the model's catalyst quotes one of the
// headlines it was shown (case-insensitive substring, either direction so a
// "[Source] headline" quote also counts). "none" and empty never match.
func catalystMatches(catalyst string, headlines []string) bool {
	c := normQuote(catalyst)
	if c == "" || c == "none" {
		return false
	}
	for _, h := range headlines {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if c == h {
			return true
		}
		if len(c) >= minCatalystLen && strings.Contains(h, c) {
			return true
		}
		if len(h) >= minCatalystLen && strings.Contains(c, h) {
			return true
		}
	}
	return false
}

func normQuote(s string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(s), "\"'“”‘’ "))
}

// normMacro lowercases, trims surrounding quotes and collapses whitespace.
func normMacro(s string) string {
	return strings.Join(strings.Fields(normQuote(s)), " ")
}

// macroCatalystMatches reports whether a bond-ETF (or review) catalyst is ONE
// item printed in the PORTFOLIO block: a full line, or a single " | "-separated
// "label: value" token of a line (for the macro regime line, also its label
// alone, e.g. "risk-off, inverted curve"). Case and whitespace are normalized.
// A concatenation of several lines or phrases never matches, and placeholder
// text such as "unavailable" is never a valid catalyst.
func macroCatalystMatches(catalyst, block string) bool {
	c := normMacro(catalyst)
	if len(c) < minMacroCatalystLen || c == "none" || strings.Contains(c, "unavailable") || strings.Contains(c, "n/a") {
		return false
	}
	for _, line := range strings.Split(block, "\n") {
		line = normMacro(line)
		if line == "" || strings.HasPrefix(line, "## ") {
			continue
		}
		if c == line {
			return true
		}
		for i, token := range strings.Split(line, " | ") {
			token = strings.TrimSpace(token)
			if c == token {
				return true
			}
			// "Sleeves vs IPS bands (% of equity): equities 60.0% (...)": the first
			// token also counts without its line label, as does the regime label.
			if i == 0 {
				if _, value, ok := strings.Cut(token, ": "); ok && c == strings.TrimSpace(value) {
					return true
				}
			}
		}
	}
	return false
}

// capRoom reports whether n more orders fit under both turnover caps.
func (s *Scheduler) capRoom(cs *cycleState, n int) bool {
	if s.cfg.MaxTradesPerDay > 0 && cs.tradesDay+n > s.cfg.MaxTradesPerDay {
		return false
	}
	if s.cfg.MaxTradesPerWeek > 0 && cs.tradesWeek+n > s.cfg.MaxTradesPerWeek {
		return false
	}
	return true
}

func (s *Scheduler) dropCap(symbol, action string, cs *cycleState) {
	cs.drop(symbol, action, "cap", fmt.Sprintf("trade cap (today %d/%s, week %d/%s)",
		cs.tradesDay, capLabel(s.cfg.MaxTradesPerDay), cs.tradesWeek, capLabel(s.cfg.MaxTradesPerWeek)))
}

func (s *Scheduler) minBuyConviction() int {
	if s.cfg.MinBuyConviction > 0 {
		return s.cfg.MinBuyConviction
	}
	return risk.DefaultMinConviction
}

// roleProblem returns why a decision lacks the portfolio fields ("" if fine).
func roleProblem(d *gemini.Decision) string {
	if !gemini.ValidRole(d.Role) {
		return fmt.Sprintf("missing or invalid role %q", d.Role)
	}
	if strings.TrimSpace(d.Rationale) == "" {
		return "missing portfolio_rationale"
	}
	return ""
}

// isRegimeAvailable reads the cycle's macro regime.
func (cs *cycleState) regimeAvailable() bool { return cs.pf != nil && cs.pf.regime.Available }

// buyEligibility returns why a BUY may not be executed ("" if it may) and a
// short key for the cycle summary. These checks do not depend on how many
// orders were placed this cycle, so a BUY that fails them never uses a
// turnover slot.
func (s *Scheduler) buyEligibility(p *pendingDecision, cs *cycleState) (why, key string) {
	d := p.decision
	bond, isBond := portfolio.BondInfoFor(p.symbol)
	switch {
	case p.sellOnly:
		return "symbol is outside the validated universe", "universe"
	case cs.buyBlock != "":
		return cs.buyBlock, "buy-block"
	}
	if prob := roleProblem(d); prob != "" {
		return prob, "role/rationale"
	}
	if d.Conviction < s.minBuyConviction() {
		return fmt.Sprintf("conviction %d below minimum %d", d.Conviction, s.minBuyConviction()), "conviction"
	}

	if isBond {
		// Bond / T-bill ETF sleeve: the catalyst is a macro datapoint or label
		// from the PORTFOLIO block, not a news headline.
		if !s.cfg.Bonds.Allowed(p.symbol) {
			return fmt.Sprintf("%s is not enabled in the bond universe", p.symbol), "bond-universe"
		}
		if !cs.regimeAvailable() && !portfolio.SafeWithoutMacro[bond.Symbol] {
			return fmt.Sprintf("macro regime unavailable: only SGOV, BIL and SHY may be bought (not %s)", p.symbol), "macro-unavailable"
		}
	}
	// Catalyst. A bond ETF, or any review action, must quote ONE item of the
	// PORTFOLIO block; a stock must quote a headline it was shown. (Band
	// rebalancing needs neither: planRebalance chooses it, the model does not.)
	switch {
	case isBond || p.fromReview:
		if !macroCatalystMatches(d.Catalyst, p.portfolioText) {
			return fmt.Sprintf("no verified macro catalyst (%q is not one line or one \"label: value\" item of the PORTFOLIO block)", d.Catalyst), "catalyst"
		}
	case !catalystMatches(d.Catalyst, p.headlines):
		return fmt.Sprintf("no verified catalyst (%q is not one of the %d headlines shown)", d.Catalyst, len(p.headlines)), "catalyst"
	}

	if cs.history == nil {
		return "order history unavailable, cannot check cooldown and turnover limits", "history"
	}
	if s.cfg.ReentryCooldownDays > 0 {
		if days, ok := cs.history.SinceLastSell(p.symbol); ok && days < s.cfg.ReentryCooldownDays {
			return fmt.Sprintf("re-entry cooldown: sold %d trading day(s) ago, minimum %d", days, s.cfg.ReentryCooldownDays), "cooldown"
		}
	}

	// Cash inside its band is not a problem and must not trigger buying.
	if (isBond && bond.CashLike || d.Role == "cash_parking") && cs.account != nil && cs.account.Equity > 0 {
		if idle := cs.account.Cash / cs.account.Equity * 100; idle <= s.policy.CashMax {
			return fmt.Sprintf("cash %.1f%% of equity is inside its band (max %.0f%%): parking is not needed", idle, s.policy.CashMax), "cash-in-band"
		}
	}

	// Highly correlated with what is held: only as a rotation out of a held symbol.
	if p.corr != nil && p.corr.Avg > maxCorrelation && !s.validReplaces(p, cs) {
		return fmt.Sprintf("average 60d correlation to holdings %.2f exceeds %.1f and no valid \"replaces\" (%q)", p.corr.Avg, maxCorrelation, d.Replaces), "correlation"
	}
	return "", ""
}

// validReplaces reports whether the decision names a different, actually held symbol.
func (s *Scheduler) validReplaces(p *pendingDecision, cs *cycleState) bool {
	r := p.decision.Replaces
	return r != "" && r != p.symbol && cs.positions[r] != nil
}

// executeDecisions acts on the cycle's collected answers. Risk-reducing SELLs
// run first, then eligible BUYs by conviction (highest first), so when a
// turnover cap binds the most valuable orders are the ones that go through.
func (s *Scheduler) executeDecisions(cs *cycleState) {
	var sells, buys []pendingDecision
	seen := map[string]bool{}
	for _, p := range cs.pending {
		if p.decision.Action == "BUY" || p.decision.Action == "SELL" {
			if seen[p.symbol] {
				log.Printf("[%s] %s ignored: another decision for this symbol is already queued this cycle", p.symbol, p.decision.Action)
				continue
			}
			seen[p.symbol] = true
		}
		switch p.decision.Action {
		case "BUY":
			cs.counts.buy++
			if why, key := s.buyEligibility(&p, cs); why != "" {
				cs.drop(p.symbol, "BUY", key, why)
				continue
			}
			buys = append(buys, p)
		case "SELL":
			cs.counts.sell++
			if p.position == nil {
				log.Printf("[%s] SELL ignored: no long position to close (long-only)", p.symbol)
				continue
			}
			// A circuit-breaker exit reduces risk and is never held up by missing metadata.
			if prob := roleProblem(p.decision); prob != "" && !cs.breaker {
				cs.drop(p.symbol, "SELL", "role/rationale", prob)
				continue
			}
			sells = append(sells, p)
		case "HOLD":
			cs.counts.hold++
		default:
			log.Printf("[%s] unknown action %q — ignored", p.symbol, p.decision.Action)
		}
	}
	sort.SliceStable(buys, func(i, j int) bool { return buys[i].decision.Conviction > buys[j].decision.Conviction })

	for _, p := range sells {
		s.execSell(p, cs)
	}
	// The policy tranche goes before the discretionary BUYs: they would
	// otherwise use up the shared daily cap first.
	s.execRebalance(cs)
	for _, p := range buys {
		// A BUY that came from the weekly review is policy-driven: in advisory mode
		// it is only reported, never submitted, and it takes no trade cap.
		if p.fromReview && s.bandsMode() == bandsAdvisory {
			s.adviseBuy(p, cs)
			continue
		}
		s.execBuy(p, cs)
	}
}

// sellBlocked applies the holding period and turnover caps to a SELL that
// takes `slots` orders. It returns the key and reason when blocked.
func (s *Scheduler) sellBlocked(p pendingDecision, cs *cycleState) (key, why string) {
	// Under a circuit breaker a SELL is a risk exit: the holding period and the
	// turnover caps do not apply, and it is not counted against them.
	if cs.breaker || cs.history == nil {
		return "", ""
	}
	// Rebalancing SELLs from the weekly review are exempt from the holding
	// period (they still count toward the caps).
	if s.cfg.MinHoldDays > 0 && !p.fromReview {
		if age, ok := cs.history.PositionAge(p.symbol); ok && age < s.cfg.MinHoldDays {
			return "min-hold", fmt.Sprintf("position is %d trading day(s) old, minimum hold %d", age, s.cfg.MinHoldDays)
		}
	}
	return "", ""
}

func (s *Scheduler) execSell(p pendingDecision, cs *cycleState) bool {
	if key, why := s.sellBlocked(p, cs); key != "" {
		cs.drop(p.symbol, "SELL", key, why)
		return false
	}
	if !cs.breaker && cs.history != nil && !s.capRoom(cs, 1) {
		s.dropCap(p.symbol, "SELL", cs)
		return false
	}
	if !s.closePosition(p.symbol, p.position, cs) {
		return false
	}
	if !cs.breaker {
		cs.tradesDay++
		cs.tradesWeek++
	}
	cs.took("sell")
	return true
}

func (s *Scheduler) execBuy(p pendingDecision, cs *cycleState) {
	// A rotation (BUY with "replaces" naming a held symbol) sells the old
	// position first. Both orders must fit the caps and the holding period, or
	// neither is placed: never leave the book half-rotated.
	repl := p.decision.Replaces
	if repl != "" && repl != p.symbol && cs.positions[repl] != nil {
		if !s.capRoom(cs, 2) {
			s.dropCap(p.symbol, "BUY", cs)
			return
		}
		sell := pendingDecision{
			symbol: repl, position: cs.positions[repl], fromReview: p.fromReview,
			decision: &gemini.Decision{Action: "SELL", Conviction: p.decision.Conviction, Role: p.decision.Role,
				Rationale: "rotation into " + p.symbol + ": " + p.decision.Rationale},
		}
		if key, why := s.sellBlocked(sell, cs); key != "" {
			cs.drop(p.symbol, "BUY", key, "replaced position "+repl+" cannot be sold: "+why)
			return
		}
		if !s.execSell(sell, cs) {
			cs.drop(p.symbol, "BUY", "replaces", "replaced position "+repl+" was not sold")
			return
		}
	}

	if !s.capRoom(cs, 1) {
		s.dropCap(p.symbol, "BUY", cs)
		return
	}
	if p.position == nil && cs.positions[p.symbol] == nil && len(cs.positions) >= s.cfg.MaxOpenPositions {
		cs.drop(p.symbol, "BUY", "max-positions", fmt.Sprintf("%d open positions (max %d)", len(cs.positions), s.cfg.MaxOpenPositions))
		return
	}
	var placed bool
	if _, isBond := portfolio.BondInfoFor(p.symbol); isBond {
		placed = s.placeBondBuy(p, cs)
	} else {
		placed = s.placeBuy(p, cs)
	}
	if placed {
		cs.tradesDay++
		cs.tradesWeek++
		cs.took("buy")
	}
}

// placeBuy sizes and submits a bracket BUY of a stock or equity ETF. It
// reports whether an order was placed.
func (s *Scheduler) placeBuy(p pendingDecision, cs *cycleState) bool {
	symbol, ind, decision := p.symbol, p.ind, p.decision
	position := cs.positions[symbol]
	levels, err := risk.BracketMult(ind.Price, ind.ATR, decision.TakeProfit, s.stockStopMultiple())
	if err != nil {
		cs.drop(symbol, "BUY", "bracket", fmt.Sprintf("rejected: %v", err))
		return false
	}

	// Fresh account so buying power reflects orders placed earlier this cycle.
	acct, err := s.acct.GetAccount()
	if err != nil {
		log.Printf("[%s] BUY skipped: account fetch failed: %v", symbol, err)
		return false
	}
	currentValue := 0.0
	if position != nil {
		currentValue = position.MarketVal
	}
	qty, reason := risk.Size(risk.SizeInput{
		Equity:               acct.Equity,
		Spendable:            acct.Spendable(),
		Price:                ind.Price,
		CurrentPositionValue: currentValue,
		Conviction:           decision.Conviction,
		MinConviction:        s.cfg.MinBuyConviction,
		MaxPositionPct:       s.cfg.MaxPositionPct,
		MaxPositionUSD:       s.cfg.MaxPositionUSD,
	})
	if qty <= 0 {
		cs.drop(symbol, "BUY", "size", reason)
		return false
	}

	// Investment policy: bands, single-name, sector (unknown is its own
	// bucket), credit and duration limits. Trim to fit; below one share rejects.
	room, bind := s.policy.Room(s.bookFrom(cs, acct), symbol, ind.Sector)
	if maxQty := int(math.Floor(room/ind.Price + 1e-9)); maxQty < qty {
		if maxQty < 1 {
			cs.drop(symbol, "BUY", "ips", "rejected by the investment policy: "+bind)
			return false
		}
		log.Printf("[%s] BUY quantity cut from %d to %d by the investment policy (%s)", symbol, qty, maxQty, bind)
		qty = maxQty
	}

	// A review BUY (an equity ETF raising its sleeve) is a step toward the
	// sleeve target: at most bondStepPct of equity, never past the target.
	if p.fromReview {
		step, key, why := s.sleeveStep(symbol, s.bookFrom(cs, acct), acct.Equity)
		if key != "" {
			cs.drop(symbol, "BUY", key, why)
			return false
		}
		if maxQty := int(math.Floor(step/ind.Price + 1e-9)); maxQty < qty {
			if maxQty < 1 {
				cs.drop(symbol, "BUY", "ips", "size below one share after the stepwise rebalance cap")
				return false
			}
			log.Printf("[%s] BUY quantity cut from %d to %d (review step: at most %.0f%% of equity)", symbol, qty, maxQty, bondStepPct)
			qty = maxQty
		}
	}

	// Per-trade risk: the loss if the stop is hit may not exceed MAX_RISK_PCT of equity.
	if capped, stopPct := risk.CapQtyByRisk(qty, ind.Price, levels.Stop, acct.Equity, s.cfg.MaxRiskPct); capped < qty {
		if capped < 1 {
			cs.drop(symbol, "BUY", "risk-cap", fmt.Sprintf("max-risk %.2f%% of equity allows less than 1 share (stop %.1f%% away)", s.cfg.MaxRiskPct, stopPct))
			return false
		}
		log.Printf("[%s] BUY qty capped: requested %d, capped %d, risk $%.2f of $%.2f allowed (max-risk %.2f%%, stop %.1f%% away)",
			symbol, qty, capped, float64(capped)*(ind.Price-levels.Stop), acct.Equity*s.cfg.MaxRiskPct/100, s.cfg.MaxRiskPct, stopPct)
		qty = capped
	}

	if err := s.orders.PlaceBracketOrder(symbol, qty, broker.Buy, levels.Stop, levels.Take, broker.KindEntry); err != nil {
		return s.orderFailed(cs, symbol, "BUY", "BUY (bracket)", err)
	}
	s.logBuy(symbol, fmt.Sprintf("%d shares @ ~$%.2f (bracket stop=%.2f take=%.2f R:R=%.1f)", qty, ind.Price, levels.Stop, levels.Take, levels.RewardRisk))
	s.recordBuy(cs, symbol, position, qty, ind.Price)
	return true
}

// orderFailed handles an order the broker layer did not place. A duplicate (the
// same order already went in today, e.g. before a restart) is a skipped
// decision, not an error; anything else is logged. It always returns false.
func (s *Scheduler) orderFailed(cs *cycleState, symbol, action, what string, err error) bool {
	if errors.Is(err, broker.ErrDuplicateOrder) {
		cs.drop(symbol, action, "duplicate", err.Error())
		return false
	}
	log.Printf("[%s] %s failed: %v", symbol, what, err)
	return false
}

// sleeveStep is the most a single review / bond purchase of sym may add: at
// most bondStepPct of equity, and for the bond and equity sleeves no more than
// the gap to the sleeve target (key != "" when the sleeve is already at or
// above it). The cash sleeve (T-bill ETFs) has no gap check: parking cash is
// governed by the cash band instead.
func (s *Scheduler) sleeveStep(sym string, book portfolio.Book, equity float64) (value float64, key, why string) {
	value = equity * bondStepPct / 100
	if sleeve := portfolio.Sleeve(sym); sleeve != portfolio.SleeveCash {
		w := book.SleeveWeights()[sleeve]
		gap := (s.policy.Target(sleeve) - w) / 100 * equity
		if gap <= 0 {
			return 0, "sleeve-target", fmt.Sprintf("%s sleeve %.1f%% is at or above its %.0f%% target", sleeve, w, s.policy.Target(sleeve))
		}
		value = math.Min(value, gap)
	}
	return value, "", ""
}

// recordBuy reflects a placed BUY in the cycle's position map so later orders
// this cycle see it.
func (s *Scheduler) recordBuy(cs *cycleState, symbol string, position *portfolio.Position, qty int, price float64) {
	cost := float64(qty) * price
	if position == nil {
		cs.positions[symbol] = &portfolio.Position{Symbol: symbol, Qty: qty, AvgPrice: price, MarketVal: cost}
		return
	}
	position.MarketVal += cost
	position.Qty += qty
}

// placeBondBuy buys a bond / T-bill ETF. Bond ETFs are not sized by the
// conviction ladder: the purchase moves the sleeve toward its target weight, by
// at most bondStepPct of equity. T-bill ETFs get a plain market order; other
// bond ETFs a wider ATR stop (no take-profit; the weekly review revisits them),
// and the per-trade risk cap uses that stop.
func (s *Scheduler) placeBondBuy(p pendingDecision, cs *cycleState) bool {
	sym, ind := p.symbol, p.ind
	info, _ := portfolio.BondInfoFor(sym)
	if ind == nil || ind.Price <= 0 {
		cs.drop(sym, "BUY", "size", "no price for the bond ETF")
		return false
	}
	acct, err := s.acct.GetAccount()
	if err != nil {
		log.Printf("[%s] BUY skipped: account fetch failed: %v", sym, err)
		return false
	}
	book := s.bookFrom(cs, acct)

	step, key, why := s.sleeveStep(sym, book, acct.Equity)
	if key != "" {
		cs.drop(sym, "BUY", key, why)
		return false
	}
	room, bind := s.policy.Room(book, sym, "")
	value := math.Min(math.Min(step, room), acct.Spendable())
	qty := int(math.Floor(value/ind.Price + 1e-9)) // epsilon: 28.000000000000004% must not cost a share
	if qty < 1 {
		why := "size below one share"
		if room < step && bind != "" {
			why = "rejected by the investment policy: " + bind
		}
		cs.drop(sym, "BUY", "ips", why)
		return false
	}

	if info.CashLike {
		if err := s.orders.PlaceMarketOrder(sym, qty, broker.Buy, broker.KindEntry); err != nil {
			return s.orderFailed(cs, sym, "BUY", "BUY", err)
		}
		s.logBuy(sym, fmt.Sprintf("%d shares @ ~$%.2f (cash-like ETF, no bracket, %.1f%% of equity)", qty, ind.Price, float64(qty)*ind.Price/acct.Equity*100))
		s.recordBuy(cs, sym, cs.positions[sym], qty, ind.Price)
		return true
	}

	// A bond ETF moves a fraction of a percent a day, so a plain ATR stop sits
	// inside ordinary noise: the stop is at least MIN_POLICY_STOP_PCT away (or
	// there is none), as for the policy rebalancing trades.
	stop, err := s.policyStop(ind.Price, ind.ATR, risk.BondStopATRMultiple)
	if err != nil {
		cs.drop(sym, "BUY", "bracket", fmt.Sprintf("rejected: %v", err))
		return false
	}
	if stop > 0 {
		if capped, stopPct := risk.CapQtyByRisk(qty, ind.Price, stop, acct.Equity, s.cfg.MaxRiskPct); capped < qty {
			if capped < 1 {
				cs.drop(sym, "BUY", "risk-cap", fmt.Sprintf("max-risk %.2f%% of equity allows less than 1 share (stop %.1f%% away)", s.cfg.MaxRiskPct, stopPct))
				return false
			}
			log.Printf("[%s] BUY qty capped: requested %d, capped %d, risk $%.2f of $%.2f allowed (max-risk %.2f%%, stop %.1f%% away)",
				sym, qty, capped, float64(capped)*(ind.Price-stop), acct.Equity*s.cfg.MaxRiskPct/100, s.cfg.MaxRiskPct, stopPct)
			qty = capped
		}
	}
	if stop > 0 {
		err = s.orders.PlaceOTOOrder(sym, qty, broker.Buy, stop, broker.KindEntry)
	} else {
		err = s.orders.PlaceMarketOrder(sym, qty, broker.Buy, broker.KindEntry)
	}
	if err != nil {
		return s.orderFailed(cs, sym, "BUY", "BUY (bond ETF)", err)
	}
	stopText := "no stop"
	if stop > 0 {
		stopText = fmt.Sprintf("stop=%.2f, %.1f%% below", stop, (ind.Price-stop)/ind.Price*100)
	}
	s.logBuy(sym, fmt.Sprintf("%d shares @ ~$%.2f (bond ETF, %s, no take-profit, %.1f%% of equity)",
		qty, ind.Price, stopText, float64(qty)*ind.Price/acct.Equity*100))
	s.recordBuy(cs, sym, cs.positions[sym], qty, ind.Price)
	return true
}

// closePosition exits a whole long position. Open bracket legs reserve the
// shares, so they are cancelled first. It reports whether the order was placed.
func (s *Scheduler) closePosition(symbol string, pos *portfolio.Position, cs *cycleState) bool {
	// Ask first: cancelling a position's stop legs and only then finding that the
	// exit is a duplicate (or that the lookup fails) would leave it unprotected.
	if err := s.orders.CheckOrder(symbol, broker.Sell, broker.KindExit); err != nil {
		return s.orderFailed(cs, symbol, "SELL", "SELL", err)
	}
	if n, err := s.orders.CancelOpenOrders(symbol); err != nil {
		log.Printf("[%s] cancelling open orders failed (%d cancelled): %v", symbol, n, err)
	}
	if err := s.orders.PlaceMarketOrder(symbol, pos.Qty, broker.Sell, broker.KindExit); err != nil {
		return s.orderFailed(cs, symbol, "SELL", "SELL", err)
	}
	log.Printf("[%s] SELL %d shares (closing long)", symbol, pos.Qty)
	delete(cs.positions, symbol)
	cs.sold[symbol] = true
	return true
}
