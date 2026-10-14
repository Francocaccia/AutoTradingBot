package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/francocaccia/quantengine/internal/analysis"
	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/market"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/regime"
	"github.com/francocaccia/quantengine/internal/risk"
	"github.com/francocaccia/quantengine/internal/stats"
	"github.com/francocaccia/quantengine/internal/trades"
	"github.com/francocaccia/quantengine/internal/universe"
)

const (
	maxSuggestions = 3
	// minCatalystLen is the shortest quote accepted as a catalyst (or as a
	// headline contained in one), so a stray word cannot "match" everything.
	minCatalystLen = 12
	// maxCorrelation is the average 60-day correlation to current holdings above
	// which a BUY needs "replaces".
	maxCorrelation = 0.8
	// bondStepPct is the most a single bond-sleeve purchase may add, % of equity.
	bondStepPct = 5.0
	// candidateAddPct is the hypothetical purchase size shown in CANDIDATE FIT.
	candidateAddPct = 5.0
	// reviewBreachPts is how far outside a band a sleeve must be to trigger an
	// extra review.
	reviewBreachPts = 5.0
)

// orderPlacer is the part of the Alpaca client that places and cancels orders.
type orderPlacer interface {
	PlaceBracketOrder(symbol string, qty int, side broker.Side, stopLoss, takeProfit float64) error
	PlaceOTOOrder(symbol string, qty int, side broker.Side, stopLoss float64) error
	PlaceMarketOrder(symbol string, qty int, side broker.Side) error
	CancelOpenOrders(symbol string) (int, error)
}

// accountSource fetches the live account.
type accountSource interface {
	GetAccount() (*portfolio.Account, error)
}

type Config struct {
	Broker    *broker.AlpacaClient
	News      *news.AlpacaNewsClient
	Gemini    *gemini.Client
	Portfolio *portfolio.Portfolio
	Universe  *universe.Universe
	Stats     *stats.Service // equity peak for the drawdown halt, benchmark performance

	MaxPositionPct   float64 // % of equity per position (hard cap)
	MaxPositionUSD   float64 // optional absolute cap; <= 0 disables
	MaxOpenPositions int
	Debug            bool // log Gemini's full reasoning

	CycleInterval time.Duration // market cycle period; default 15m
	EvalCooldown  time.Duration // min time between Gemini evaluations of a HELD symbol; 0 disables

	MaxRiskPct          float64 // cap on qty*(entry-stop) as % of equity; <= 0 disables
	MinBuyConviction    int     // BUY needs at least this conviction; <= 0 means risk.DefaultMinConviction
	MinHoldDays         int     // no SELL of a position younger than this many trading days; 0 disables
	ReentryCooldownDays int     // no BUY this many trading days after a sell; 0 disables
	MaxTradesPerDay     int     // bot-decision orders per day; <= 0 means unlimited
	MaxTradesPerWeek    int     // bot-decision orders per rolling 5 trading days; <= 0 means unlimited

	// History supplies filled orders and the trading calendar for the holding,
	// cooldown and turnover guards. Without it BUYs are blocked.
	History trades.Source

	// Policy is the investment policy statement (zero value = DefaultPolicy).
	Policy portfolio.Policy
	// Bonds is the set of bond ETFs the bot may buy (nil = the whole curated
	// table; portfolio.DisabledBondUniverse() = BOND_MODE=off).
	Bonds *portfolio.BondUniverse
	// Analyzer supplies volatility, beta and correlation; nil disables them.
	Analyzer *analysis.Analyzer
	// Macro fetches yields/VIX/trend data; nil uses indicators.GetMacro.
	Macro func() (indicators.Macro, error)
}

const defaultCycleInterval = 15 * time.Minute

type Scheduler struct {
	cfg        Config
	ctx        context.Context
	cancel     context.CancelFunc
	newsCache  []news.Article
	sectors    map[string]string // symbol -> sector, learned from indicator fetches
	dayHaltFor string            // session date for which the daily-loss breaker has tripped
	paused     bool
	gate       *evalGate

	orders orderPlacer
	acct   accountSource
	// lastDecisions is the model's previous answer per symbol, shown back to it
	// so it can see when it is about to reverse itself. Best effort: in memory.
	lastDecisions map[string]gemini.LastDecision

	policy   portfolio.Policy // IPS; its targets move with the weekly review
	lastPeak float64          // equity high-water mark seen by checkBuyBlock

	lastReviewDay     string // session date of the last weekly review call
	lastBandReviewDay string // session date of the last band-breach review call

	// fetchBond loads indicators for a bond ETF proposed by the review.
	fetchBond func(symbol string) (*indicators.Result, error)
}

func New(cfg Config) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.CycleInterval <= 0 {
		cfg.CycleInterval = defaultCycleInterval
	}
	if cfg.Policy == (portfolio.Policy{}) {
		cfg.Policy = portfolio.DefaultPolicy()
	}
	if cfg.Bonds == nil {
		cfg.Bonds = portfolio.NewBondUniverse(nil)
	}
	if cfg.Macro == nil {
		cfg.Macro = indicators.GetMacro
	}
	s := &Scheduler{cfg: cfg, ctx: ctx, cancel: cancel, sectors: map[string]string{}, gate: newEvalGate(cfg.EvalCooldown),
		lastDecisions: map[string]gemini.LastDecision{}, policy: cfg.Policy}
	if cfg.Broker != nil {
		s.orders = cfg.Broker
	}
	if cfg.Portfolio != nil {
		s.acct = cfg.Portfolio
	}
	if cfg.Universe != nil {
		s.fetchBond = cfg.Universe.Validate
	}
	return s
}

func (s *Scheduler) Pause()         { s.paused = true }
func (s *Scheduler) Resume()        { s.paused = false }
func (s *Scheduler) IsPaused() bool { return s.paused }

func (s *Scheduler) Start() {
	log.Printf("Scheduler started (cycle every %s, eval cooldown %s); first decision cycle at the next tick", s.cfg.CycleInterval, s.cfg.EvalCooldown)
	// No decision cycle at process start: redeploys would burn Gemini quota.
	s.fetchAndCacheNews()
	newsTicker := time.NewTicker(4 * time.Hour)
	marketTicker := time.NewTicker(s.cfg.CycleInterval)
	for {
		select {
		case <-s.ctx.Done():
			newsTicker.Stop()
			marketTicker.Stop()
			return
		case <-newsTicker.C:
			s.fetchAndCacheNews()
		case <-marketTicker.C:
			s.runMarketCycle()
		}
	}
}

func (s *Scheduler) Stop() { s.cancel() }

// fetchAndCacheNews caches news for the core watchlist plus general market
// news (the source of new universe candidates), de-duplicated by headline.
func (s *Scheduler) fetchAndCacheNews() {
	log.Println("[News] Fetching latest news...")
	var all []news.Article
	seen := map[string]bool{}
	for _, symbols := range [][]string{s.cfg.Universe.Core, nil} {
		limit := 20
		if symbols == nil {
			limit = 30
		}
		articles, err := s.cfg.News.FetchLatest(symbols, limit)
		if err != nil {
			log.Printf("[News] Error: %v", err)
			continue
		}
		for _, a := range articles {
			if !seen[a.Headline] {
				seen[a.Headline] = true
				all = append(all, a)
			}
		}
	}
	if len(all) == 0 {
		return // keep the previous cache
	}
	s.newsCache = all
	log.Printf("[News] Cached %d articles", len(all))
}

// newsFor returns the cached articles that mention symbol, falling back to
// the newest general headlines.
func (s *Scheduler) newsFor(symbol string) []news.Article {
	var out []news.Article
	for _, a := range s.newsCache {
		for _, sym := range a.Symbols {
			if sym == symbol {
				out = append(out, a)
				break
			}
		}
	}
	if len(out) == 0 && len(s.newsCache) > 5 {
		return s.newsCache[:5]
	}
	return out
}

func (s *Scheduler) newsSymbols() []string {
	var out []string
	for _, a := range s.newsCache {
		out = append(out, a.Symbols...)
	}
	return out
}

// pfContext is the portfolio-level view computed once per cycle.
type pfContext struct {
	in     analysis.Inputs
	regime regime.Regime
}

// cycleState is the account/position snapshot shared by one decision cycle.
type cycleState struct {
	account   *portfolio.Account
	positions map[string]*portfolio.Position
	buyBlock  string // non-empty: no new BUYs this cycle, with the reason

	stopGemini   bool // Gemini quota/budget hit: no more decisions this cycle
	evaluated    int
	skipCooldown int
	skipNoSetup  int
	skipNoNews   int // screener candidates with no cached headline mentioning them

	now     time.Time // Alpaca clock, ET
	breaker bool      // a circuit breaker (daily loss / drawdown) is active

	// history is nil when order history could not be loaded; BUYs are then
	// blocked because the turnover limits cannot be checked.
	history    *trades.Snapshot
	tradesDay  int // bot-decision orders filled today, plus placed this cycle
	tradesWeek int // same over the rolling 5 trading days

	pf *pfContext // nil only when the account snapshot is unavailable

	pending     []pendingDecision // collected during evaluation, executed afterwards
	counts      decisionCounts
	blocked     int            // decisions a guard refused to execute
	dropReasons map[string]int // guard key -> count
	sold        map[string]bool
}

// decisionCounts tallies what the model answered this cycle.
type decisionCounts struct{ buy, sell, hold int }

// pendingDecision is one model answer waiting for the end-of-cycle execution
// pass, which orders trades by priority when the turnover caps bind.
type pendingDecision struct {
	symbol        string
	ind           *indicators.Result
	position      *portfolio.Position
	decision      *gemini.Decision
	sellOnly      bool
	headlines     []string // the headlines shown to the model, for catalyst verification
	portfolioText string   // the PORTFOLIO block shown to the model, for macro-catalyst verification
	corr          *analysis.Correlation
	fromReview    bool // came from the weekly review (SELLs are exempt from MIN_HOLD_DAYS)
}

// drop records a decision a guard refused to execute.
func (cs *cycleState) drop(symbol, action, key, why string) {
	log.Printf("[%s] %s skipped: %s", symbol, action, why)
	cs.blocked++
	if cs.dropReasons == nil {
		cs.dropReasons = map[string]int{}
	}
	cs.dropReasons[key]++
}

// headlinesFor returns the cached headlines that mention symbol (no
// general-news fallback); the evaluation gate uses them to detect new news.
func (s *Scheduler) headlinesFor(symbol string) []string {
	var out []string
	for _, a := range s.newsCache {
		for _, sym := range a.Symbols {
			if sym == symbol {
				out = append(out, a.Headline)
				break
			}
		}
	}
	return out
}

func (s *Scheduler) runMarketCycle() {
	if s.paused {
		log.Println("[Scheduler] Paused — skipping cycle")
		return
	}
	sess, err := market.GetSession(s.cfg.Broker)
	if err != nil {
		log.Printf("[Market] Session check failed: %v — skipping cycle", err)
		return
	}
	if !sess.IsOpen {
		log.Println("[Market] Closed — skipping cycle")
		return
	}
	log.Println("[Market] Open — running decision cycle")

	// With Gemini paused (daily quota or local budget) no decisions are made.
	// Stops and take-profits live in server-side bracket orders, so they keep working.
	if paused, why := s.cfg.Gemini.Paused(); paused {
		used, budget := s.cfg.Gemini.Usage()
		log.Printf("[Gemini] paused (%s, %d/%d calls) — no decisions this cycle; bracket orders stay active", why, used, budget)
		return
	}

	acct, err := s.cfg.Portfolio.GetAccount()
	if err != nil {
		log.Printf("[Account] fetch failed: %v — skipping cycle", err)
		return
	}
	posList, err := s.cfg.Portfolio.GetAllPositions()
	if err != nil {
		log.Printf("[Positions] fetch failed: %v — skipping cycle", err)
		return
	}
	cs := &cycleState{
		account:     acct,
		positions:   map[string]*portfolio.Position{},
		now:         sess.Now,
		dropReasons: map[string]int{},
		sold:        map[string]bool{},
	}
	for i := range posList {
		p := posList[i]
		cs.positions[p.Symbol] = &p
	}
	cs.buyBlock, cs.breaker = s.checkBuyBlock(sess, acct)
	if cs.buyBlock != "" {
		log.Printf("[Risk] new BUYs blocked: %s (SELL/exits still active)", cs.buyBlock)
	}

	// Position ages, re-entry cooldowns and turnover counts come from Alpaca's
	// order history, never from local state: restarts wipe memory and disk.
	if s.cfg.History != nil {
		if snap, err := trades.Load(s.cfg.History, sess.Now); err != nil {
			log.Printf("[Trades] order history unavailable: %v — BUYs blocked this cycle, SELLs not restricted", err)
		} else {
			cs.history = snap
			cs.tradesDay, cs.tradesWeek = snap.DecisionCounts()
		}
	} else {
		log.Printf("[Trades] no order history source configured — BUYs blocked this cycle")
	}

	// The bot manages every open position, whether or not it is in the core
	// list or the candidate pool. Held symbols go first so they win when the
	// Gemini budget is tight.
	held := make([]string, 0, len(cs.positions))
	for sym := range cs.positions {
		held = append(held, sym)
	}
	sort.Strings(held)
	isHeld := map[string]bool{}
	for _, sym := range held {
		isHeld[sym] = true
	}
	s.cfg.Universe.Held = held
	indicators.Prefetch(append(append([]string{}, s.cfg.Universe.Core...), held...)) // one batched fetch

	items := s.cfg.Universe.Build(s.newsSymbols())
	inUniverse := map[string]*universe.Item{}
	for i := range items {
		inUniverse[items[i].Symbol] = &items[i]
		s.noteSector(items[i].Symbol, items[i].Ind)
	}

	// Portfolio-level context (weights, risk, regime) once per cycle, then the
	// weekly review, whose actions join this cycle's decisions.
	s.buildPortfolio(cs)
	s.maybeReview(cs, sess)

	for _, sym := range held {
		if cs.stopGemini {
			break
		}
		if it := inUniverse[sym]; it != nil {
			s.evaluate(sym, it.Ind, false, cs)
			continue
		}
		// Outside the universe or failing the BUY filters (e.g. a low-volume
		// ETF or a bond ETF): still managed, SELL-only. The filters gate new
		// positions only; bond ETF purchases come from the weekly review.
		ind, err := indicators.Calculate(sym)
		if err != nil {
			log.Printf("[%s] WARNING: indicators unavailable for held position: %v — existing bracket orders left untouched", sym, err)
			continue
		}
		s.noteSector(sym, ind)
		s.evaluate(sym, ind, true, cs)
	}
	for _, it := range items {
		if cs.stopGemini {
			break
		}
		if isHeld[it.Symbol] {
			continue
		}
		// A screener candidate must have at least one cached headline that
		// mentions its ticker before it is worth a Gemini call.
		if s.noHeadlineCandidate(it) {
			cs.skipNoNews++
			if s.cfg.Debug {
				log.Printf("[%s] candidate skipped (no cached headline mentions it)", it.Symbol)
			}
			continue
		}
		s.evaluate(it.Symbol, it.Ind, false, cs)
	}

	s.executeDecisions(cs)
	s.logCycleSummary(cs)
}

// logCycleSummary prints the one-line cycle summary. buys/sells/holds are the
// model's answers; dropped are answers a guard refused, with reasons; skipped
// are symbols never sent to Gemini.
func (s *Scheduler) logCycleSummary(cs *cycleState) {
	used, budget := s.cfg.Gemini.Usage()
	skipped := cs.skipCooldown + cs.skipNoSetup + cs.skipNoNews
	dropped := fmt.Sprintf("%d", cs.blocked)
	if len(cs.dropReasons) > 0 {
		keys := make([]string, 0, len(cs.dropReasons))
		for k := range cs.dropReasons {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dropped += " ("
		for i, k := range keys {
			if i > 0 {
				dropped += ", "
			}
			dropped += fmt.Sprintf("%s x%d", k, cs.dropReasons[k])
		}
		dropped += ")"
	}
	log.Printf("[Cycle] evaluated=%d holds=%d buys=%d sells=%d dropped=%s not-evaluated=%d | trades today %d/%s, week %d/%s | Gemini calls today %d/%d",
		cs.evaluated, cs.counts.hold, cs.counts.buy, cs.counts.sell, dropped, skipped,
		cs.tradesDay, capLabel(s.cfg.MaxTradesPerDay), cs.tradesWeek, capLabel(s.cfg.MaxTradesPerWeek), used, budget)
}

// noHeadlineCandidate reports whether it is a screener candidate (not core)
// that no cached headline mentions.
func (s *Scheduler) noHeadlineCandidate(it universe.Item) bool {
	return !it.Core && len(s.headlinesFor(it.Symbol)) == 0
}

func capLabel(n int) string {
	if n <= 0 {
		return "inf"
	}
	return fmt.Sprint(n)
}

// checkBuyBlock applies the circuit breakers and the open/close buffer and
// returns the reason new BUYs are blocked ("" if allowed). breaker is true
// when a circuit breaker (daily loss or drawdown) is the cause.
func (s *Scheduler) checkBuyBlock(sess *market.Session, acct *portfolio.Account) (reason string, breaker bool) {
	var peak float64
	if s.cfg.Stats != nil {
		peak = s.cfg.Stats.ObservePeak(acct.Equity)
	}
	s.lastPeak = peak
	dayStop, ddHalt, dayPct, ddPct := risk.Breakers(acct.Equity, acct.LastEquity, peak)
	if ddHalt {
		reason := fmt.Sprintf("DRAWDOWN HALT: equity $%.2f is %.1f%% below peak $%.2f (limit %.0f%%)", acct.Equity, ddPct, peak, risk.MaxDrawdownPct)
		log.Printf("[RISK] !!!!!!!! %s — BUYs halted until drawdown recovers !!!!!!!!", reason)
		return reason, true
	}
	if dayStop {
		s.dayHaltFor = sess.Date
	}
	if s.dayHaltFor == sess.Date {
		return fmt.Sprintf("daily loss breaker: equity %.2f%% vs day start (limit -%.0f%%), no new positions until next session", dayPct, risk.DailyLossLimitPct), true
	}
	if !sess.TradingDay {
		return "trading calendar unavailable for today", false
	}
	if !market.EntryWindowOpen(sess.Now, sess.Open, sess.Close, risk.EntryBuffer) {
		return fmt.Sprintf("within %v of the open or close", risk.EntryBuffer), false
	}
	return "", false
}

func (s *Scheduler) noteSector(symbol string, ind *indicators.Result) {
	if ind != nil && ind.Sector != "" {
		s.sectors[symbol] = ind.Sector
	}
}

// bookFrom turns the cycle's positions and an account snapshot into the
// policy's view of the portfolio.
func (s *Scheduler) bookFrom(cs *cycleState, acct *portfolio.Account) portfolio.Book {
	b := portfolio.Book{Equity: acct.Equity, Cash: acct.Cash}
	syms := make([]string, 0, len(cs.positions))
	for sym := range cs.positions {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	for _, sym := range syms {
		b.Holdings = append(b.Holdings, portfolio.Holding{Symbol: sym, Value: cs.positions[sym].MarketVal, Sector: s.sectors[sym]})
	}
	return b
}

// heldSymbols lists the symbols currently held, sorted.
func heldSymbols(cs *cycleState) []string {
	out := make([]string, 0, len(cs.positions))
	for sym := range cs.positions {
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}

// buildPortfolio computes the cycle's portfolio context: sleeve weights,
// volatility and beta, drawdown, benchmark performance and the macro regime.
// Every part degrades to "unavailable" instead of failing the cycle.
func (s *Scheduler) buildPortfolio(cs *cycleState) {
	if cs.account == nil {
		return
	}
	book := s.bookFrom(cs, cs.account)
	in := analysis.Inputs{Policy: s.policy, Book: book}

	if macro, err := s.cfg.Macro(); err != nil {
		log.Printf("[Regime] macro data unavailable: %v — regime unavailable, bond purchases limited to SGOV/BIL/SHY", err)
		in.Regime = regime.Unavailable()
	} else {
		in.Regime = regime.Classify(macro)
		if !in.Regime.Available {
			log.Printf("[Regime] macro data incomplete — regime unavailable, bond purchases limited to SGOV/BIL/SHY")
		}
	}

	if s.cfg.Analyzer != nil && book.Equity > 0 {
		weights := map[string]float64{}
		var invested float64
		for _, h := range book.Holdings {
			weights[h.Symbol] = h.Value
			invested += h.Value
		}
		in.Vol, in.Beta, in.HaveRisk = s.cfg.Analyzer.PortfolioRisk(weights, invested/book.Equity)
	}
	if s.lastPeak > 0 && book.Equity > 0 {
		in.DrawdownPct, in.HaveDD = max((s.lastPeak-book.Equity)/s.lastPeak*100, 0), true
	}
	if s.cfg.Stats != nil {
		in.BotReturn, in.SPYReturn, in.Bench6040 = s.cfg.Stats.Performance()
	}
	cs.pf = &pfContext{in: in, regime: in.Regime}
	if s.cfg.Debug {
		log.Printf("[Portfolio] regime=%s", in.Regime.Label)
	}
}

// evaluate asks Gemini about one symbol and queues the answer for the
// end-of-cycle execution pass. sellOnly marks held symbols that are outside
// the validated universe.
func (s *Scheduler) evaluate(symbol string, ind *indicators.Result, sellOnly bool, cs *cycleState) {
	position := cs.positions[symbol]
	canBuy := !sellOnly && cs.buyBlock == "" && (position != nil || len(cs.positions) < s.cfg.MaxOpenPositions)
	if position == nil && !canBuy {
		// Nothing actionable (long-only, and BUY is not allowed): save the Gemini call.
		return
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
		return
	}
	if s.cfg.Debug {
		log.Printf("[%s] evaluating: %s", symbol, why)
	}

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

	articles := s.newsFor(symbol)
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
	var pfText string
	var corr *analysis.Correlation
	if cs.pf != nil {
		var cand *analysis.Candidate
		if position == nil && canBuy {
			cand = &analysis.Candidate{Symbol: symbol, Sector: ind.Sector, AddPct: candidateAddPct}
			if s.cfg.Analyzer != nil {
				if c, ok := s.cfg.Analyzer.CorrelationTo(symbol, heldSymbols(cs)); ok {
					corr, cand.Corr = &c, &c
				}
			}
			cand.Room, cand.Bind = s.policy.Room(s.bookFrom(cs, cs.account), symbol, ind.Sector)
		}
		pfText = analysis.Render(cs.pf.in, cand)
	}

	prompt := gemini.BuildPrompt(gemini.PromptInput{
		Symbol:     symbol,
		Indicators: ind,
		Position:   position,
		News:       articles,
		StopLoss:   stop,
		DaysHeld:   daysHeld,
		Last:       last,
		Portfolio:  pfText,
		BondETF:    isBond,
		CashLike:   isBond && bondInfo.CashLike,
	})
	decision, err := s.cfg.Gemini.Decide(prompt)
	if err != nil {
		if errors.Is(err, gemini.ErrQuotaExhausted) || errors.Is(err, gemini.ErrBudgetExhausted) {
			cs.stopGemini = true
			log.Printf("[%s] Gemini unavailable (%v) — no more decisions this cycle", symbol, err)
			return
		}
		log.Printf("[%s] Gemini error: %v", symbol, err)
		return
	}
	cs.evaluated++
	s.gate.Record(in)
	if decision.Conviction < 0 {
		decision.Conviction = 0
	} else if decision.Conviction > 10 {
		decision.Conviction = 10
	}
	log.Printf("[%s] %s conviction=%d role=%s | %s", symbol, decision.Action, decision.Conviction, decision.Role, decision.Thesis)
	if s.cfg.Debug {
		log.Printf("[%s] reasoning: %s | catalyst: %s | portfolio: %s", symbol, decision.Reasoning, decision.Catalyst, decision.Rationale)
	}
	if n := len(decision.SuggestedTickers); n > 0 {
		if n > maxSuggestions {
			n = maxSuggestions
		}
		s.cfg.Universe.Suggest(decision.SuggestedTickers[:n])
	}
	s.lastDecisions[symbol] = gemini.LastDecision{Action: decision.Action, Thesis: decision.Thesis, Date: trades.Date(cs.now)}

	// The headlines the prompt showed, in order: the catalyst must be one of them.
	var shown []string
	for i, a := range articles {
		if i >= gemini.MaxPromptHeadlines {
			break
		}
		shown = append(shown, a.Headline)
	}
	cs.pending = append(cs.pending, pendingDecision{
		symbol: symbol, ind: ind, position: position, decision: decision, sellOnly: sellOnly,
		headlines: shown, portfolioText: pfText, corr: corr,
	})
}
