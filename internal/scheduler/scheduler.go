package scheduler

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
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
)

// orderPlacer is the part of the Alpaca client that places and cancels orders.
//
// The kind (broker.KindEntry, KindExit, KindRebalance) is part of the
// client_order_id, which is derived from the date, symbol, side and kind: a
// second order for the same thing the same day is refused with
// broker.ErrDuplicateOrder.
type orderPlacer interface {
	PlaceBracketOrder(symbol string, qty int, side broker.Side, stopLoss, takeProfit float64, kind string) error
	PlaceOTOOrder(symbol string, qty int, side broker.Side, stopLoss float64, kind string) error
	PlaceMarketOrder(symbol string, qty int, side broker.Side, kind string) error
	CancelOpenOrders(symbol string) (int, error)
	// CheckOrder returns broker.ErrDuplicateOrder (or a lookup error) when an
	// order of this symbol, side and kind could not be placed now. It sends nothing.
	CheckOrder(symbol string, side broker.Side, kind string) error
}

// accountSource fetches the live account.
type accountSource interface {
	GetAccount() (*portfolio.Account, error)
}

// positionSource lists the open positions.
type positionSource interface {
	GetAllPositions() ([]portfolio.Position, error)
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
	// BatchMaxSymbols caps how many symbols one Gemini batch call carries
	// (<= 0 means defaultBatchMax); the rest wait for the next cycle.
	BatchMaxSymbols int

	MaxRiskPct          float64 // cap on qty*(entry-stop) as % of equity; <= 0 disables
	MinBuyConviction    int     // BUY needs at least this conviction; <= 0 means risk.DefaultMinConviction
	MinHoldDays         int     // no SELL of a position younger than this many trading days; 0 disables
	ReentryCooldownDays int     // no BUY this many trading days after a sell; 0 disables
	MaxTradesPerDay     int     // bot-decision orders per day; <= 0 means unlimited
	MaxTradesPerWeek    int     // bot-decision orders per rolling 5 trading days; <= 0 means unlimited
	// GuardsStartDate (YYYY-MM-DD, ET): decisions before it do not count toward
	// the daily/weekly caps. Empty counts everything in the lookback.
	GuardsStartDate string

	// History supplies filled orders and the trading calendar for the holding,
	// cooldown and turnover guards. Without it BUYs are blocked.
	History trades.Source

	// EntryBlackoutOpen and EntryBlackoutClose are the no-new-BUY windows after
	// the open and before the close (ENTRY_BLACKOUT_OPEN_MIN / _CLOSE_MIN).
	EntryBlackoutOpen, EntryBlackoutClose time.Duration
	// MinStopATRMult is the least ATR multiple of a stock stop (MIN_STOP_ATR_MULT);
	// the stop is never closer than risk.StopATRMultiple ATRs either.
	MinStopATRMult float64
	// MinPolicyStopPct is the least distance, % of price, of the stop on a policy
	// rebalancing buy (MIN_POLICY_STOP_PCT). 0 means defaultMinPolicyStopPct; a
	// negative value means no stop at all.
	MinPolicyStopPct float64

	// IPSBandsMode says what happens when a sleeve is below its band minimum:
	// "gradual" (default) buys core ETFs in tranches, "advisory" only logs the
	// trade it would make, "off" does nothing. Bands always cap purchases.
	IPSBandsMode string
	// RebalanceTranchePct is the most one rebalancing trade buys, % of equity
	// (<= 0 means defaultRebalanceTranchePct).
	RebalanceTranchePct float64
	// RebalanceMaxTradesPerWeek caps rebalancing trades per rolling 5 trading
	// days (<= 0 means defaultRebalanceMaxWeek); at most one a day regardless.
	RebalanceMaxTradesPerWeek int
	// RebalanceBondETFs is the preference order of bond ETFs for the bond sleeve
	// (nil means defaultRebalanceBondETFs). Only enabled, tradable ones are used.
	RebalanceBondETFs []string

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

const (
	defaultCycleInterval = 15 * time.Minute
	// defaultBatchMax is the default BATCH_MAX_SYMBOLS.
	defaultBatchMax = 12
)

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
	pos    positionSource
	// lastDecisions is the model's previous answer per symbol, shown back to it
	// so it can see when it is about to reverse itself. Best effort: in memory.
	lastDecisions map[string]gemini.LastDecision

	policy   portfolio.Policy // IPS; its targets move with the weekly review
	lastPeak float64          // equity high-water mark seen by checkBuyBlock

	lastReviewDay string // session date of the last weekly review call

	// geminiPaused overrides cfg.Gemini.Paused (tests).
	geminiPaused func() (bool, string)
	// llm is the model client used for batch / per-symbol decisions (cfg.Gemini;
	// replaced in tests).
	llm decider

	// fetchBond loads indicators for a bond ETF proposed by the review.
	fetchBond func(symbol string) (*indicators.Result, error)
	// fetchRaw loads indicators without the universe filters (indicators.Calculate);
	// the rebalancing path falls back to it for a core ETF whose validation failed
	// because of a data outage.
	fetchRaw func(symbol string) (*indicators.Result, error)

	// Wall-clock scheduling (see tick): the cycle runs once per time slot of the
	// America/New_York trading day, however long the process slept in between.
	now       func() time.Time
	runCycle  func() // one decision cycle (runMarketCycle; replaced in tests)
	lastSlot  string // key of the last slot a cycle ran in
	lastCycle time.Time
	skipDay   string      // ET date on which the market is closed for good: no more slots are tried
	newsAt    time.Time   // when the news cache was last refreshed
	fetchNews func()      // reloads s.newsCache (fetchAndCacheNews; replaced in tests)
	marks     markerStore // durable facts (the weekly review marker); nil when not needed
	dry       bool        // an advisory dry run is in progress: logs say "would BUY" (see advisory.go)
	slotsRun  int
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
		lastDecisions: map[string]gemini.LastDecision{}, policy: cfg.Policy, now: time.Now, fetchRaw: indicators.Calculate}
	s.runCycle = s.runMarketCycle
	s.fetchNews = s.fetchAndCacheNews
	if cfg.Gemini != nil {
		s.llm = cfg.Gemini
	}
	if cfg.Broker != nil {
		s.orders = cfg.Broker
		s.marks = cfg.Broker
	}
	if cfg.Portfolio != nil {
		s.acct = cfg.Portfolio
		s.pos = cfg.Portfolio
	}
	if cfg.Universe != nil {
		s.fetchBond = cfg.Universe.Validate
	}
	return s
}

func (s *Scheduler) Pause()         { s.paused = true }
func (s *Scheduler) Resume()        { s.paused = false }
func (s *Scheduler) IsPaused() bool { return s.paused }

// pollEvery is how often the loop looks at the wall clock. It is cheap (no
// network call) and bounds how late a cycle can start after its slot begins.
const pollEvery = 20 * time.Second

// newsMaxAge is how old the news cache may get before a cycle refreshes it.
const newsMaxAge = 4 * time.Hour

// Start runs the scheduler until Stop. Cycles are tied to the America/New_York
// wall clock, not to time since boot: see tick.
func (s *Scheduler) Start() {
	log.Printf("Scheduler started (a cycle in each %s slot of the ET trading day, eval cooldown %s, IPS_BANDS_MODE=%s)", s.cfg.CycleInterval, s.cfg.EvalCooldown, s.bandsMode())
	s.logDecisionBreakdown(s.clock())
	s.refreshNews()
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	s.tick(s.clock()) // a process that starts (or wakes) inside a slot runs its cycle now
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.tick(s.clock())
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

	batchSyms   []string       // symbols sent in this cycle's Gemini batch
	deferred    int            // due symbols left for the next cycle by the batch cap
	missing     int            // batch symbols the model did not answer (treated as HOLD)
	usageBefore map[string]int // Gemini calls today per model when the cycle started

	pending     []pendingDecision // collected during evaluation, executed afterwards
	counts      decisionCounts
	blocked     int            // decisions a guard refused to execute
	dropReasons map[string]int // guard key -> count
	sold        map[string]bool

	due int // symbols that came out of the evaluation gate as due this cycle

	// Investment-policy rebalancing (see rebalance.go).
	rebal     rebalanceState
	rebalDay  int // rebalancing orders filled today, plus placed this cycle
	rebalWeek int // same over the rolling 5 trading days

	// What the cycle's single [Review] line reports (see reviewline.go).
	whyNot []string // reasons nothing (more) happened, in the order found
	did    []string // actions taken: "rebalance", "buy", "sell"
}

// why records a reason for the [Review] line's skip field.
func (cs *cycleState) why(reason string) {
	if reason != "" {
		cs.whyNot = append(cs.whyNot, reason)
	}
}

// took records an action taken this cycle ("rebalance", "buy" or "sell").
func (cs *cycleState) took(action string) {
	for _, a := range cs.did {
		if a == action {
			return
		}
	}
	cs.did = append(cs.did, action)
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
		s.logEarlyReview(nil, "scheduler paused")
		return
	}
	sess, err := market.GetSession(s.cfg.Broker)
	if err != nil {
		log.Printf("[Market] Session check failed: %v — skipping cycle", err)
		s.logEarlyReview(nil, "market session unknown")
		return
	}
	// A holiday, or the hours after an early close, will not get a cycle today:
	// stop asking Alpaca for the rest of the day.
	if !sess.TradingDay || (!sess.Close.IsZero() && !sess.Now.Before(sess.Close)) {
		s.skipDay = sess.Date
	}
	if !sess.IsOpen {
		log.Println("[Market] Closed — skipping cycle")
		s.logEarlyReview(sess, "market closed")
		return
	}
	log.Println("[Market] Open — running decision cycle")

	usageBefore := map[string]int{}
	if s.cfg.Gemini != nil {
		for _, u := range s.cfg.Gemini.UsageByModel() {
			usageBefore[u.Model] = u.Used
		}
	}

	acct, err := s.cfg.Portfolio.GetAccount()
	if err != nil {
		log.Printf("[Account] fetch failed: %v — skipping cycle", err)
		s.logEarlyReview(sess, "account unavailable")
		return
	}
	posList, err := s.cfg.Portfolio.GetAllPositions()
	if err != nil {
		log.Printf("[Positions] fetch failed: %v — skipping cycle", err)
		s.logEarlyReview(sess, "positions unavailable")
		return
	}
	cs := &cycleState{
		account:     acct,
		positions:   map[string]*portfolio.Position{},
		now:         sess.Now,
		dropReasons: map[string]int{},
		sold:        map[string]bool{},
		usageBefore: usageBefore,
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
			snap.SetCountStart(s.cfg.GuardsStartDate)
			cs.history = snap
			cs.tradesDay, cs.tradesWeek = snap.DecisionCounts()
		}
	} else {
		log.Printf("[Trades] no order history source configured — BUYs blocked this cycle")
	}

	// Portfolio-level context (weights, risk, regime) once per cycle.
	s.buildPortfolio(cs)

	// With Gemini paused (daily quota or local budget) there are no model
	// decisions, but the cycle still runs: the investment-policy rebalancing and
	// the bookkeeping do not need the model, and stops and take-profits live in
	// server-side bracket orders that keep working.
	if paused, why := s.geminiPausedNow(); paused {
		label := "quota"
		switch {
		case strings.Contains(why, "budget"):
			label = "budget"
		case strings.Contains(why, "no usable"), strings.Contains(why, "not configured"):
			label = "models"
		}
		log.Printf("[Gemini] %s: paused (%s) — no model decisions this cycle; bracket orders stay active", label, why)
		cs.stopGemini = true
		cs.why("Gemini paused (" + why + "): no model decisions")
	}
	if !cs.stopGemini {
		s.evaluateSymbols(cs)
	}
	s.maybeReview(cs, sess)
	s.planRebalance(cs, sess)

	s.executeDecisions(cs)
	s.logCycleSummary(cs)
	s.logReviewLine(cs, sess)
}

// geminiPausedNow reports whether model calls must be skipped this cycle, and why.
func (s *Scheduler) geminiPausedNow() (bool, string) {
	switch {
	case s.geminiPaused != nil:
		return s.geminiPaused()
	case s.cfg.Gemini != nil:
		return s.cfg.Gemini.Paused()
	}
	return true, "Gemini not configured"
}

// evaluateSymbols collects every symbol due for a model decision (held
// positions first, then the core list, then screener candidates) and asks
// Gemini about them in one batch call.
func (s *Scheduler) evaluateSymbols(cs *cycleState) {
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

	// Every symbol due for evaluation joins ONE Gemini batch call: held
	// positions first, then core symbols, then screener candidates (see
	// selectBatch for the cap). The weekly review stays a separate call: its
	// answer has a different shape and its own rules, and mixing the two made
	// the prompt unreliable.
	var due []*batchEntry
	for _, sym := range held {
		if it := inUniverse[sym]; it != nil {
			if e := s.due(sym, it.Ind, false, gemini.PrioHeld, cs); e != nil {
				due = append(due, e)
			}
			continue
		}
		// Outside the universe or failing the BUY filters (e.g. a low-volume
		// ETF or a bond ETF): still managed, SELL-only. The filters gate new
		// positions only; bond ETF purchases come from the rebalancing step.
		ind, err := indicators.Calculate(sym)
		if err != nil {
			log.Printf("[%s] WARNING: indicators unavailable for held position: %v — existing bracket orders left untouched", sym, err)
			continue
		}
		s.noteSector(sym, ind)
		if e := s.due(sym, ind, true, gemini.PrioHeld, cs); e != nil {
			due = append(due, e)
		}
	}
	for _, it := range items {
		if isHeld[it.Symbol] {
			continue
		}
		// A screener candidate must have at least one cached headline that
		// mentions its ticker before it is worth evaluating.
		if s.noHeadlineCandidate(it) {
			cs.skipNoNews++
			if s.cfg.Debug {
				log.Printf("[%s] candidate skipped (no cached headline mentions it)", it.Symbol)
			}
			continue
		}
		prio := gemini.PrioCandidate
		if it.Core {
			prio = gemini.PrioCore
		}
		if e := s.due(it.Symbol, it.Ind, false, prio, cs); e != nil {
			due = append(due, e)
		}
	}
	cs.due = len(due)
	s.runBatch(cs, due)
}

// geminiLine is the per-cycle Gemini report: the model new calls go to, the
// calls this cycle made per model, and an estimate of what is left. The
// estimate is an upper bound while the day's earlier usage is unknown.
func (s *Scheduler) geminiLine(cs *cycleState) string {
	if s.cfg.Gemini == nil {
		return "Gemini: not configured"
	}
	var used []string
	for _, u := range s.cfg.Gemini.UsageByModel() {
		if n := u.Used - cs.usageBefore[u.Model]; n > 0 {
			used = append(used, fmt.Sprintf("%s x%d", u.Model, n))
		}
	}
	active := s.cfg.Gemini.ActiveModel()
	if active == "" {
		active = "none usable"
	}
	rem, exact := s.cfg.Gemini.RemainingEstimate()
	remaining := fmt.Sprintf("%d", rem)
	if !exact {
		remaining = fmt.Sprintf("at most %d (usage before the last restart is unknown; the 429 response is the real limit)", rem)
	}
	return fmt.Sprintf("Gemini: active model %s | used this cycle: %s | remaining budget estimate: %s | counts today: %s",
		active, orNone(used), remaining, s.cfg.Gemini.UsageSummary())
}

// logCycleSummary prints the one-line cycle summary. buys/sells/holds are the
// model's answers; dropped are answers a guard refused, with reasons; skipped
// are symbols never sent to Gemini; missing are batch symbols the model left
// out (treated as HOLD).
func (s *Scheduler) logCycleSummary(cs *cycleState) {
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
	log.Printf("[Cycle] batch=%d %v deferred=%d | evaluated=%d holds=%d buys=%d sells=%d missing=%d dropped=%s not-evaluated=%d | trades today %d/%s, week %d/%s",
		len(cs.batchSyms), cs.batchSyms, cs.deferred,
		cs.evaluated, cs.counts.hold, cs.counts.buy, cs.counts.sell, cs.missing, dropped, skipped,
		cs.tradesDay, capLabel(s.cfg.MaxTradesPerDay), cs.tradesWeek, capLabel(s.cfg.MaxTradesPerWeek))
	log.Print("[Cycle] " + s.geminiLine(cs))
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
	if !market.EntryWindowOpenAsym(sess.Now, sess.Open, sess.Close, s.cfg.EntryBlackoutOpen, s.cfg.EntryBlackoutClose) {
		return fmt.Sprintf("within %v of the open or %v of the close", s.cfg.EntryBlackoutOpen, s.cfg.EntryBlackoutClose), false
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
