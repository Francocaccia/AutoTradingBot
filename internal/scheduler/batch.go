package scheduler

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/francocaccia/quantengine/internal/analysis"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/risk"
	"github.com/francocaccia/quantengine/internal/trades"
)

// decider is the part of the Gemini client a decision cycle uses.
type decider interface {
	DecideBatch(prompt string, prio gemini.Priority) ([]gemini.BatchResult, error)
	DecideAt(prompt string, prio gemini.Priority) (*gemini.Decision, error)
	Review(prompt string) (*gemini.Review, error)
	Remaining() int
}

// batchEntry is one symbol due for evaluation this cycle.
type batchEntry struct {
	symbol   string
	ind      *indicators.Result
	sellOnly bool
	canBuy   bool
	prio     gemini.Priority
	position *portfolio.Position
	in       evalInput // what the evaluation gate records once the symbol is answered

	// Filled by buildEntry for the symbols that make the batch.
	input    gemini.PromptInput // full per-symbol prompt input (Portfolio = the whole block)
	fit      string             // the symbol's own CANDIDATE FIT lines, without the shared block
	articles []news.Article     // every cached headline for the symbol, newest first
	pfText   string             // the PORTFOLIO text the symbol's prompt shows, for macro-catalyst checks
	corr     *analysis.Correlation
}

func (s *Scheduler) batchMax() int {
	if s.cfg.BatchMaxSymbols > 0 {
		return s.cfg.BatchMaxSymbols
	}
	return defaultBatchMax
}

// due decides whether symbol should be evaluated this cycle: it applies the
// "nothing actionable" check and the evaluation gate. A nil result means skip.
// sellOnly marks held symbols that are outside the validated universe.
func (s *Scheduler) due(symbol string, ind *indicators.Result, sellOnly bool, prio gemini.Priority, cs *cycleState) *batchEntry {
	position := cs.positions[symbol]
	canBuy := !sellOnly && cs.buyBlock == "" && (position != nil || len(cs.positions) < s.cfg.MaxOpenPositions)
	if position == nil && !canBuy {
		// Nothing actionable (long-only, and BUY is not allowed): save the Gemini call.
		return nil
	}

	in := evalInput{Symbol: symbol, Held: position != nil, Price: ind.Price, RSI: ind.RSI, Headlines: s.headlinesFor(symbol), Now: time.Now()}
	ok, skip, why := s.gate.Check(in)
	if !ok {
		if skip == "no setup" {
			cs.skipNoSetup++
		} else {
			cs.skipCooldown++
		}
		if s.cfg.Debug {
			log.Printf("[%s] skipped (%s)", symbol, skip)
		}
		return nil
	}
	if s.cfg.Debug {
		log.Printf("[%s] due: %s", symbol, why)
	}
	return &batchEntry{symbol: symbol, ind: ind, sellOnly: sellOnly, canBuy: canBuy, prio: prio, position: position, in: in}
}

// selectBatch picks the symbols one Gemini call will carry: held first, then
// core, then candidates (the order within a class is kept), at most max. The
// rest are returned as deferred; they were not recorded by the evaluation
// gate, so they are due again next cycle.
func selectBatch(due []*batchEntry, max int) (batch, deferred []*batchEntry) {
	sorted := append([]*batchEntry(nil), due...)
	rank := func(p gemini.Priority) int { // held, then core, then candidate
		switch p {
		case gemini.PrioHeld:
			return 0
		case gemini.PrioCore:
			return 1
		}
		return 2
	}
	sort.SliceStable(sorted, func(i, j int) bool { return rank(sorted[i].prio) < rank(sorted[j].prio) })
	if max > 0 && len(sorted) > max {
		return sorted[:max], sorted[max:]
	}
	return sorted, nil
}

// buildEntry computes the prompt context of a symbol that made the batch:
// stop level, headlines, position age, last decision, and its part of the
// PORTFOLIO block. shared is the cycle's shared block (no candidate).
func (s *Scheduler) buildEntry(e *batchEntry, cs *cycleState, shared string) {
	symbol, ind, position := e.symbol, e.ind, e.position
	bondInfo, isBond := portfolio.BondInfoFor(symbol)
	stop := 0.0
	if isBond {
		if !bondInfo.CashLike {
			if st, err := risk.StopBelow(ind.Price, ind.ATR, risk.BondStopATRMultiple); err == nil {
				stop = st
			}
		}
	} else if l, err := risk.Bracket(ind.Price, ind.ATR, 0); err == nil {
		stop = l.Stop
	}

	e.articles = s.newsFor(symbol)
	daysHeld := -1
	if position != nil && cs.history != nil {
		if age, ok := cs.history.PositionAge(symbol); ok {
			daysHeld = age
		}
	}
	var last *gemini.LastDecision
	if l, ok := s.lastDecisions[symbol]; ok {
		last = &l
	}

	// PORTFOLIO block: computed in Go. A flat symbol also gets its fit (average
	// correlation to holdings, effect on sleeve and sector weights, IPS room).
	if cs.pf != nil {
		var cand *analysis.Candidate
		if position == nil && e.canBuy {
			cand = &analysis.Candidate{Symbol: symbol, Sector: ind.Sector, AddPct: candidateAddPct}
			if s.cfg.Analyzer != nil {
				if c, ok := s.cfg.Analyzer.CorrelationTo(symbol, heldSymbols(cs)); ok {
					e.corr, cand.Corr = &c, &c
				}
			}
			cand.Room, cand.Bind = s.policy.Room(s.bookFrom(cs, cs.account), symbol, ind.Sector)
		}
		e.pfText = analysis.Render(cs.pf.in, cand)
		e.fit = strings.TrimPrefix(e.pfText, shared)
	}

	e.input = gemini.PromptInput{
		Symbol:     symbol,
		Indicators: ind,
		Position:   position,
		News:       e.articles,
		StopLoss:   stop,
		DaysHeld:   daysHeld,
		Last:       last,
		Portfolio:  e.pfText,
		BondETF:    isBond,
		CashLike:   isBond && bondInfo.CashLike,
	}
}

// runBatch evaluates the due symbols with one Gemini call, falls back to one
// call per symbol only when the budget can afford it, and queues every answer
// that parses as a pending decision for the end-of-cycle guards.
func (s *Scheduler) runBatch(cs *cycleState, due []*batchEntry) {
	if len(due) == 0 || cs.stopGemini || s.llm == nil {
		return
	}
	batch, deferred := selectBatch(due, s.batchMax())
	cs.deferred = len(deferred)
	if len(deferred) > 0 {
		var names []string
		for _, e := range deferred {
			names = append(names, e.symbol)
		}
		log.Printf("[Batch] %d symbols due, batch cap %d: deferred to the next cycle: %s", len(due), s.batchMax(), strings.Join(names, ", "))
	}

	shared := ""
	if cs.pf != nil {
		shared = analysis.Render(cs.pf.in, nil)
	}
	inputs := make([]gemini.PromptInput, len(batch))
	prio := gemini.PrioCandidate
	for i, e := range batch {
		s.buildEntry(e, cs, shared)
		in := e.input
		in.Portfolio = e.fit
		inputs[i] = in
		cs.batchSyms = append(cs.batchSyms, e.symbol)
		if e.prio > prio {
			prio = e.prio
		}
	}

	results, err := s.llm.DecideBatch(gemini.BuildBatchPrompt(shared, inputs), prio)
	if err != nil {
		s.batchFailed(cs, batch, err)
		return
	}
	s.applyBatch(cs, batch, results)
}

// stopOnQuota flags the cycle when err means Gemini cannot be used any more
// today and reports whether it did.
func stopOnQuota(cs *cycleState, err error) bool {
	if errors.Is(err, gemini.ErrBudgetExhausted) || errors.Is(err, gemini.ErrQuotaExhausted) || errors.Is(err, gemini.ErrNoModel) {
		cs.stopGemini = true
		return true
	}
	return false
}

// batchFailed handles a failed batch call. Quota, budget or no-model errors
// end the cycle's Gemini use. Any other failure (503 after the client's own
// retries, a reply that is not a JSON array) falls back to one call per
// symbol only if the remaining daily budget exceeds the number of symbols;
// otherwise the cycle is skipped, because per-symbol calls would eat the day.
func (s *Scheduler) batchFailed(cs *cycleState, batch []*batchEntry, err error) {
	if stopOnQuota(cs, err) {
		log.Printf("[Batch] Gemini unavailable: %v — no decisions this cycle", err)
		return
	}
	remaining := s.llm.Remaining()
	if remaining <= len(batch) {
		log.Printf("[Batch] call failed (%v); %d calls left today is not more than the %d symbols, so no per-symbol fallback — skipping this cycle", err, remaining, len(batch))
		return
	}
	log.Printf("[Batch] call failed (%v); %d calls left today for %d symbols — falling back to one call per symbol", err, remaining, len(batch))
	for _, e := range batch {
		d, err := s.llm.DecideAt(gemini.BuildPrompt(e.input), e.prio)
		if err != nil {
			if stopOnQuota(cs, err) {
				log.Printf("[%s] Gemini: %v — no more decisions this cycle", e.symbol, err)
				return
			}
			log.Printf("[%s] Gemini error: %v", e.symbol, err)
			continue
		}
		s.accept(cs, e, d, gemini.MaxPromptHeadlines)
	}
}

// applyBatch matches the batch answer to the symbols asked about. Every item
// is handled on its own: an invalid item, an unknown symbol or a duplicate is
// logged and skipped without touching the others, and a symbol the model left
// out is treated as HOLD.
func (s *Scheduler) applyBatch(cs *cycleState, batch []*batchEntry, results []gemini.BatchResult) {
	asked := make(map[string]*batchEntry, len(batch))
	for _, e := range batch {
		asked[e.symbol] = e
	}
	answered := map[string]bool{}
	for _, r := range results {
		e := asked[r.Symbol]
		switch {
		case r.Symbol == "":
			cs.drop("?", "item", "invalid-item", fmt.Sprintf("batch item without a symbol: %v", r.Err))
		case e == nil:
			log.Printf("[%s] batch item ignored: not one of the symbols asked about", r.Symbol)
		case answered[r.Symbol]:
			log.Printf("[%s] duplicate batch item ignored", r.Symbol)
		case r.Err != nil || r.Decision == nil:
			answered[r.Symbol] = true
			s.gate.Record(e.in) // the call was spent; do not ask again every cycle
			cs.drop(r.Symbol, "item", "invalid-item", fmt.Sprintf("unusable batch item: %v", r.Err))
		default:
			answered[r.Symbol] = true
			s.accept(cs, e, r.Decision, gemini.BatchHeadlines)
		}
	}
	for _, e := range batch {
		if !answered[e.symbol] {
			cs.missing++
			s.gate.Record(e.in)
			log.Printf("[%s] missing from the batch response: treated as HOLD", e.symbol)
		}
	}
}

// accept takes one model answer for e: it is clamped, logged, remembered and
// queued for the end-of-cycle execution pass, where every guard (catalyst,
// conviction, risk cap, IPS, trade caps, hold period, cooldowns) is applied.
// maxHeadlines is how many headlines the prompt showed, for catalyst checks.
func (s *Scheduler) accept(cs *cycleState, e *batchEntry, decision *gemini.Decision, maxHeadlines int) {
	symbol := e.symbol
	s.gate.Record(e.in)
	switch decision.Action {
	case "BUY", "SELL", "HOLD":
	default:
		cs.drop(symbol, "item", "invalid-action", fmt.Sprintf("unknown action %q", decision.Action))
		return
	}
	cs.evaluated++
	if decision.Conviction < 0 {
		decision.Conviction = 0
	} else if decision.Conviction > 10 {
		decision.Conviction = 10
	}
	log.Printf("[%s] %s conviction=%d role=%s | %s", symbol, decision.Action, decision.Conviction, decision.Role, decision.Thesis)
	if s.cfg.Debug {
		log.Printf("[%s] reasoning: %s | catalyst: %s | portfolio: %s", symbol, decision.Reasoning, decision.Catalyst, decision.Rationale)
	}
	if n := len(decision.SuggestedTickers); n > 0 && s.cfg.Universe != nil {
		if n > maxSuggestions {
			n = maxSuggestions
		}
		s.cfg.Universe.Suggest(decision.SuggestedTickers[:n])
	}
	s.lastDecisions[symbol] = gemini.LastDecision{Action: decision.Action, Thesis: decision.Thesis, Date: trades.Date(cs.now)}

	// The headlines the prompt showed, in order: the catalyst must be one of them.
	var shown []string
	for i, a := range e.articles {
		if i >= maxHeadlines {
			break
		}
		shown = append(shown, a.Headline)
	}
	cs.pending = append(cs.pending, pendingDecision{
		symbol: symbol, ind: e.ind, position: e.position, decision: decision, sellOnly: e.sellOnly,
		headlines: shown, portfolioText: e.pfText, corr: e.corr,
	})
}
