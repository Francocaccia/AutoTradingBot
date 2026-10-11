package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/claude"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/market"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

type Config struct {
	Broker    *broker.AlpacaClient
	News      *news.AlpacaNewsClient
	Claude    *claude.Client
	Portfolio *portfolio.Portfolio
	Watchlist []string
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
	isOpen, err := market.IsMarketOpen()
	if err != nil || !isOpen {
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
	position, _ := s.cfg.Portfolio.GetPosition(symbol)
	prompt := claude.BuildPrompt(claude.PromptInput{
		Symbol:     symbol,
		Indicators: ind,
		Position:   position,
		News:       s.newsCache,
	})
	decision, err := s.cfg.Claude.Decide(prompt)
	if err != nil {
		log.Printf("[%s] Claude error: %v", symbol, err)
		return
	}
	log.Printf("[%s] -> %s | %s", symbol, decision.Action, decision.Reasoning)
	switch decision.Action {
	case "BUY":
		if err := s.cfg.Broker.PlaceMarketOrder(symbol, decision.Quantity, broker.Buy); err != nil {
			log.Printf("[%s] BUY failed: %v", symbol, err)
		} else {
			log.Printf("[%s] BUY %d shares", symbol, decision.Quantity)
		}
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
