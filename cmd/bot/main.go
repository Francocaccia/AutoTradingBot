package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // market hours use America/New_York; slim images may lack zoneinfo

	"github.com/joho/godotenv"

	"github.com/francocaccia/quantengine/internal/analysis"
	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/marketdata"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/risk"
	"github.com/francocaccia/quantengine/internal/scheduler"
	"github.com/francocaccia/quantengine/internal/stats"
	"github.com/francocaccia/quantengine/internal/trades"
	"github.com/francocaccia/quantengine/internal/universe"
)

const (
	defaultMaxPositionPct   = 10.0
	defaultMaxOpenPositions = 8
	defaultMaxCandidates    = 3
	defaultCycleMin         = 15
	defaultMinBuyConviction = 8
	defaultMinHoldDays      = 3
	defaultReentryDays      = 3
	defaultMaxTradesDay     = 2
	defaultMaxTradesWeek    = 5
	defaultEvalCooldownMin  = 90
)

var coreWatchlist = []string{"SPY", "QQQ", "AAPL", "MSFT", "NVDA", "AMZN", "GOOG"}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file, using environment variables")
	}

	apiKey, apiSecret := os.Getenv("ALPACA_API_KEY"), os.Getenv("ALPACA_API_SECRET")
	alpacaClient := broker.NewAlpacaClient(apiKey, apiSecret, os.Getenv("ALPACA_BASE_URL"))
	newsClient := news.NewAlpacaNewsClient(apiKey, apiSecret)
	dataClient := marketdata.NewClient(apiKey, apiSecret)
	geminiClient := gemini.NewClient(os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_FALLBACK_MODEL"))
	geminiClient.SetLimits(envPositive("GEMINI_MAX_RPM", gemini.DefaultMaxRPM), envPositive("GEMINI_DAILY_BUDGET", gemini.DefaultDailyBudget))
	port := portfolio.NewPortfolio(alpacaClient)

	tracker := stats.New(stats.AlpacaSource{Broker: alpacaClient, MarketData: dataClient}, envString("TRACK_START_DATE", stats.DefaultStartDate))

	maxCandidates := envInt("UNIVERSE_MAX_CANDIDATES", defaultMaxCandidates)
	uni := universe.New(coreWatchlist, maxCandidates, universe.BrokerAssets{Broker: alpacaClient}, dataClient, indicators.Calculate)
	uni.Prefetch = indicators.Prefetch
	// Curated bond ETFs skip the single-stock price/volume filters.
	uni.Exempt = func(sym string) bool { _, ok := portfolio.BondInfoFor(sym); return ok }

	maxPositionPct := envFloat("MAX_POSITION_PCT", defaultMaxPositionPct)
	policy, warnings := portfolio.PolicyFromEnv(os.Getenv, maxPositionPct)
	for _, w := range warnings {
		log.Printf("[Policy] %s", w)
	}
	log.Printf("[Policy] IPS bands: equities %.0f-%.0f%%, bonds %.0f-%.0f%%, cash/T-bills %.0f-%.0f%%; targets %.0f/%.0f/%.0f; single stock <= %.0f%%, bond ETF <= %.0f%%, sector <= %.0f%%, credit <= %.0f%%, duration <= %.0fy",
		policy.EquityMin, policy.EquityMax, policy.BondMin, policy.BondMax, policy.CashMin, policy.CashMax,
		policy.TargetEquity, policy.TargetBond, policy.TargetCash,
		policy.MaxStockPct, policy.MaxBondETFPct, policy.SectorMaxPct, policy.CreditMaxPct, policy.MaxDuration)
	bonds := setupBonds(alpacaClient)

	sched := scheduler.New(scheduler.Config{
		Broker:           alpacaClient,
		News:             newsClient,
		Gemini:           geminiClient,
		Portfolio:        port,
		Universe:         uni,
		Stats:            tracker,
		MaxPositionPct:   maxPositionPct,
		MaxPositionUSD:   envFloat("MAX_POSITION_USD", 0),
		MaxOpenPositions: envInt("MAX_OPEN_POSITIONS", defaultMaxOpenPositions),
		Debug:            strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug"),
		CycleInterval:    time.Duration(envPositive("CYCLE_INTERVAL_MIN", defaultCycleMin)) * time.Minute,
		EvalCooldown:     time.Duration(envInt("EVAL_COOLDOWN_MIN", defaultEvalCooldownMin)) * time.Minute,

		MaxRiskPct:          envFloat("MAX_RISK_PCT", risk.DefaultMaxRiskPct),
		MinBuyConviction:    envPositive("MIN_BUY_CONVICTION", defaultMinBuyConviction),
		MinHoldDays:         envInt("MIN_HOLD_DAYS", defaultMinHoldDays),
		ReentryCooldownDays: envInt("REENTRY_COOLDOWN_DAYS", defaultReentryDays),
		MaxTradesPerDay:     envPositive("MAX_TRADES_PER_DAY", defaultMaxTradesDay),
		MaxTradesPerWeek:    envPositive("MAX_TRADES_PER_WEEK", defaultMaxTradesWeek),
		History:             trades.AlpacaSource{Broker: alpacaClient},
		Policy:              policy,
		Bonds:               bonds,
		Analyzer:            analysis.NewAnalyzer(dataClient),
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
	mux.Handle("/stats", tracker.Handler())
	srv := &http.Server{Addr: ":" + httpPort, Handler: mux}
	go func() {
		log.Printf("HTTP server listening on :%s (/health, /stats)", httpPort)
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

func envString(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envFloat reads a non-negative float env var, keeping def when unset or invalid.
func envFloat(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		log.Printf("Invalid %s %q, using default %v", name, v, def)
		return def
	}
	return f
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Printf("Invalid %s %q, using default %d", name, v, def)
		return def
	}
	return n
}

// envPositive is envInt that also rejects 0.
func envPositive(name string, def int) int {
	n := envInt(name, def)
	if n <= 0 {
		return def
	}
	return n
}

// setupBonds probes what the account can trade, resolves BOND_MODE and returns
// the bond ETFs the bot may buy. Nothing is assumed: native fixed income is
// used only if the account reports it (and even then the bot has no native
// order flow, so it trades the ETFs and says so).
func setupBonds(b *broker.AlpacaClient) *portfolio.BondUniverse {
	caps := b.ProbeCapabilities()
	log.Printf("[Capabilities] %s", caps)
	requested := strings.ToLower(envString("BOND_MODE", broker.BondModeAuto))
	resolved, effective, note := broker.ResolveBondMode(requested, caps)
	log.Printf("[Capabilities] BOND_MODE=%s resolved=%s effective=%s: %s", requested, resolved, effective, note)
	if effective == broker.BondModeOff {
		return portfolio.DisabledBondUniverse()
	}

	var wanted []string
	for _, sym := range strings.Split(os.Getenv("BOND_ETFS"), ",") {
		if sym = strings.ToUpper(strings.TrimSpace(sym)); sym != "" {
			wanted = append(wanted, sym)
		}
	}
	candidates := portfolio.NewBondUniverse(wanted).Symbols()
	assets := universe.BrokerAssets{Broker: b}
	var tradable []string
	var missing []string
	for _, sym := range candidates {
		a, err := assets.GetAsset(sym)
		switch {
		case err != nil && !errors.Is(err, broker.ErrNotFound):
			tradable = append(tradable, sym) // lookup failed: cannot tell, keep it
		case err != nil || universe.CheckAsset(a) != nil:
			missing = append(missing, sym)
		default:
			tradable = append(tradable, sym)
		}
	}
	log.Printf("[Capabilities] bond ETFs tradable: %d/%d %v; not tradable: %v", len(tradable), len(candidates), tradable, missing)
	return portfolio.NewBondUniverse(tradable)
}
