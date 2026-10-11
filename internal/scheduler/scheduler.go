package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/market"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

type Config struct {
	Broker         *broker.AlpacaClient
	News           *news.AlpacaNewsClient
	Gemini         *gemini.Client
	Portfolio      *portfolio.Portfolio
	Watchlist      []string
	MaxPositionUSD float64
}

type Scheduler struct {
	cfg       Config
	ctx       context.Context
	cancel    context.CancelFunc
	newsCache []news.Article
	paused    bool
}

func New(cfg Config) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{cfg: cfg, ctx: ctx, cancel: cancel}
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

func (s *Scheduler) fetchAndCacheNews() {
	log.Println("[News] Fetching latest news...")
	articles, err := s.cfg.News.FetchLatest(s.cfg.Watchlist, 20)
	if err != nil {
		log.Printf("[News] Error: %v", err)
		return
	}
	s.newsCache = articles
	log.Printf("[News] Cached %d articles", len(articles))
}

func (s *Scheduler) runMarketCycle() {
	if s.paused {
		log.Println("[Scheduler] Paused — skipping cycle")
		return
	}
	isOpen, err := market.IsMarketOpen(s.cfg.Broker)
	if err != nil {
		log.Printf("[Market] Clock check failed: %v — skipping cycle", err)
		return
	}
	if !isOpen {
		log.Println("[Market] Closed — skipping cycle")
		return
	}
	log.Println("[Market] Open — running decision cycle")
	for _, symbol := range s.cfg.Watchlist {
		s.evaluateSymbol(symbol)
	}
}

func (s *Scheduler) evaluateSymbol(symbol string) {
	ind, err := indicators.Calculate(symbol)
	if err != nil {
		log.Printf("[%s] Indicators error: %v", symbol, err)
		return
	}
	position, err := s.cfg.Portfolio.GetPosition(symbol)
	if err != nil {
		log.Printf("[%s] Position lookup error: %v — skipping symbol", symbol, err)
		return
	}
	prompt := gemini.BuildPrompt(gemini.PromptInput{
		Symbol:     symbol,
		Indicators: ind,
		Position:   position,
		News:       s.newsCache,
	})
	decision, err := s.cfg.Gemini.Decide(prompt)
	if err != nil {
		log.Printf("[%s] Gemini error: %v", symbol, err)
		return
	}
	log.Printf("[%s] -> %s | %s", symbol, decision.Action, decision.Reasoning)
	switch decision.Action {
	case "BUY":
		s.placeBuy(symbol, ind, position, decision)
	case "SELL":
		if err := s.cfg.Broker.PlaceMarketOrder(symbol, decision.Quantity, broker.Sell); err != nil {
			log.Printf("[%s] SELL failed: %v", symbol, err)
		} else {
			log.Printf("[%s] SELL %d shares", symbol, decision.Quantity)
		}
	case "HOLD":
		log.Printf("[%s] HOLD", symbol)
	}
}

func (s *Scheduler) placeBuy(symbol string, ind *indicators.Result, position *portfolio.Position, decision *gemini.Decision) {
	qty := decision.Quantity
	if qty <= 0 || ind.Price <= 0 {
		log.Printf("[%s] BUY skipped: invalid quantity (%d) or price (%.2f)", symbol, qty, ind.Price)
		return
	}

	currentValue := 0.0
	if position != nil {
		currentValue = position.MarketVal
	}
	maxPositionUSD := s.cfg.MaxPositionUSD
	newValue := currentValue + float64(qty)*ind.Price
	if newValue > maxPositionUSD {
		allowedValue := maxPositionUSD - currentValue
		cappedQty := int(allowedValue / ind.Price)
		log.Printf("[%s] BUY quantity capped from %d to %d: current position $%.2f + order would exceed MAX_POSITION_USD $%.2f",
			symbol, qty, cappedQty, currentValue, maxPositionUSD)
		qty = cappedQty
	}
	if qty <= 0 {
		log.Printf("[%s] BUY skipped: position already at or above MAX_POSITION_USD limit ($%.2f >= $%.2f)", symbol, currentValue, maxPositionUSD)
		return
	}

	useBracket := decision.StopLoss > 0 && decision.TakeProfit > 0 &&
		decision.StopLoss < ind.Price && ind.Price < decision.TakeProfit
	if useBracket {
		if err := s.cfg.Broker.PlaceBracketOrder(symbol, qty, broker.Buy, decision.StopLoss, decision.TakeProfit); err != nil {
			log.Printf("[%s] BUY (bracket) failed: %v", symbol, err)
		} else {
			log.Printf("[%s] BUY %d shares (bracket, stop=%.2f take=%.2f)", symbol, qty, decision.StopLoss, decision.TakeProfit)
		}
		return
	}

	log.Printf("[%s] stop_loss/take_profit invalid (stop=%.2f price=%.2f take=%.2f) — using plain market order",
		symbol, decision.StopLoss, ind.Price, decision.TakeProfit)
	if err := s.cfg.Broker.PlaceMarketOrder(symbol, qty, broker.Buy); err != nil {
		log.Printf("[%s] BUY failed: %v", symbol, err)
	} else {
		log.Printf("[%s] BUY %d shares", symbol, qty)
	}
}
