# AutoTradingBot

An autonomous paper-trading bot written in Go. Every five minutes during US market hours it combines technical indicators, recent news and an LLM (Gemini 2.5 Flash) to decide whether to **BUY**, **SELL** or **HOLD** each symbol in a core watchlist plus a few screened candidates, and executes the decision through Alpaca's paper-trading API. It is **long-only**: no shorting, margin, leverage or options.

> **Paper trading only.** This is a personal/educational/portfolio project, not financial advice. It has not been backtested and should not be pointed at a live account.

## How it works

```
every 5 min (Mon–Fri, market hours per Alpaca's clock/calendar)
  ├─ account + positions → circuit breakers, open/close buffer
  ├─ universe = core watchlist + up to N validated candidates
  │     (Alpaca screener, tickers in cached news, Gemini suggestions)
  └─ for each symbol
       ├─ Python (yfinance) → RSI, Ichimoku, MACD, Stochastic, Keltner, ATR, volume, sector
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
- **Python 3.11** (`yfinance`, `pandas`): indicator calculation, called from Go as a subprocess
- **Alpaca Markets** paper API: orders, positions, news
- **Google Gemini 2.5 Flash**: decision engine
- **Docker** and **Render**: deployment

## Project structure

```
cmd/bot/                 entrypoint, health endpoint, graceful shutdown
internal/scheduler/      5-minute market cycle, order flow, daily stats trigger
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
| `LOG_LEVEL` | `debug` also logs Gemini's full reasoning; anything else logs one line per decision (default `info`) |
| `PORT` | Port for the `/health` and `/stats` endpoints (default `8080`) |

## Dynamic universe

The core watchlist (SPY, QQQ, AAPL, MSFT, NVDA, AMZN, GOOG) is always evaluated. Each cycle up to `UNIVERSE_MAX_CANDIDATES` more symbols are added from Alpaca's most-actives and movers screeners, tickers mentioned in the cached Alpaca news, and `suggested_tickers` returned by Gemini (these join the **next** cycle's pool). Every symbol, core included, must pass these filters in code before Gemini is called or any order is placed:

- exists on Alpaca (`GET /v2/assets/{symbol}`), status `active`, tradable, class `us_equity`
- exchange is NYSE, NASDAQ or ARCA (no OTC)
- price >= $5 and average daily volume >= 1M shares (from the yfinance data)
- not a leveraged or inverse ETF by name
- market data fetch succeeded (otherwise the symbol is skipped and logged)

Positions already held that drop out of the universe are still evaluated, SELL-only.

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

- Indicators use daily bars but the loop runs every five minutes, so consecutive cycles often see near-identical data.
- No backtesting and no persistence of decisions beyond logs.
- Ichimoku Span A/B read as 0 because three months of daily bars is too short for the 52-period spans shifted by 26.

## Roadmap

- Decision log in a database plus a small dashboard
- Backtesting harness to evaluate the prompt and indicators offline