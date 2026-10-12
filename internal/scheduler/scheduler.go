package scheduler

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/market"
	"github.com/francocaccia/quantengine/internal/marketdata"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/risk"
	"github.com/francocaccia/quantengine/internal/stats"
	"github.com/francocaccia/quantengine/internal/universe"
)

const (
	benchmarkSymbol = "SPY"
	maxSuggestions  = 3
)

type Config struct {
	Broker     *broker.AlpacaClient
	News       *news.AlpacaNewsClient
	Gemini     *gemini.Client
	Portfolio  *portfolio.Portfolio
	Universe   *universe.Universe
	Stats      *stats.Tracker
	MarketData *marketdata.Client

	MaxPositionPct   float64 // % of equity per position (hard cap)
	MaxPositionUSD   float64 // optional absolute cap; <= 0 disables
	MaxOpenPositions int
	Debug            bool // log Gemini's full reasoning
}

type Scheduler struct {
	cfg        Config
	ctx        context.Context
	cancel     context.CancelFunc
	newsCache  []news.Article
	sectors    map[string]string // symbol -> sector, learned from indicator fetches
	dayHaltFor string            // session date for which the daily-loss breaker has tripped
	paused     bool
}

func New(cfg Config) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{cfg: cfg, ctx: ctx, cancel: cancel, sectors: map[string]string{}}
}

func (s *Scheduler) Pause()         { s.paused = true }
func (s *Scheduler) Resume()        { s.paused = false }
func (s *Scheduler) IsPaused() bool { return s.paused }

func (s *Scheduler) Start() {
	log.Println("Scheduler started")
	s.fetchAndCacheNews()
	newsTicker := time.NewTicker(4 * time.Hour)
	marketTicker := time.NewTicker(5 * time.Minute)
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
		s.maybeRecordDay(sess)
		log.Println("[Market] Closed — skipping cycle")
		return
	}
	log.Println("[Market] Open — running decision cycle")

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
	}
	for i := range posList {
		p := posList[i]
		cs.positions[p.Symbol] = &p
		if sec := s.sectors[p.Symbol]; sec != "" {
			cs.sectorValue[sec] += p.MarketVal
		}
	}
	cs.buyBlock = s.checkBuyBlock(sess, acct)
	if cs.buyBlock != "" {
		log.Printf("[Risk] new BUYs blocked: %s (SELL/exits still active)", cs.buyBlock)
	}

	items := s.cfg.Universe.Build(s.newsSymbols())
	inUniverse := map[string]bool{}
	for _, it := range items {
		inUniverse[it.Symbol] = true
		s.noteSector(it.Symbol, it.Ind)
		s.evaluate(it.Symbol, it.Ind, false, cs)
	}

	// Held positions that fell out of the universe (or failed the filters,
	// e.g. dropped below $5) must still be managed, but never bought.
	for sym := range cs.positions {
		if inUniverse[sym] {
			continue
		}
		ind, err := indicators.Calculate(sym)
		if err != nil {
			log.Printf("[%s] Indicators error for held position: %v", sym, err)
			continue
		}
		s.noteSector(sym, ind)
		s.evaluate(sym, ind, true, cs)
	}
}

// checkBuyBlock applies the circuit breakers and the open/close buffer and
// returns the reason new BUYs are blocked ("" if allowed).
func (s *Scheduler) checkBuyBlock(sess *market.Session, acct *portfolio.Account) string {
	var peak float64
	if s.cfg.Stats != nil {
		peak = s.cfg.Stats.ObservePeak(acct.Equity)
	}
	dayStop, ddHalt, dayPct, ddPct := risk.Breakers(acct.Equity, acct.LastEquity, peak)
	if ddHalt {
		reason := fmt.Sprintf("DRAWDOWN HALT: equity $%.2f is %.1f%% below peak $%.2f (limit %.0f%%)", acct.Equity, ddPct, peak, risk.MaxDrawdownPct)
		log.Printf("[RISK] !!!!!!!! %s — BUYs halted until drawdown recovers !!!!!!!!", reason)
		return reason
	}
	if dayStop {
		s.dayHaltFor = sess.Date
	}
	if s.dayHaltFor == sess.Date {
		return fmt.Sprintf("daily loss breaker: equity %.2f%% vs day start (limit -%.0f%%), no new positions until next session", dayPct, risk.DailyLossLimitPct)
	}
	if !sess.TradingDay {
		return "trading calendar unavailable for today"
	}
	if !market.EntryWindowOpen(sess.Now, sess.Open, sess.Close, risk.EntryBuffer) {
		return fmt.Sprintf("within %v of the open or close", risk.EntryBuffer)
	}
	return ""
}

func (s *Scheduler) noteSector(symbol string, ind *indicators.Result) {
	if ind != nil && ind.Sector != "" {
		s.sectors[symbol] = ind.Sector
	}
}

// evaluate asks Gemini about one symbol and acts on the answer. sellOnly
// marks held symbols that are outside the validated universe.
func (s *Scheduler) evaluate(symbol string, ind *indicators.Result, sellOnly bool, cs *cycleState) {
	position := cs.positions[symbol]
	canBuy := !sellOnly && cs.buyBlock == "" && (position != nil || len(cs.positions) < s.cfg.MaxOpenPositions)
	if position == nil && !canBuy {
		// Nothing actionable (long-only, and BUY is not allowed): save the Gemini call.
		return
	}

	stop := 0.0
	if l, err := risk.Bracket(ind.Price, ind.ATR, 0); err == nil {
		stop = l.Stop
	}
	prompt := gemini.BuildPrompt(gemini.PromptInput{
		Symbol:     symbol,
		Indicators: ind,
		Position:   position,
		News:       s.newsFor(symbol),
		StopLoss:   stop,
	})
	decision, err := s.cfg.Gemini.Decide(prompt)
	if err != nil {
		log.Printf("[%s] Gemini error: %v", symbol, err)
		return
	}
	if decision.Conviction < 0 {
		decision.Conviction = 0
	} else if decision.Conviction > 10 {
		decision.Conviction = 10
	}
	log.Printf("[%s] %s conviction=%d | %s", symbol, decision.Action, decision.Conviction, decision.Thesis)
	if s.cfg.Debug {
		log.Printf("[%s] reasoning: %s", symbol, decision.Reasoning)
	}
	if n := len(decision.SuggestedTickers); n > 0 {
		if n > maxSuggestions {
			n = maxSuggestions
		}
		s.cfg.Universe.Suggest(decision.SuggestedTickers[:n])
	}

	switch decision.Action {
	case "BUY":
		switch {
		case sellOnly:
			log.Printf("[%s] BUY ignored: symbol is outside the validated universe", symbol)
		case cs.buyBlock != "":
			log.Printf("[%s] BUY blocked: %s", symbol, cs.buyBlock)
		default:
			s.placeBuy(symbol, ind, position, decision, cs)
		}
	case "SELL":
		if position == nil {
			log.Printf("[%s] SELL ignored: no long position to close (long-only)", symbol)
			return
		}
		s.closePosition(symbol, position, cs)
	case "HOLD":
	default:
		log.Printf("[%s] unknown action %q — ignored", symbol, decision.Action)
	}
}

func (s *Scheduler) placeBuy(symbol string, ind *indicators.Result, position *portfolio.Position, decision *gemini.Decision, cs *cycleState) {
	if decision.Conviction < risk.MinConviction {
		log.Printf("[%s] BUY skipped: conviction %d below minimum %d", symbol, decision.Conviction, risk.MinConviction)
		return
	}
	levels, err := risk.Bracket(ind.Price, ind.ATR, decision.TakeProfit)
	if err != nil {
		log.Printf("[%s] BUY rejected: %v", symbol, err)
		return
	}

	// Fresh account so buying power reflects orders placed earlier this cycle.
	acct, err := s.cfg.Portfolio.GetAccount()
	if err != nil {
		log.Printf("[%s] BUY skipped: account fetch failed: %v", symbol, err)
		return
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
		MaxPositionPct:       s.cfg.MaxPositionPct,
		MaxPositionUSD:       s.cfg.MaxPositionUSD,
	})
	if qty <= 0 {
		log.Printf("[%s] BUY skipped: %s", symbol, reason)
		return
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
			return
		}
		log.Printf("[%s] BUY quantity cut from %d to %d by %.0f%% %s sector limit", symbol, qty, capped, risk.MaxSectorPct, ind.Sector)
		qty = capped
	}

	if err := s.cfg.Broker.PlaceBracketOrder(symbol, qty, broker.Buy, levels.Stop, levels.Take); err != nil {
		log.Printf("[%s] BUY (bracket) failed: %v", symbol, err)
		return
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
}

// closePosition exits a whole long position. Open bracket legs reserve the
// shares, so they are cancelled first.
func (s *Scheduler) closePosition(symbol string, pos *portfolio.Position, cs *cycleState) {
	if n, err := s.cfg.Broker.CancelOpenOrders(symbol); err != nil {
		log.Printf("[%s] cancelling open orders failed (%d cancelled): %v", symbol, n, err)
	}
	if err := s.cfg.Broker.PlaceMarketOrder(symbol, pos.Qty, broker.Sell); err != nil {
		log.Printf("[%s] SELL failed: %v", symbol, err)
		return
	}
	log.Printf("[%s] SELL %d shares (closing long)", symbol, pos.Qty)
	if sec := s.sectors[symbol]; sec != "" {
		cs.sectorValue[sec] -= pos.MarketVal
	}
	delete(cs.positions, symbol)
}

// maybeRecordDay writes the end-of-day performance line once per session,
// after the close. It is safe to call on every closed-market tick.
func (s *Scheduler) maybeRecordDay(sess *market.Session) {
	if s.cfg.Stats == nil || s.cfg.MarketData == nil {
		return
	}
	if !sess.TradingDay || !sess.Now.After(sess.Close) || s.cfg.Stats.Recorded(sess.Date) {
		return
	}
	acct, err := s.cfg.Portfolio.GetAccount()
	if err != nil {
		log.Printf("[Stats] account fetch failed: %v — will retry", err)
		return
	}
	positions, err := s.cfg.Portfolio.GetAllPositions()
	if err != nil {
		log.Printf("[Stats] positions fetch failed: %v — will retry", err)
		return
	}
	spyClose, spyPrev, barDate, err := s.cfg.MarketData.DailyCloses(benchmarkSymbol)
	if err != nil {
		log.Printf("[Stats] %s snapshot failed: %v — will retry", benchmarkSymbol, err)
		return
	}
	if barDate != sess.Date {
		log.Printf("[Stats] %s daily bar is for %s, not %s yet — will retry", benchmarkSymbol, barDate, sess.Date)
		return
	}
	rec, err := s.cfg.Stats.RecordDay(sess.Date, acct.Equity, acct.LastEquity, spyClose, spyPrev, len(positions))
	if err != nil {
		log.Printf("[Stats] could not record %s: %v", sess.Date, err)
		return
	}
	log.Print(rec.FormatLine())
}
