# AutoTradingBot

An autonomous paper-trading bot written in Go. Every 15 minutes (configurable) during US market hours it combines technical indicators, recent news and an LLM (Gemini 2.5 Flash) to decide whether to **BUY**, **SELL** or **HOLD** each symbol in a core watchlist plus a few screened candidates, and executes the decision through Alpaca's paper-trading API. It is **long-only**: no shorting, margin, leverage or options.

> **Paper trading only.** This is a personal/educational/portfolio project, not financial advice. It has not been backtested and should not be pointed at a live account.

## How it works

```
every CYCLE_INTERVAL_MIN (default 15) min (Mon–Fri, market hours per Alpaca's clock/calendar)
  [no cycle at process start: the first one runs at the next tick]
  ├─ account + positions → circuit breakers, open/close buffer
  ├─ universe = core watchlist + up to N validated candidates
  │     (Alpaca screener, tickers in cached news, Gemini suggestions)
  ├─ held positions first (all of them, core or not), then universe symbols
  └─ for each symbol
       ├─ Python (one batched call per cycle) → RSI, Ichimoku, MACD, Stochastic, Keltner, ATR, volume
       ├─ gate: per-symbol cooldown + exceptions, prefilter for quiet non-held symbols
       ├─ position, news for the symbol
       ├─ prompt → Gemini → {action, conviction 1-10, thesis, reasoning, suggested_tickers}
       └─ BUY  → size by conviction, ATR bracket (stop + take-profit), R:R >= 2
          SELL → cancel bracket legs, close the long
```

The model is told the bot is long-only, must ground its answer in the supplied indicators and news, and should BUY when conviction is 7+ with a catalyst or technical setup. Position size, stop-loss and every limit below are enforced in code, not by the prompt.

Each decision logs one line, `[SYM] ACTION conviction=N | thesis`; set `LOG_LEVEL=debug` to also log Gemini's full reasoning.

<!-- Paste a short excerpt of real logs from Render here, inside a code block -->

## Stack

- **Go 1.22**: scheduler, Alpaca and Gemini clients, order execution
- **Python 3.11** (`yfinance`, `pandas`): indicator calculation, called from Go as a subprocess. Daily bars come from Alpaca market data (IEX feed, free) with a batched `yfinance` fallback; `yfinance` supplies fundamentals (beta, P/E, sector, average volume)
- **Alpaca Markets** paper API: orders, positions, news
- **Google Gemini 2.5 Flash**: decision engine
- **Docker** and **Render**: deployment

## Project structure

```
cmd/bot/                 entrypoint, health endpoint, graceful shutdown
internal/scheduler/      market cycle, evaluation gate (cooldown/prefilter), order flow, daily stats trigger
internal/broker/         Alpaca REST client (orders, positions, assets, calendar)
internal/news/           Alpaca News client
internal/marketdata/     Alpaca screener and SPY daily-bar snapshot
internal/gemini/         prompt builder and Gemini client (retry + fallback model)
internal/universe/       dynamic universe and hard symbol filters
internal/risk/           sizing, bracket levels, circuit breakers, sector limit
internal/stats/          daily performance CSV, peak equity, GET /stats
internal/portfolio/      account and position lookup
internal/market/         market session (clock + calendar) and entry window
internal/indicators/     Go wrapper around the Python script
python/indicators/       indicator calculations (pure pandas)
render.yaml              Render blueprint
```

## Run locally

Requirements: Go 1.22+, Python 3.11+ with `pip install yfinance pandas`, an [Alpaca paper account](https://alpaca.markets) and a [Gemini API key](https://aistudio.google.com).

```bash
cp .env.example .env     # then fill in your keys
go run ./cmd/bot
```

| Variable | Description |
|---|---|
| `ALPACA_API_KEY`, `ALPACA_API_SECRET` | Alpaca paper-trading credentials |
| `ALPACA_BASE_URL` | Defaults to `https://paper-api.alpaca.markets` (no `/v2` suffix) |
| `GEMINI_API_KEY` | Google AI Studio key |
| `GEMINI_FALLBACK_MODEL` | Model tried once (e.g. `gemini-2.5-flash-lite`) after the primary exhausts its retries; empty disables it (default empty) |
| `MAX_POSITION_PCT` | Hard cap per position as % of account equity (default `10`) |
| `MAX_POSITION_USD` | Optional absolute cap per position in USD; unset or `0` disables it |
| `MAX_OPEN_POSITIONS` | Max simultaneous open positions (default `8`) |
| `UNIVERSE_MAX_CANDIDATES` | Extra screened candidates per cycle on top of the core watchlist (default `5`; `0` disables the dynamic universe) |
| `GEMINI_MAX_RPM` | Global cap on Gemini requests per minute, retries included; calls wait for room (default `8`) |
| `GEMINI_DAILY_BUDGET` | Max Gemini requests per day (America/Los_Angeles day), retries included; decisions pause when spent (default `200`) |
| `EVAL_COOLDOWN_MIN` | A symbol is sent to Gemini at most once per this many minutes, unless price moved >2%, RSI crossed 30/70, or new news mentions it; `0` disables (default `45`) |
| `CYCLE_INTERVAL_MIN` | Minutes between market cycles (default `15`) |
| `LOG_LEVEL` | `debug` also logs Gemini's full reasoning; anything else logs one line per decision (default `info`) |
| `PORT` | Port for the `/health` and `/stats` endpoints (default `8080`) |

## Dynamic universe

The core watchlist (SPY, QQQ, AAPL, MSFT, NVDA, AMZN, GOOG) is always evaluated. Each cycle up to `UNIVERSE_MAX_CANDIDATES` more symbols are added from Alpaca's most-actives and movers screeners, tickers mentioned in the cached Alpaca news, and `suggested_tickers` returned by Gemini (these join the **next** cycle's pool). Every symbol, core included, must pass these filters in code before Gemini is called or any order is placed:

- exists on Alpaca (`GET /v2/assets/{symbol}`), status `active`, tradable, class `us_equity`
- exchange is NYSE, NASDAQ or ARCA (no OTC)
- price >= $5 and average daily volume >= 1M shares (from the yfinance data)
- not a leveraged or inverse ETF by name
- market data fetch succeeded (otherwise the symbol is skipped and logged)

Filters also reject warrants, rights and units (Alpaca asset name containing "Warrant", "Right" or "Unit", and 5-letter tickers ending in W, WS, R or U) and skip a candidate whose other share class is already represented (e.g. GOOGL when GOOG is in the core list).

The filters apply **only to opening new positions**. The bot manages every position in the account: each held symbol is added to the evaluation set automatically, evaluated for SELL on the cooldown schedule even if it is not in the core list or candidate pool, and can be sold even if it fails the BUY filters (e.g. a held low-volume ETF).

## Gemini quota and call volume

- Every request, retries included, passes a global rate limiter (`GEMINI_MAX_RPM`) and a daily budget (`GEMINI_DAILY_BUDGET`); the running count is logged once per cycle.
- HTTP 429 with "quota" in the body (and no per-minute marker) is a daily limit: it is **not retried**. The bot logs `[Gemini] daily quota exhausted, pausing until reset` once and makes no Gemini calls until 00:00 America/Los_Angeles. Other 429s (per-minute) are retried, honoring `Retry-After`; 500, 503 and empty responses keep their retries.
- While paused, cycles still run reporting, and stops/take-profits keep working because they are server-side bracket orders, but no decisions are made.
- Held positions are evaluated before candidates, so they win when the budget is tight. Non-held symbols with RSI between 40 and 60 and no new news are skipped without calling Gemini.

## Risk controls

All limits are enforced in code and apply to BUYs; SELL and the server-side stop-loss legs keep working in every state.

- **Sizing:** target position = % of account equity by conviction (7 -> 5%, 8 -> 7%, 9-10 -> 10%), capped by `MAX_POSITION_PCT`, the optional `MAX_POSITION_USD`, and spendable cash (the lesser of Alpaca's `cash` and `buying_power`, so margin is never used). Value already held in the symbol counts toward the target. Conviction below 7 never buys.
- **Position count:** no new symbol once `MAX_OPEN_POSITIONS` are open.
- **Sector cap:** at most 30% of equity in one sector (yfinance sector data); the order is trimmed to fit. If sector data is missing (e.g. ETFs) the rule is skipped and logged.
- **Bracket orders:** stop-loss at 2x ATR(14) below entry; take-profit at 2x the risk distance unless Gemini proposes one, which must also be >= 2x. A trade with reward:risk below 2 is rejected. Brackets are GTC so the exit legs survive overnight.
- **Daily loss breaker:** if equity is down 3% or more from the previous close, no new positions until the next session.
- **Drawdown halt:** if equity is 10% or more below its peak, BUYs halt and a loud `[RISK]` line is logged every cycle until drawdown recovers. The peak is stored in `data/state.json`.
- **Entry buffer:** no new BUY in the first or last 10 minutes of the session (half days respected via Alpaca's calendar).
- **Long-only:** SELL is only honored when a long position exists, and it closes the whole position after cancelling its open bracket legs.

## Performance tracking

At the first check after each close the bot appends a line to `data/performance.csv` and logs it:

```
[Stats] 2026-10-12 equity=$101234.50 day=+0.42% (SPY +0.30%) cum=+1.20% (SPY +0.80%) positions=3
```

`GET /stats` returns the latest record plus start and peak equity as JSON. Cumulative figures are measured from the first recorded day. `data/` is git-ignored; on hosts with an ephemeral disk (Render free tier) it resets on redeploy or restart, which also resets the peak used by the drawdown halt and the SPY comparison baseline. `/stats` has no authentication.

## Deployment

The included `Dockerfile` builds the Go binary on top of a Python image. `render.yaml` deploys it as a Render web service; the bot exposes `/health` so an uptime monitor can keep the free instance awake. Secrets are set in the Render dashboard, never in the repository.

## Known limitations

- **Gemini free-tier quota** is small (a few hundred requests per day per model, and low per-minute limits). The cooldown, prefilter, rate limiter and daily budget keep usage below it, but a busy account with many held positions can still exhaust it; decisions then pause until 00:00 Pacific.
- **Yahoo rate limits on cloud IPs:** Yahoo often throttles shared hosting IPs (Render included). Technicals therefore come from Alpaca's free IEX bars, cached 10 minutes; only fundamentals (beta, P/E, sector, average volume) use Yahoo, cached 24 hours. If Yahoo fails, indicators still work but average volume is unknown, so new symbols fail the liquidity filter until fundamentals load (held positions are still evaluated and sold). Data failures for a held position log a WARNING and leave its bracket orders untouched.
- IEX volume covers only a fraction of market volume, which is why liquidity uses Yahoo's average volume rather than the bars.
- Indicators use daily bars but the loop runs every 15 minutes, so consecutive cycles often see near-identical data.
- No backtesting and no persistence of decisions beyond logs.
- Ichimoku Span A/B read as 0 because three months of daily bars is too short for the 52-period spans shifted by 26.

## Roadmap

- Decision log in a database plus a small dashboard
- Backtesting harness to evaluate the prompt and indicators offline