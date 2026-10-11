package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/scheduler"
)

const defaultMaxPositionUSD = 5000.0

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file, using environment variables")
	}

	alpacaClient := broker.NewAlpacaClient(
		os.Getenv("ALPACA_API_KEY"),
		os.Getenv("ALPACA_API_SECRET"),
		os.Getenv("ALPACA_BASE_URL"),
	)

	newsClient := news.NewAlpacaNewsClient(
		os.Getenv("ALPACA_API_KEY"),
		os.Getenv("ALPACA_API_SECRET"),
	)

	geminiClient := gemini.NewClient(os.Getenv("GEMINI_API_KEY"))
	port := portfolio.NewPortfolio(alpacaClient)

	maxPositionUSD := defaultMaxPositionUSD
	if v := os.Getenv("MAX_POSITION_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			maxPositionUSD = f
		} else {
			log.Printf("Invalid MAX_POSITION_USD %q, using default %.2f", v, defaultMaxPositionUSD)
		}
	}

	sched := scheduler.New(scheduler.Config{
		Broker:         alpacaClient,
		News:           newsClient,
		Gemini:         geminiClient,
		Portfolio:      port,
		Watchlist:      []string{"SPY", "QQQ", "AAPL", "MSFT", "NVDA", "AMZN", "GOOG"},
		MaxPositionUSD: maxPositionUSD,
	})

	// Health endpoint so the host (Render) and an uptime monitor can reach the bot.
	httpPort := os.Getenv("PORT")
	if httpPort == "" {
		httpPort = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: ":" + httpPort, Handler: mux}
	go func() {
		log.Printf("Health endpoint listening on :%s", httpPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	log.Println("QuantEngine starting...")
	go sched.Start()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down...")
	sched.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
