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
	defaultBatchMaxSymbols  = 12

	defaultBlackoutMin      = 10
	defaultMinPolicyStopPct = 3.0

	defaultRebalanceTranchePct = 5.0
	defaultRebalanceMaxWeek    = 2

	// defaultGuardsStartDate is the deploy date of the "atb-" order tagging:
	// decisions before it do not count toward the daily/weekly caps.
	defaultGuardsStartDate = "2026-10-14"
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
	geminiClient := gemini.NewClient(os.Getenv("GEMINI_API_KEY"), envString("GEMINI_MODEL", gemini.DefaultModel), envString("GEMINI_FALLBACK_MODEL", gemini.DefaultFallbackModel))
	geminiClient.SetLimits(envPositive("GEMINI_MAX_RPM", gemini.DefaultMaxRPM), envPositive("GEMINI_DAILY_BUDGET", gemini.DefaultDailyBudget))
	// GET /models does not use generate quota: drop configured models that do not exist.
	geminiClient.VerifyModels()
	port := portfolio.NewPortfolio(alpacaClient)

	tracker := stats.New(stats.AlpacaSource{Broker: alpacaClient, MarketData: dataClient}, envString("TRACK_START_DATE", stats.DefaultStartDate))
	// Browsers may read /stats/history from the owner's GitHub Pages site, an
	// optional custom domain (CORS_EXTRA_ORIGIN) and localhost; nobody else.
	tracker.SetOriginPolicy(stats.OriginPolicy(os.Getenv("CORS_EXTRA_ORIGIN")))

	maxCandidates := envInt("UNIVERSE_MAX_CANDIDATES", defaultMaxCandidates)
	uni := universe.New(coreWatchlist, maxCandidates, universe.BrokerAssets{Broker: alpacaClient}, dataClient, indicators.Calculate)
	uni.Prefetch = indicators.Prefetch
	uni.Limits = universe.Limits{
		MaxDayMovePct: envFloat("CANDIDATE_MAX_DAY_MOVE_PCT", 20),
		MaxRangePct:   envFloat("CANDIDATE_MAX_RANGE_PCT", 15),
		MaxATRPct:     envFloat("CANDIDATE_MAX_ATR_PCT", 7),
		MinMarketCap:  envFloat("CANDIDATE_MIN_MARKET_CAP_USD", 2e9),
		MinDollarVol:  envFloat("LIQUIDITY_MIN_DOLLAR_VOL", 20_000_000),
	}
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
	tracker.SetBands(stats.Bands{
		Equities: stats.Band{Min: policy.EquityMin, Max: policy.EquityMax},
		Bonds:    stats.Band{Min: policy.BondMin, Max: policy.BondMax},
		Cash:     stats.Band{Min: policy.CashMin, Max: policy.CashMax},
	})
	bonds := setupBonds(alpacaClient)

	bandsMode, modeOK := scheduler.NormalizeBandsMode(os.Getenv("IPS_BANDS_MODE"))
	if !modeOK {
		log.Printf("[Policy] invalid IPS_BANDS_MODE %q (want gradual, advisory or off), using %s", os.Getenv("IPS_BANDS_MODE"), bandsMode)
	}
	rebalanceTranche := envFloat("REBALANCE_TRANCHE_PCT", defaultRebalanceTranchePct)
	if rebalanceTranche <= 0 {
		rebalanceTranche = defaultRebalanceTranchePct
	}
	log.Printf("[Policy] IPS_BANDS_MODE=%s: a sleeve below its minimum band is %s; tranche %.1f%% of equity, at most %d rebalancing trades per week and one per day",
		bandsMode, bandsModeText(bandsMode), rebalanceTranche, envPositive("REBALANCE_MAX_TRADES_PER_WEEK", defaultRebalanceMaxWeek))

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
		BatchMaxSymbols:  envPositive("BATCH_MAX_SYMBOLS", defaultBatchMaxSymbols),

		MaxRiskPct:          envFloat("MAX_RISK_PCT", risk.DefaultMaxRiskPct),
		MinBuyConviction:    envPositive("MIN_BUY_CONVICTION", defaultMinBuyConviction),
		MinHoldDays:         envInt("MIN_HOLD_DAYS", defaultMinHoldDays),
		ReentryCooldownDays: envInt("REENTRY_COOLDOWN_DAYS", defaultReentryDays),
		MaxTradesPerDay:     envPositive("MAX_TRADES_PER_DAY", defaultMaxTradesDay),
		MaxTradesPerWeek:    envPositive("MAX_TRADES_PER_WEEK", defaultMaxTradesWeek),
		GuardsStartDate:     envDate("GUARDS_START_DATE", defaultGuardsStartDate),
		History:             trades.AlpacaSource{Broker: alpacaClient},
		Policy:              policy,
		Bonds:               bonds,
		Analyzer:            analysis.NewAnalyzer(dataClient),

		EntryBlackoutOpen:  time.Duration(envInt("ENTRY_BLACKOUT_OPEN_MIN", defaultBlackoutMin)) * time.Minute,
		EntryBlackoutClose: time.Duration(envInt("ENTRY_BLACKOUT_CLOSE_MIN", defaultBlackoutMin)) * time.Minute,
		MinStopATRMult:     envFloat("MIN_STOP_ATR_MULT", risk.StopATRMultiple),
		MinPolicyStopPct:   policyStopPct(envFloat("MIN_POLICY_STOP_PCT", defaultMinPolicyStopPct)),

		IPSBandsMode:              bandsMode,
		RebalanceTranchePct:       rebalanceTranche,
		RebalanceMaxTradesPerWeek: envPositive("REBALANCE_MAX_TRADES_PER_WEEK", defaultRebalanceMaxWeek),
		RebalanceBondETFs:         envList("REBALANCE_BOND_ETFS"),
	})

	// Health endpoint so the host (Render) and an uptime monitor can reach the bot.
	httpPort := os.Getenv("PORT")
	if httpPort == "" {
		httpPort = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.Handle("/stats", tracker.Handler())
	mux.Handle("/stats/history", tracker.HistoryHandler())
	srv := &http.Server{Addr: ":" + httpPort, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("HTTP server listening on :%s (/health, /stats, /stats/history)", httpPort)
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

// healthHandler answers the host's and the uptime pinger's checks. It touches
// nothing outside the process (no broker, no market data, no model), so it is
// fast and cannot fail because an upstream is down.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write([]byte("ok"))
	}
}

// policyStopPct maps MIN_POLICY_STOP_PCT to the scheduler's convention: 0 means
// "no stop on policy trades", which the scheduler reads as a negative value.
func policyStopPct(v float64) float64 {
	if v == 0 {
		return -1
	}
	return v
}

func bandsModeText(mode string) string {
	switch mode {
	case "advisory":
		return "only logged, no order is placed"
	case "off":
		return "left alone"
	}
	return "filled with core ETFs in tranches"
}

// envList reads a comma-separated upper-case symbol list; nil when unset.
func envList(name string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(name), ",") {
		if v = strings.ToUpper(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func envString(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envDate reads an ISO date (YYYY-MM-DD), keeping def when unset or invalid.
func envDate(name, def string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	if _, err := time.Parse("2006-01-02", v); err != nil {
		log.Printf("Invalid %s %q (want YYYY-MM-DD), using default %s", name, v, def)
		return def
	}
	return v
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
