# AutoTradingBot

An autonomous paper-trading bot written in Go. Every five minutes during US market hours it combines technical indicators, recent news and an LLM (Gemini 2.5 Flash) to decide whether to **BUY**, **SELL** or **HOLD** each symbol on a watchlist, and executes the decision through Alpaca's paper-trading API.

> **Paper trading only.** This is a personal/educational/portfolio project, not financial advice. It has not been backtested and should not be pointed at a live account.

## How it works

```
every 5 min (Mon–Fri, 9:30–16:00 ET)
  └─ for each symbol in the watchlist
       ├─ Python (yfinance) → RSI, Ichimoku, MACD, Stochastic, Keltner, beta, P/E
       ├─ Alpaca → current position and unrealized P&L
       ├─ latest news (Alpaca News API, refreshed every 4 h)
       ├─ prompt → Gemini 2.5 Flash → JSON decision {action, quantity, reasoning}
       └─ BUY / SELL → market order on Alpaca (paper)
```

The model is instructed to trade only stocks and ETFs on a swing-trading horizon, to justify every decision with both technicals and news, and to default to HOLD when there is no clear edge.

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
internal/scheduler/      5-minute market cycle and 4-hour news refresh
internal/broker/         Alpaca REST client (orders, positions)
internal/news/           Alpaca News client
internal/gemini/         prompt builder and Gemini client
internal/portfolio/      position lookup
internal/market/         market-hours check
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
| `ALPACA_BASE_URL` | Defaults to `https://paper-api.alpaca.markets` |
| `GEMINI_API_KEY` | Google AI Studio key |
| `MAX_POSITION_USD` | Max position size per symbol in USD; BUY quantity is capped so current + new value doesn't exceed this (default `5000`) |
| `PORT` | Port for the `/health` endpoint (default `8080`) |

## Deployment

The included `Dockerfile` builds the Go binary on top of a Python image. `render.yaml` deploys it as a Render web service; the bot exposes `/health` so an uptime monitor can keep the free instance awake. Secrets are set in the Render dashboard, never in the repository.

## Known limitations

- Indicators use daily bars but the loop runs every five minutes, so consecutive cycles often see near-identical data.
- No backtesting and no persistence of decisions beyond logs.

## Roadmap

- Decision log in a database plus a small dashboard
- Backtesting harness to evaluate the prompt and indicators offline