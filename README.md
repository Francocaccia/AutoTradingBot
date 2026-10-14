# AutoTradingBot

> Paper trading only. Not financial advice. This is an educational/engineering project; an LLM making trade decisions is experimental and can lose money.

An autonomous paper-trading bot written in Go. Every 15 minutes (configurable) during US market hours it combines technical indicators, recent news, portfolio context and an LLM (Gemini 2.5 Flash) to decide whether to **BUY**, **SELL** or **HOLD** each symbol in a core watchlist plus a few screened candidates, and executes the decision through Alpaca's paper-trading API. It is **long-only**: no shorting, margin, leverage or options.

## Goal

The bot manages a small **multi-asset investor portfolio** (stocks, equity ETFs, bond ETFs and cash), not a stock picker. The objective is to **maximize risk-adjusted return (Sharpe-like) subject to a drawdown limit**, benchmarked against **SPY** and a **60/40 (SPY/AGG)** portfolio. "Highest return with lowest risk" is a trade-off: the bot must never take extra risk just to chase return, and must never trade just to be active. The same framing is the first thing the model reads in every prompt.

## Live track record

[autotradingbot-kjr0.onrender.com/stats](https://autotradingbot-kjr0.onrender.com/stats): percentages only, from the paper account, benchmarked against SPY, tracking since `TRACK_START_DATE` (default `2026-10-13`). Computed on demand from Alpaca and cached for 5 minutes; the free Render instance may take about a minute to wake up. Before two trading days of data exist, every metric is `null` and `note` is `"insufficient data"`.

Example shape (**placeholder numbers, not real results**):

```json
{
  "start_date": "2026-10-13",
  "days_tracked": 20,
  "bot_return_pct": 1.84,
  "spy_return_pct": 2.31,
  "excess_return_pct": -0.47,
  "max_drawdown_pct": -2.65,
  "best_day_pct": 1.12,
  "worst_day_pct": -1.38,
  "num_trades": 14,
  "win_rate_pct": 57.14,
  "open_positions_count": 3,
  "benchmark_60_40_return_pct": 1.12,
  "bot_vs_60_40_pct": 0.72,
  "max_drawdown_spy_pct": -3.4,
  "max_drawdown_60_40_pct": -1.9,
  "risk_adjusted": {"bot": 1.31, "spy": 1.05, "benchmark_60_40": 1.22},
  "allocation_pct": {"equities": 58.2, "bonds": 22.5, "cash": 19.3},
  "last_updated": "2026-11-10T21:05:00Z"
}
```

Returns are measured from the close before the start date. `max_drawdown_pct` is the worst peak-to-trough decline of the daily equity curve (negative). `win_rate_pct` counts round trips opened and closed since the start (average-cost P&L) and is `null` until one closes. `benchmark_60_40_return_pct` is 60% SPY / 40% AGG rebalanced daily. `risk_adjusted` is mean daily return over its standard deviation, annualized (no risk-free rate); it reads `"insufficient data"` until 20 trading days exist. `allocation_pct` splits equity into sleeves (cash includes T-bill ETFs). No dollar amounts, share counts, held symbols or account identifiers are ever returned.

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

The model is told the bot is long-only, must ground its answer in the supplied indicators and news, and is told that doing nothing is a valid, often best, decision; it may BUY only with a quoted news catalyst plus a multi-week technical setup. Position size, stop-loss and every limit below are enforced in code, not by the prompt.

Each decision logs one line, `[SYM] ACTION conviction=N | thesis`; set `LOG_LEVEL=debug` to also log Gemini's full reasoning.

Sample output (illustrative; real logs will replace it):

```
[Market] Open — running decision cycle
[AAPL] HOLD conviction=5 | <one-line thesis>
[NVDA] BUY conviction=8 | <one-line thesis>
```

## Architecture

```
scheduler → universe filters → indicators → Gemini decision (JSON) → risk checks → Alpaca bracket orders
```

1. **Scheduler** (`internal/scheduler`): ticks every `CYCLE_INTERVAL_MIN`, checks Alpaca's clock and calendar, and skips closed sessions.
2. **Universe filters** (`internal/universe`): core watchlist plus screened candidates, each passing the hard filters (exchange, price, volume, no leveraged ETFs or warrants).
3. **Indicators**: Alpaca IEX daily bars feed a Python script (RSI, Ichimoku, MACD, Stochastic, Keltner, ATR); `yfinance` supplies fundamentals (beta, P/E, sector, average volume).
4. **Gemini decision**: the prompt carries indicators, news and the current position; the model replies with JSON (`action`, `conviction`, `thesis`, `reasoning`, `suggested_tickers`).
5. **Risk checks** (`internal/risk`): sizing by conviction, position and sector caps, reward:risk >= 2, circuit breakers. Enforced in code, not by the prompt.
6. **Execution**: market entry with server-side stop-loss and take-profit legs (Alpaca bracket orders).

Free-tier constraints: Gemini's free quota is small, so calls are rate limited, budgeted and gated by a per-symbol cooldown. Render's free web service sleeps after inactivity, so an UptimeRobot monitor pings `/health` to keep it awake (a sleeping bot runs no cycles). Render's disk is ephemeral, which is why nothing is persisted and `/stats` is derived from Alpaca.

## Stack

- **Go 1.22**: scheduler, Alpaca and Gemini clients, order execution
- **Python 3.11** (`yfinance`, `pandas`): indicator calculation, called from Go as a subprocess. Daily bars come from Alpaca market data (IEX feed, free) with a batched `yfinance` fallback; `yfinance` supplies fundamentals (beta, P/E, sector, average volume)
- **Alpaca Markets** paper API: orders, positions, news
- **Google Gemini 2.5 Flash**: decision engine
- **Docker** and **Render**: deployment

## Project structure

```
cmd/bot/                 entrypoint, health endpoint, graceful shutdown
internal/scheduler/      market cycle, evaluation gate (cooldown/prefilter), order flow
internal/broker/         Alpaca REST client (orders, positions, assets, calendar)
internal/news/           Alpaca News client
internal/marketdata/     Alpaca screener and SPY daily bars
internal/gemini/         prompt builder and Gemini client (retry + fallback model)
internal/universe/       dynamic universe and hard symbol filters
internal/risk/           sizing, bracket levels, circuit breakers, sector limit
internal/trades/         position age, re-entry and turnover counts from Alpaca order history
internal/portfolio/      account, positions, investment policy (bands, limits) and the curated bond ETFs
internal/analysis/       60d volatility, beta, correlation and the PORTFOLIO prompt block
internal/regime/         deterministic macro regime label from yields, VIX and 200d trends
internal/stats/          GET /stats (computed from Alpaca, cached) and the in-memory equity peak
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
| `GEMINI_MODEL` | Primary Gemini model (default `gemini-2.5-flash`). Checked against `GET /v1beta/models` at startup; a model that is not listed is dropped with a WARN instead of being called |
| `GEMINI_FALLBACK_MODEL` | Model used when the primary is unavailable or its per-day quota is spent (switch for the rest of the day), and tried once after the primary exhausts its retries (default `gemini-3.5-flash-lite`; `gemini-2.5-flash-lite` now returns 404 for new users). Same startup check as `GEMINI_MODEL`; set it equal to `GEMINI_MODEL` to disable the fallback |
| `MAX_POSITION_PCT` | Hard cap per position as % of account equity (default `10`) |
| `MAX_POSITION_USD` | Optional absolute cap per position in USD; unset or `0` disables it |
| `MAX_OPEN_POSITIONS` | Max simultaneous open positions (default `8`) |
| `UNIVERSE_MAX_CANDIDATES` | Extra screened candidates per cycle on top of the core watchlist (default `3`; `0` disables the dynamic universe). A candidate is only sent to Gemini if a cached headline mentions its ticker |
| `GEMINI_MAX_RPM` | Global cap on Gemini requests per minute, retries included; calls wait for room (default `8`) |
| `GEMINI_DAILY_BUDGET` | Max successful (HTTP 200) Gemini calls **per model** per day (America/Los_Angeles day), counted in memory; callers stop by priority as it is spent (default `18`). When a 429 reports the real per-day limit, that value replaces this number for that model for the rest of the day |
| `BATCH_MAX_SYMBOLS` | Most symbols sent in one Gemini batch call (default `12`). If more are due, held positions go first, then core symbols, then candidates; the rest wait for the next cycle |
| `EVAL_COOLDOWN_MIN` | A HELD symbol is sent to Gemini at most once per this many minutes, unless price moved >2%, RSI crossed 30/70, or new news mentions it; `0` disables. Symbols we do not hold are evaluated once per trading day after 10:00 ET and ignore those exceptions (default `90`) |
| `MAX_RISK_PCT` | Cap on loss-at-stop per BUY, as % of equity: `qty * (entry - stop) <= MAX_RISK_PCT` (default `1.0`) |
| `MIN_BUY_CONVICTION` | Minimum conviction for a BUY; a BUY also needs a catalyst quoted from the headlines shown to the model (default `8`) |
| `MIN_HOLD_DAYS` | No SELL of a position opened fewer than this many trading days ago, unless a circuit breaker is active; `0` disables (default `3`) |
| `REENTRY_COOLDOWN_DAYS` | No BUY of a symbol for this many trading days after any sell of it; `0` disables (default `3`) |
| `MAX_TRADES_PER_DAY` | Max orders the bot opens from its own decisions per day (default `2`) |
| `MAX_TRADES_PER_WEEK` | Same, over a rolling 5 trading days (default `5`) |
| `GUARDS_START_DATE` | ISO date (ET); decisions before it do not count toward the daily/weekly caps (default `2026-10-14`, the deploy date of the `atb-` order tagging) |
| `BOND_MODE` | `auto` (default), `etf`, `native` or `off`. At startup the bot asks Alpaca whether fixed income is tradable; `auto` uses native bonds if supported, otherwise bond ETFs. See "Portfolio construction" |
| `BOND_ETFS` | Optional comma list limiting which curated bond ETFs may be bought (default: all of SGOV, BIL, SHY, IEI, IEF, TLT, TIP, AGG, BND, LQD, VCSH, HYG, EMB) |
| `IPS_EQUITY_MIN`, `IPS_EQUITY_MAX` | Equity sleeve band, % of equity (defaults `40`, `75`) |
| `IPS_BOND_MIN`, `IPS_BOND_MAX` | Bond sleeve band (defaults `15`, `40`) |
| `IPS_CASH_MIN`, `IPS_CASH_MAX` | Cash / T-bill ETF sleeve band (defaults `5`, `30`) |
| `IPS_TARGET_EQUITY`, `IPS_TARGET_BOND`, `IPS_TARGET_CASH` | Starting sleeve targets, used to size bond purchases; the weekly review may move them inside the bands (defaults `60`, `30`, `10`) |
| `IPS_MAX_BOND_ETF_PCT` | Largest single bond ETF, % of equity (default `15`) |
| `IPS_SECTOR_MAX_PCT` | Largest single sector of the equity sleeve (default `30`) |
| `IPS_CREDIT_MAX_PCT` | HYG / EMB-type credit-risk bonds combined (default `10`) |
| `IPS_MAX_DURATION` | Portfolio weighted bond duration cap, years (default `8`) |
| `IPS_MIN_REAL_CASH_PCT` | Actual cash always kept, % of equity (default `2`) |
| `CYCLE_INTERVAL_MIN` | Minutes between market cycles (default `15`) |
| `TRACK_START_DATE` | First day (`YYYY-MM-DD`) counted by `/stats` (default `2026-10-13`) |
| `LOG_LEVEL` | `debug` also logs Gemini's full reasoning; anything else logs one line per decision (default `info`) |
| `PORT` | Port for the `/health` and `/stats` endpoints (default `8080`) |

## Selective by design

Cash and HOLD are positions. Most days should end with zero or one trade, and a week with two or three well-founded trades beats ten trades a day. Paper trading hides spread and slippage, so churn looks free here but is not. The prompt says so, and code enforces it: a BUY needs conviction >= `MIN_BUY_CONVICTION` and a catalyst that is one of the headlines shown to the model; positions are not sold before `MIN_HOLD_DAYS`; a sold symbol is not re-bought for `REENTRY_COOLDOWN_DAYS`; and `MAX_TRADES_PER_DAY` / `MAX_TRADES_PER_WEEK` cap turnover. Decisions are collected each cycle and executed afterwards, risk-reducing SELLs first, then BUYs by conviction, so a cap binds on the weakest orders. Position age, last sell and trade counts are rebuilt from Alpaca's filled orders (not local state), so restarts do not reset them. **Only decision-initiated orders are counted:** every order the bot submits carries a `client_order_id` starting with `atb-`, and the counters count filled parent orders with that prefix, never bracket/OTO child legs (`parent_order_id` set), stop/limit fills, cancelled or replaced orders. Legacy orders without the prefix count only if they are parents (order class none, simple, bracket or oto) shaped like a decision (a market buy, or a plain market sell). At startup the bot logs a one-time `[Trades] startup breakdown` listing each counted order (symbol, side, time, `client_order_id`) and how many others were left out, so the count can be verified by eye; `GUARDS_START_DATE` resets the counters to orders on or after that date; a legacy position with no order in the last 45 days has unknown age and no holding restriction. Sizing ladder: conviction 8 = 5% of equity, 9 = 7%, 10 = 10%.

## Portfolio construction

The bot works against an **investment policy statement (IPS)**, enforced in code (`internal/portfolio/policy.go`), not left to the model:

| Sleeve | Band (% of equity) | Holds |
|---|---|---|
| Equities | 40-75 | single stocks and equity ETFs |
| Bonds | 15-40 | bond ETFs (SHY, IEI, IEF, TLT, TIP, AGG, BND, LQD, VCSH, HYG, EMB) |
| Cash | 5-30 | cash and T-bill ETFs (SGOV, BIL) |

Limits: single stock <= 10% (`MAX_POSITION_PCT`), single bond ETF <= 15%, one sector <= 30% (an unknown sector counts as its own shared "Unknown" bucket, never as free diversification; broad index ETFs are exempt), credit-risk bonds (HYG, EMB) combined <= 10%, weighted bond duration <= 8 years (static duration table), at most `MAX_OPEN_POSITIONS` positions. A BUY is trimmed to the room the policy leaves, or rejected with the binding constraint logged if not even one share fits. Drift outside a band is never fixed with panic trades: it is handled by the weekly review.

**Bonds on this account.** At startup the bot calls `GET /v2/assets?status=active&asset_class=fixed_income` and logs a `[Capabilities]` line with exactly what Alpaca answered (HTTP status and asset count), then checks that each curated bond ETF is tradable. Alpaca's standard asset classes are US equities, options and crypto, so on a standard paper account individual Treasuries, corporate and sovereign bonds are **not expected to be tradable** (the probe reports what the account actually says) and **bond ETFs are the practical stand-in** (LQD / VCSH for investment-grade corporates, EMB for emerging-market sovereigns, IEF / TLT for Treasuries). The bot does not fake bond trading: even if the endpoint ever reports native bonds, it has no native order flow, logs that, and trades the ETFs. `BOND_MODE=off` disables the bond sleeve.

**Bond ETF sizing and stops.** Bond ETFs are not sized by the conviction ladder. Each purchase moves the bond sleeve toward its target weight by at most 5% of equity. T-bill ETFs get a plain market order; other bond ETFs get a market entry with a 3x ATR stop and no take-profit (an Alpaca OTO order), and the weekly review revisits them. The `MAX_RISK_PCT` cap uses the stop actually applied.

**Portfolio context.** Every decision prompt carries a `PORTFOLIO` block computed in Go: sleeve weights vs the bands, actual cash, top 5 positions, sector weights, bond duration, 60-day realized volatility and beta to SPY, drawdown from peak, performance vs SPY and vs 60/40 since `TRACK_START_DATE`, and for a candidate its average 60-day correlation to current holdings and its effect on sleeve and sector weights. A **macro regime** block (cached 1 hour, from Yahoo through the existing worker) shows the 13-week, 5-, 10- and 30-year yields (`^IRX`, `^FVX`, `^TNX`, `^TYX`), the 10y-3m slope, VIX, and SPY and TLT against their 200-day averages, with a label from simple rules: `risk-off` (VIX >= 25 or SPY below its 200d MA), `risk-on` (VIX < 20 and SPY above), otherwise `neutral`, plus `rates-rising` (TLT below its 200d MA) and `inverted curve` (10y below 13w). If any series is missing the regime is `unavailable`, the bot keeps running, and only SGOV, BIL and SHY may be bought.

**Decision rules.** Every BUY or SELL must name a `role` (core, growth, income, hedge, cash_parking) and a `portfolio_rationale`. A bond ETF BUY needs conviction >= `MIN_BUY_CONVICTION` and a catalyst that is **one** item of the PORTFOLIO block: one full line, or one `label: value` token of a line (case and whitespace are normalized; a concatenation of several lines or phrases is rejected); stock BUYs need a quoted headline. A BUY whose average correlation to holdings exceeds 0.8 is rejected unless `replaces` names a held symbol, in which case that position is sold first (both orders must fit the holding period and turnover caps, or neither is placed). Cash inside its band is not a reason to buy anything: parking cash in SGOV / BIL is only accepted when actual cash is above its band maximum. All turnover guards apply to bond ETF trades.

**Weekly review.** One extra Gemini call on the first eligible cycle on Monday after 10:00 ET (or once a day when a sleeve is more than 5 points outside its band) sees the whole portfolio and regime and returns target sleeve weights plus at most 2 actions. Targets are clipped into the bands (or rejected if they cannot sum to 100); actions become ordinary decisions that pass every guard. Review SELLs are exempt from `MIN_HOLD_DAYS` but count toward the trade caps. The outcome is logged on one `[Review]` line. The review runs at most once per process per day, and never while Gemini is paused (quota or budget).

**What the review may buy.** Bond ETFs of the sleeve, T-bill cash-parking ETFs (SGOV, BIL) and the core equity ETFs (SPY, QQQ), validated against the IPS (the sleeve must be under its target, band maxima, single-name limits) rather than "bond ETFs only". When the review is caused by a band breach (a sleeve more than 5 points outside its band) the **code** writes the catalyst (for example `band breach: cash 69.0% > 30% max`) and skips the quote check; `portfolio_rationale`, conviction, the per-trade risk cap and the daily/weekly trade caps still apply, and the correlation rule is waived for these code-driven rebalance BUYs. A large breach is closed gradually: at most 5% of equity per trade (never past the sleeve target) and at most `MAX_TRADES_PER_DAY` trades a day, so a 39-point breach takes several days. Each rebalance BUY logs `rebalance in progress, N points remaining`.

**Benchmarks.** `/stats` compares the bot with SPY and with a 60/40 SPY/AGG portfolio rebalanced daily, including their max drawdowns and a risk-adjusted ratio.

## Dynamic universe

The core watchlist (SPY, QQQ, AAPL, MSFT, NVDA, AMZN, GOOG) is always evaluated. Each cycle up to `UNIVERSE_MAX_CANDIDATES` more symbols are added from Alpaca's most-actives and movers screeners, tickers mentioned in the cached Alpaca news, and `suggested_tickers` returned by Gemini (these join the **next** cycle's pool). Every symbol, core included, must pass these filters in code before Gemini is called or any order is placed:

- exists on Alpaca (`GET /v2/assets/{symbol}`), status `active`, tradable, class `us_equity`
- exchange is NYSE, NASDAQ or ARCA (no OTC)
- price >= $5 and average daily volume >= 1M shares (from the yfinance data). The core watchlist and the bond/ETF sleeve are **exempt from the volume filter**: if the volume is unavailable (Yahoo failing from a cloud IP) a `WARN` is logged and the symbol is kept. Screener candidates still fail closed (rejected when volume is unavailable). Yahoo fundamentals are retried with jittered backoff (about 2s, 5s; an empty Yahoo reply counts as a failure and is retried), and the last good average volume per symbol is kept for 24 hours as a fallback (logged whenever it is used)
- **liquidity fallback (candidates only, never core):** if Yahoo's average volume is unavailable, the average daily **dollar** volume over 60 sessions of Alpaca daily bars is used instead and must be >= `LIQUIDITY_MIN_DOLLAR_VOL` (default `20000000`). The accepted-candidate log line says which source was used. Alpaca's free IEX feed carries only a slice of consolidated volume, so this figure understates real liquidity and the fallback is conservative. If both sources fail the candidate is rejected
- **candidate quality (screener candidates only; core and bond ETFs are exempt):** rejected when the last bar's close is more than `CANDIDATE_MAX_DAY_MOVE_PCT` (default `20`) away from the previous close, its intraday range (high-low)/close is above `CANDIDATE_MAX_RANGE_PCT` (default `15`), ATR(14)/price is above `CANDIDATE_MAX_ATR_PCT` (default `7`), or market cap is below `CANDIDATE_MIN_MARKET_CAP_USD` (default `2e9`). Market cap comes from the Yahoo fundamentals already fetched; when it is unavailable that one check is skipped. Each rejection is logged with the measured value; `0` disables a check
- not a leveraged or inverse ETF by name
- market data fetch succeeded (otherwise the symbol is skipped and logged)

Filters also reject warrants, rights and units (Alpaca asset name containing "Warrant", "Right" or "Unit", and 5-letter tickers ending in W, WS, R or U) and skip a candidate whose other share class is already represented (e.g. GOOGL when GOOG is in the core list).

The filters apply **only to opening new positions**. The bot manages every position in the account: each held symbol is added to the evaluation set automatically, evaluated for SELL on the cooldown schedule even if it is not in the core list or candidate pool, and can be sold even if it fails the BUY filters (e.g. a held low-volume ETF).

## Gemini quota and call volume

- **One call per cycle.** Every symbol due for evaluation (held positions past their cooldown, plus symbols not yet evaluated today) is sent in **one** Gemini call: each symbol's own context block (indicators, rel_volume, the 5 most recent headlines, position state, days held, last action) plus the shared PORTFOLIO / macro regime block once. Structured output (`responseSchema`) makes the reply a JSON array with one object per symbol. Every item is then checked independently by the same guards as before (catalyst quote, `MIN_BUY_CONVICTION`, risk cap, IPS limits, trade caps, `MIN_HOLD_DAYS`, cooldowns): a bad item invalidates only itself, and a symbol the model leaves out is treated as HOLD and logged. The weekly / band-breach review stays a **separate** call (its answer has a different shape and rules; mixing them made the prompt unreliable), so a Monday cycle can use two calls.
- **If the batch call fails** (503 after the client's own retries, or a reply that is not a JSON array) the bot falls back to one call per symbol only when the remaining daily budget is larger than the number of symbols; otherwise it skips the cycle and logs why.
- **Models.** At startup the bot lists models (`GET /v1beta/models`, no generate quota) and drops any configured model that is missing. A 404, 400 or 403 on a model at runtime removes it for the rest of the process, is never retried and does **not** count as a call; the next model in the chain (`GEMINI_MODEL`, then `GEMINI_FALLBACK_MODEL`) is used.
- Every request, retries included, passes a global rate limiter (`GEMINI_MAX_RPM`). Only successful (HTTP 200) calls count against the daily budget, which is tracked **per model** (`GEMINI_DAILY_BUDGET`, default 18, replaced by the limit a per-day 429 reports). The cycle summary logs the symbols in the batch, how many were accepted, rejected (with reasons) or missing, the models used and the calls today per model.
- **429s are classified from the response body** (`error.details`: `QuotaFailure` violations and `RetryInfo`). The log gets one clean line with the quota metric, quota id and limit value, never the raw JSON. A **per-minute** limit is retried after the `RetryInfo` delay (falling back to `Retry-After`). A **per-day** limit is never retried: the primary model is marked spent and calls switch to `GEMINI_FALLBACK_MODEL` for the rest of the day (logged). Only when both models are spent does the bot pause until 00:00 America/Los_Angeles. 500, 503 and empty responses keep their retries, then the fallback model is tried once.
- **Budget vs quota.** The budget is a local limit and is logged as `budget`, separately from `quota` (Google's limit). When the budget is limited, priority is: 1) held positions, 2) the weekly / band-breach review, 3) core symbols, 4) screener candidates. Each tier stops at a share of the budget (candidates 50%, core 75%, review 90%, held 100%), so the calls that remain are kept for the more important ones; once a tier is out, nothing below it is called that cycle.
- While paused, no decisions are made, but stops/take-profits keep working because they are server-side bracket orders. The review does not run while Gemini is paused.
- Non-held symbols with RSI between 40 and 60 and no new news are skipped without calling Gemini.
- **The "calls today" counter is in memory** and resets when the process restarts (Render restarts, deploys), so it can under-count. The 429 responses are the real source of truth for the quota.

## Risk controls

All limits are enforced in code and apply to BUYs; SELL and the server-side stop-loss legs keep working in every state.

- **Sizing:** target position = % of account equity by conviction (8 -> 5%, 9 -> 7%, 10 -> 10%), capped by `MAX_POSITION_PCT`, the optional `MAX_POSITION_USD`, and spendable cash (the lesser of Alpaca's `cash` and `buying_power`, so margin is never used). Value already held in the symbol counts toward the target. Conviction below `MIN_BUY_CONVICTION` never buys.
- **Per-trade risk:** after sizing and the caps below, the quantity is cut so the loss at the stop is at most `MAX_RISK_PCT` of equity (a 23%-away stop shrinks the position); under one share skips the BUY.
- **Position count:** no new symbol once `MAX_OPEN_POSITIONS` are open.
- **Sector cap:** at most 30% of equity in one sector (yfinance sector data); the order is trimmed to fit. Symbols with no sector data share one "Unknown" bucket under the same cap (broad index ETFs such as SPY and QQQ are exempt).
- **Bracket orders:** stop-loss at 2x ATR(14) below entry; take-profit at 2x the risk distance unless Gemini proposes one, which must also be >= 2x. A trade with reward:risk below 2 is rejected. Brackets are GTC so the exit legs survive overnight.
- **Daily loss breaker:** if equity is down 3% or more from the previous close, no new positions until the next session.
- **Drawdown halt:** if equity is 10% or more below its peak, BUYs halt and a loud `[RISK]` line is logged every cycle until drawdown recovers. The peak is held in memory, seeded from Alpaca's equity history since `TRACK_START_DATE` (so it survives restarts but ignores earlier highs).
- **Entry buffer:** no new BUY in the first or last 10 minutes of the session (half days respected via Alpaca's calendar).
- **Long-only:** SELL is only honored when a long position exists, and it closes the whole position after cancelling its open bracket legs.

**Review visibility.** Every cycle in which the weekly or band-breach review does not run logs one `[Review] not run: <reason> | cash X% of equity, band lo-hi%` line (not due, before 10:00 ET, already ran today, Gemini paused or out of budget, trade cap reached, market closed, scheduler paused). A review that runs logs its outcome as before.

## Deployment

The included `Dockerfile` builds the Go binary on top of a Python image. `render.yaml` deploys it as a Render web service; the bot exposes `/health` so an uptime monitor (UptimeRobot) can keep the free instance awake. Nothing is written to disk. Secrets are set in the Render dashboard, never in the repository.

## Known limitations

- **Gemini free-tier limits are low.** `gemini-2.5-flash` allows 20 requests per day per project per model as observed in production. Batching (one call per cycle for every due symbol), the cooldown, prefilter, rate limiter, per-model priority-tiered budget and the fallback model keep usage inside that, but both models can still be spent on a busy day; decisions then pause until 00:00 Pacific. Free-tier model availability also changes (`gemini-2.5-flash-lite` was closed to new users), which is why the model names are configurable and checked at startup.
- **In-memory counters reset on a Render restart:** the Gemini per-model budget counter ("calls today"), the learned per-day limits, the last-decision memory, the evaluation gate and the review-day markers live in memory, so after a restart the bot can over-spend the real daily quota (the 429 responses then stop it) and can repeat one evaluation or review. Position age, cooldowns and trade counts are rebuilt from Alpaca and survive restarts.
- **Yahoo can fail from cloud IPs:** Yahoo often throttles shared hosting IPs (Render included). Technicals therefore come from Alpaca's free IEX bars, cached 10 minutes; only fundamentals (beta, P/E, sector, average volume) use Yahoo, cached 12 hours, retried with backoff, with the last good average volume kept 24 hours as a fallback. If Yahoo stays down, indicators still work but average volume is unknown: core symbols and bond ETFs are kept anyway (with a WARN), while new screener candidates fail the liquidity filter until fundamentals load (held positions are still evaluated and sold). Data failures for a held position log a WARNING and leave its bracket orders untouched.
- **LLM conviction is not calibrated**, so the conviction, catalyst and turnover guards are a blunt control until a decision log exists to tune them against outcomes.
- **Bond ETFs carry rate and credit risk.** They are not cash: long duration loses when rates rise and HYG / EMB fall with equities in a sell-off. The duration and credit caps use a static table of approximate durations.
- **Paper trading ignores slippage and liquidity reality**: fills are optimistic, so a strategy that looks fine here can be worse live. Low turnover is partly a defence against fooling ourselves.
- **Tracking started with an existing legacy portfolio**: positions opened before `TRACK_START_DATE` have unknown age (no holding restriction) and are excluded from the win rate, and the allocation bands may start out of range.
- Correlation, volatility and beta use 60 daily IEX closes; with fewer than 30 overlapping days they read as unavailable and the correlation rule is not applied.
- Intraday volume is never sent to the model (the IEX feed shows a small slice of consolidated volume). It sees last completed session volume vs the 60-session average, computed from the same bars.
- **IEX volume is a partial feed** covering only a fraction of market volume, which is why liquidity uses Yahoo's average volume rather than the bars, and why the model only sees `rel_volume` (last completed session vs the 60-session average of the same bars; omitted when fewer than 60 baseline sessions exist).
- Indicators use daily bars but the loop runs every 15 minutes, so consecutive cycles often see near-identical data.
- No backtesting and no persistence of decisions beyond logs. `/stats` is only as complete as Alpaca's history: positions opened before `TRACK_START_DATE` are excluded from the win rate, and an intraday request mixes the current partial day for both the bot and SPY.
- Ichimoku Span A/B read as 0 because three months of daily bars is too short for the 52-period spans shifted by 26.

## Roadmap

- Decision log in a database plus a small dashboard
- Backtesting harness to evaluate the prompt and indicators offline