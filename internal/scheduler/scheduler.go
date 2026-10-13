package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/market"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
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
)

// orderPlacer is the part of the Alpaca client that places and cancels orders.
type orderPlacer interface {
	PlaceBracketOrder(symbol string, qty int, side broker.Side, stopLoss, takeProfit float64) error
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
	Stats     *stats.Service // in-memory equity peak for the drawdown halt

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
}

func New(cfg Config) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.CycleInterval <= 0 {
		cfg.CycleInterval = defaultCycleInterval
	}
	s := &Scheduler{cfg: cfg, ctx: ctx, cancel: cancel, sectors: map[string]string{}, gate: newEvalGate(cfg.EvalCooldown),
		lastDecisions: map[string]gemini.LastDecision{}}
	if cfg.Broker != nil {
		s.orders = cfg.Broker
	}
	if cfg.Portfolio != nil {
		s.acct = cfg.Portfolio
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

// cycleState is the account/position snapshot shared by one decision cycle.
type cycleState struct {
	account     *portfolio.Account
	positions   map[string]*portfolio.Position
	sectorValue map[string]float64
	buyBlock    string // non-empty: no new BUYs this cycle, with the reason
	warnedNoSec map[string]bool

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

	pending []pendingDecision // collected during evaluation, executed afterwards
	counts  decisionCounts
	blocked int // decisions a guard refused to execute
}

// decisionCounts tallies what the model answered this cycle.
type decisionCounts struct{ buy, sell, hold int }

// pendingDecision is one model answer waiting for the end-of-cycle execution
// pass, which orders trades by priority when the turnover caps bind.
type pendingDecision struct {
	symbol    string
	ind       *indicators.Result
	position  *portfolio.Position
	decision  *gemini.Decision
	sellOnly  bool
	headlines []string // the headlines shown to the model, for catalyst verification
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
		sectorValue: map[string]float64{},
		warnedNoSec: map[string]bool{},
		now:         sess.Now,
	}
	for i := range posList {
		p := posList[i]
		cs.positions[p.Symbol] = &p
		if sec := s.sectors[p.Symbol]; sec != "" {
			cs.sectorValue[sec] += p.MarketVal
		}
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

	for _, sym := range held {
		if cs.stopGemini {
			break
		}
		if it := inUniverse[sym]; it != nil {
			s.evaluate(sym, it.Ind, false, cs)
			continue
		}
		// Outside the universe or failing the BUY filters (e.g. a low-volume
		// ETF): still managed, SELL-only. The filters gate new positions only.
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

	// BUY/SELL/HOLD are the model's answers this cycle; skipped counts symbols
	// not sent to Gemini plus answers a guard refused to execute. Orders
	// actually placed show in the trades counters.
	skipped := cs.skipCooldown + cs.skipNoSetup + cs.skipNoNews + cs.blocked
	log.Printf("[Cycle] evaluated=%d BUY=%d SELL=%d HOLD=%d skipped=%d | trades today %d/%s, week %d/%s",
		cs.evaluated, cs.counts.buy, cs.counts.sell, cs.counts.hold, skipped,
		cs.tradesDay, capLabel(s.cfg.MaxTradesPerDay), cs.tradesWeek, capLabel(s.cfg.MaxTradesPerWeek))
	used, budget := s.cfg.Gemini.Usage()
	log.Printf("[Gemini] calls today: %d/%d", used, budget)
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

	stop := 0.0
	if l, err := risk.Bracket(ind.Price, ind.ATR, 0); err == nil {
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
	prompt := gemini.BuildPrompt(gemini.PromptInput{
		Symbol:     symbol,
		Indicators: ind,
		Position:   position,
		News:       articles,
		StopLoss:   stop,
		DaysHeld:   daysHeld,
		Last:       last,
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
	log.Printf("[%s] %s conviction=%d | %s", symbol, decision.Action, decision.Conviction, decision.Thesis)
	if s.cfg.Debug {
		log.Printf("[%s] reasoning: %s | catalyst: %s", symbol, decision.Reasoning, decision.Catalyst)
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
		symbol: symbol, ind: ind, position: position, decision: decision, sellOnly: sellOnly, headlines: shown,
	})
}

// catalystMatches reports whether the model's catalyst quotes one of the
// headlines it was shown (case-insensitive substring, either direction so a
// "[Source] headline" quote also counts). "none" and empty never match.
func catalystMatches(catalyst string, headlines []string) bool {
	c := strings.ToLower(strings.Trim(strings.TrimSpace(catalyst), "\"'“”‘’ "))
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

// capBound reports whether a turnover cap blocks one more order.
func (s *Scheduler) capBound(cs *cycleState) bool {
	return (s.cfg.MaxTradesPerDay > 0 && cs.tradesDay >= s.cfg.MaxTradesPerDay) ||
		(s.cfg.MaxTradesPerWeek > 0 && cs.tradesWeek >= s.cfg.MaxTradesPerWeek)
}

func (s *Scheduler) logCapSkip(symbol, action string, cs *cycleState) {
	log.Printf("[%s] %s skipped: trade cap (today %d/%s, week %d/%s)", symbol, action,
		cs.tradesDay, capLabel(s.cfg.MaxTradesPerDay), cs.tradesWeek, capLabel(s.cfg.MaxTradesPerWeek))
	cs.blocked++
}

func (s *Scheduler) minBuyConviction() int {
	if s.cfg.MinBuyConviction > 0 {
		return s.cfg.MinBuyConviction
	}
	return risk.DefaultMinConviction
}

// buyEligibility returns why a BUY may not be executed ("" if it may). These
// checks do not depend on how many orders were placed this cycle, so a BUY that
// fails them never uses up a turnover slot.
func (s *Scheduler) buyEligibility(p *pendingDecision, cs *cycleState) string {
	switch {
	case p.sellOnly:
		return "symbol is outside the validated universe"
	case cs.buyBlock != "":
		return cs.buyBlock
	case p.decision.Conviction < s.minBuyConviction():
		return fmt.Sprintf("conviction %d below minimum %d", p.decision.Conviction, s.minBuyConviction())
	case !catalystMatches(p.decision.Catalyst, p.headlines):
		return fmt.Sprintf("no verified catalyst (%q is not one of the %d headlines shown)", p.decision.Catalyst, len(p.headlines))
	case cs.history == nil:
		return "order history unavailable, cannot check cooldown and turnover limits"
	}
	if s.cfg.ReentryCooldownDays > 0 {
		if days, ok := cs.history.SinceLastSell(p.symbol); ok && days < s.cfg.ReentryCooldownDays {
			return fmt.Sprintf("re-entry cooldown: sold %d trading day(s) ago, minimum %d", days, s.cfg.ReentryCooldownDays)
		}
	}
	return ""
}

// executeDecisions acts on the cycle's collected answers. Risk-reducing SELLs
// run first, then eligible BUYs by conviction (highest first), so when a
// turnover cap binds the most valuable orders are the ones that go through.
func (s *Scheduler) executeDecisions(cs *cycleState) {
	var sells, buys []pendingDecision
	for _, p := range cs.pending {
		switch p.decision.Action {
		case "BUY":
			cs.counts.buy++
			if why := s.buyEligibility(&p, cs); why != "" {
				log.Printf("[%s] BUY skipped: %s", p.symbol, why)
				cs.blocked++
				continue
			}
			buys = append(buys, p)
		case "SELL":
			cs.counts.sell++
			if p.position == nil {
				log.Printf("[%s] SELL ignored: no long position to close (long-only)", p.symbol)
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
	for _, p := range buys {
		s.execBuy(p, cs)
	}
}

func (s *Scheduler) execSell(p pendingDecision, cs *cycleState) {
	// Under a circuit breaker a SELL is a risk exit: the holding period and the
	// turnover caps do not apply, and it is not counted against them.
	if !cs.breaker {
		if cs.history != nil && s.cfg.MinHoldDays > 0 {
			if age, ok := cs.history.PositionAge(p.symbol); ok && age < s.cfg.MinHoldDays {
				log.Printf("[%s] SELL skipped: position is %d trading day(s) old, minimum hold %d", p.symbol, age, s.cfg.MinHoldDays)
				cs.blocked++
				return
			}
		}
		if cs.history != nil && s.capBound(cs) {
			s.logCapSkip(p.symbol, "SELL", cs)
			return
		}
	}
	if s.closePosition(p.symbol, p.position, cs) && !cs.breaker {
		cs.tradesDay++
		cs.tradesWeek++
	}
}

func (s *Scheduler) execBuy(p pendingDecision, cs *cycleState) {
	if s.capBound(cs) {
		s.logCapSkip(p.symbol, "BUY", cs)
		return
	}
	if p.position == nil && len(cs.positions) >= s.cfg.MaxOpenPositions {
		log.Printf("[%s] BUY skipped: %d open positions (max %d)", p.symbol, len(cs.positions), s.cfg.MaxOpenPositions)
		cs.blocked++
		return
	}
	if s.placeBuy(p.symbol, p.ind, cs.positions[p.symbol], p.decision, cs) {
		cs.tradesDay++
		cs.tradesWeek++
	}
}

// placeBuy sizes and submits a bracket BUY. It reports whether an order was placed.
func (s *Scheduler) placeBuy(symbol string, ind *indicators.Result, position *portfolio.Position, decision *gemini.Decision, cs *cycleState) bool {
	levels, err := risk.Bracket(ind.Price, ind.ATR, decision.TakeProfit)
	if err != nil {
		log.Printf("[%s] BUY rejected: %v", symbol, err)
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
		log.Printf("[%s] BUY skipped: %s", symbol, reason)
		return false
	}

	if room, ok := risk.SectorRoom(ind.Sector, cs.sectorValue, acct.Equity); !ok {
		if !cs.warnedNoSec[symbol] {
			cs.warnedNoSec[symbol] = true
			log.Printf("[%s] sector data unavailable — %.0f%% sector limit not applied", symbol, risk.MaxSectorPct)
		}
	} else if cost := float64(qty) * ind.Price; cost > room {
		capped := int(math.Floor(room / ind.Price))
		if capped < 1 {
			log.Printf("[%s] BUY skipped: %s sector at %.0f%% limit", symbol, ind.Sector, risk.MaxSectorPct)
			return false
		}
		log.Printf("[%s] BUY quantity cut from %d to %d by %.0f%% %s sector limit", symbol, qty, capped, risk.MaxSectorPct, ind.Sector)
		qty = capped
	}

	// Per-trade risk: the loss if the stop is hit may not exceed MAX_RISK_PCT of equity.
	if capped, stopPct := risk.CapQtyByRisk(qty, ind.Price, levels.Stop, acct.Equity, s.cfg.MaxRiskPct); capped < qty {
		if capped < 1 {
			log.Printf("[%s] BUY skipped: max-risk %.2f%% of equity allows less than 1 share (stop %.1f%% away)", symbol, s.cfg.MaxRiskPct, stopPct)
			return false
		}
		log.Printf("[%s] BUY qty capped from %d to %d by max-risk (stop %.1f%% away)", symbol, qty, capped, stopPct)
		qty = capped
	}

	if err := s.orders.PlaceBracketOrder(symbol, qty, broker.Buy, levels.Stop, levels.Take); err != nil {
		log.Printf("[%s] BUY (bracket) failed: %v", symbol, err)
		return false
	}
	log.Printf("[%s] BUY %d shares @ ~$%.2f (bracket stop=%.2f take=%.2f R:R=%.1f)", symbol, qty, ind.Price, levels.Stop, levels.Take, levels.RewardRisk)

	cost := float64(qty) * ind.Price
	if position == nil {
		cs.positions[symbol] = &portfolio.Position{Symbol: symbol, Qty: qty, AvgPrice: ind.Price, MarketVal: cost}
	} else {
		position.MarketVal += cost
		position.Qty += qty
	}
	if ind.Sector != "" {
		cs.sectorValue[ind.Sector] += cost
	}
	return true
}

// closePosition exits a whole long position. Open bracket legs reserve the
// shares, so they are cancelled first. It reports whether the order was placed.
func (s *Scheduler) closePosition(symbol string, pos *portfolio.Position, cs *cycleState) bool {
	if n, err := s.orders.CancelOpenOrders(symbol); err != nil {
		log.Printf("[%s] cancelling open orders failed (%d cancelled): %v", symbol, n, err)
	}
	if err := s.orders.PlaceMarketOrder(symbol, pos.Qty, broker.Sell); err != nil {
		log.Printf("[%s] SELL failed: %v", symbol, err)
		return false
	}
	log.Printf("[%s] SELL %d shares (closing long)", symbol, pos.Qty)
	if sec := s.sectors[symbol]; sec != "" {
		cs.sectorValue[sec] -= pos.MarketVal
	}
	delete(cs.positions, symbol)
	return true
}
