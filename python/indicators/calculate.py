"""Indicator / fundamentals worker.

Usage:
    calculate.py bars SYM [SYM ...]          technicals for all symbols, one batched fetch
    calculate.py fundamentals SYM [SYM ...]  beta, P/E, sector, name, average volume (yfinance)

Contract with the Go caller: stdout carries exactly one JSON object and nothing
else ({"SYM": {...} | {"error": "..."}} or a top-level {"error": "..."});
warnings and logging go to stderr; the exit code is 0 even on failure.
"""
import sys
import os
import io
import json
import time
import random
import logging
import warnings
import urllib.request
import urllib.parse
from datetime import datetime, timedelta, timezone

warnings.filterwarnings("ignore")
logging.basicConfig(stream=sys.stderr, level=logging.WARNING)

import pandas as pd

MIN_BARS = 52
RATE_LIMIT_MARKERS = ("too many requests", "rate limit", "429", "yfratelimit")


def safe(value):
    if pd.isna(value):
        return 0.0
    return float(value)


def ichimoku(df):
    high, low, close = df["High"], df["Low"], df["Close"]
    tenkan = (high.rolling(9).max() + low.rolling(9).min()) / 2
    kijun  = (high.rolling(26).max() + low.rolling(26).min()) / 2
    span_a = ((tenkan + kijun) / 2).shift(26)
    span_b = ((high.rolling(52).max() + low.rolling(52).min()) / 2).shift(26)
    chikou = close.shift(-26)

    return {
        "tenkan": round(safe(tenkan.iloc[-1]), 4),
        "kijun":  round(safe(kijun.iloc[-1]), 4),
        "span_a": round(safe(span_a.iloc[-27]), 4),
        "span_b": round(safe(span_b.iloc[-27]), 4),
        "chikou": round(safe(chikou.iloc[-27]), 4),
    }


def macd(close, fast=12, slow=26, signal=9):
    ema_fast = close.ewm(span=fast).mean()
    ema_slow = close.ewm(span=slow).mean()
    macd_line = ema_fast - ema_slow
    signal_line = macd_line.ewm(span=signal).mean()
    histogram = macd_line - signal_line

    return {
        "macd":      round(safe(macd_line.iloc[-1]), 6),
        "signal":    round(safe(signal_line.iloc[-1]), 6),
        "histogram": round(safe(histogram.iloc[-1]), 6),
    }


def stochastic(df, k=14, d=3):
    low_min  = df["Low"].rolling(k).min()
    high_max = df["High"].rolling(k).max()
    k_line = 100 * (df["Close"] - low_min) / (high_max - low_min)
    d_line = k_line.rolling(d).mean()

    return {
        "k": round(safe(k_line.iloc[-1]), 4),
        "d": round(safe(d_line.iloc[-1]), 4),
    }


def keltner(df, length=20, scalar=2):
    close = df["Close"]
    high  = df["High"]
    low   = df["Low"]

    ema = close.ewm(span=length).mean()

    tr = pd.concat([
        high - low,
        (high - close.shift()).abs(),
        (low - close.shift()).abs()
    ], axis=1).max(axis=1)

    atr = tr.ewm(span=length).mean()

    return {
        "upper":  round(safe((ema + scalar * atr).iloc[-1]), 4),
        "middle": round(safe(ema.iloc[-1]), 4),
        "lower":  round(safe((ema - scalar * atr).iloc[-1]), 4),
    }


def atr(df, length=14):
    close, high, low = df["Close"], df["High"], df["Low"]
    tr = pd.concat([
        high - low,
        (high - close.shift()).abs(),
        (low - close.shift()).abs()
    ], axis=1).max(axis=1)

    return round(safe(tr.ewm(alpha=1 / length, adjust=False).mean().iloc[-1]), 4)


def avg_volume(df, sessions=60):
    # Exclude the last bar: during the session it is a partial day.
    hist = df["Volume"].iloc[-(sessions + 1):-1]
    return round(safe(hist.mean()), 0)


def rsi(close, length=14):
    delta = close.diff()
    gain  = delta.clip(lower=0).ewm(span=length).mean()
    loss  = (-delta.clip(upper=0)).ewm(span=length).mean()
    rs    = gain / loss

    return round(safe((100 - 100 / (1 + rs)).iloc[-1]), 4)


def is_rate_limited(exc):
    text = (type(exc).__name__ + " " + str(exc)).lower()
    return any(m in text for m in RATE_LIMIT_MARKERS)


def with_retry(fn, attempts=3, base=2.0):
    """Call fn(); on a rate-limit error back off with jitter and retry."""
    for i in range(attempts):
        try:
            return fn()
        except Exception as exc:  # noqa: BLE001
            if i == attempts - 1 or not is_rate_limited(exc):
                raise
            delay = base * (2 ** i) + random.uniform(0, 1.5)
            print(f"rate limited ({exc}); retrying in {delay:.1f}s", file=sys.stderr)
            time.sleep(delay)


def technicals(symbol, df, source):
    if df is None or df.empty:
        raise ValueError(f"no data for {symbol}")
    df = df.dropna(subset=["Close"])
    if len(df) < MIN_BARS:
        raise ValueError(f"not enough data for {symbol} ({len(df)} bars)")
    return {
        "symbol":     symbol,
        "source":     source,
        "price":      round(safe(df["Close"].iloc[-1]), 4),
        "volume":     int(df["Volume"].iloc[-1]),
        "rsi":        rsi(df["Close"]),
        "atr":        atr(df),
        "avg_volume": avg_volume(df),
        "ichimoku":   ichimoku(df),
        "macd":       macd(df["Close"]),
        "stochastic": stochastic(df),
        "keltner":    keltner(df),
    }


def alpaca_bars(symbols):
    """Daily bars from Alpaca market data (IEX feed, free). {sym: DataFrame}."""
    key = os.environ.get("ALPACA_API_KEY", "")
    secret = os.environ.get("ALPACA_API_SECRET", "")
    if not key or not secret:
        raise RuntimeError("ALPACA_API_KEY/ALPACA_API_SECRET not set")
    start = (datetime.now(timezone.utc) - timedelta(days=130)).strftime("%Y-%m-%d")
    rows = {s: [] for s in symbols}
    token = None
    while True:
        params = {"symbols": ",".join(symbols), "timeframe": "1Day", "start": start,
                  "limit": 10000, "adjustment": "split", "feed": "iex"}
        if token:
            params["page_token"] = token
        req = urllib.request.Request(
            "https://data.alpaca.markets/v2/stocks/bars?" + urllib.parse.urlencode(params),
            headers={"APCA-API-KEY-ID": key, "APCA-API-SECRET-KEY": secret})
        with urllib.request.urlopen(req, timeout=30) as resp:
            data = json.loads(resp.read().decode("utf-8"))
        for sym, bars in (data.get("bars") or {}).items():
            rows.setdefault(sym, []).extend(bars)
        token = data.get("next_page_token")
        if not token:
            break
    out = {}
    for sym, bars in rows.items():
        if bars:
            df = pd.DataFrame(bars).rename(columns={"o": "Open", "h": "High", "l": "Low", "c": "Close", "v": "Volume"})
            out[sym] = df[["Open", "High", "Low", "Close", "Volume"]].reset_index(drop=True)
    return out


def yahoo_bars(symbols):
    """One batched yf.download for all symbols. {sym: DataFrame}."""
    import yfinance as yf

    def fetch():
        return yf.download(list(symbols), period="3mo", interval="1d", group_by="ticker",
                           threads=False, progress=False, auto_adjust=False)

    raw = with_retry(fetch)
    out = {}
    for sym in symbols:
        try:
            df = raw[sym] if len(symbols) > 1 else raw
            if isinstance(df.columns, pd.MultiIndex):
                df = df[sym] if sym in df.columns.get_level_values(0) else df.droplevel(0, axis=1)
            out[sym] = df.dropna(how="all")
        except KeyError:
            pass
    return out


def cmd_bars(symbols):
    results, frames, source = {}, {}, "alpaca-iex"
    try:
        frames = alpaca_bars(symbols)
    except Exception as exc:  # noqa: BLE001
        print(f"alpaca bars failed ({exc}); falling back to yfinance", file=sys.stderr)
    missing = [s for s in symbols if s not in frames or len(frames[s]) < MIN_BARS]
    if missing:
        try:
            for sym, df in yahoo_bars(missing).items():
                frames[sym] = df
                results[sym] = ("yfinance", df)
        except Exception as exc:  # noqa: BLE001
            print(f"yfinance bars failed: {exc}", file=sys.stderr)
            for sym in missing:
                results.setdefault(sym, ("error", f"bars unavailable: {exc}"))
    out = {}
    for sym in symbols:
        kind, payload = results.get(sym, (source, frames.get(sym)))
        try:
            if kind == "error":
                raise RuntimeError(payload)
            out[sym] = technicals(sym, frames.get(sym), kind)
        except Exception as exc:  # noqa: BLE001
            out[sym] = {"error": str(exc)}
    return out


def cmd_fundamentals(symbols):
    import yfinance as yf

    out = {}
    for i, sym in enumerate(symbols):
        if i:
            time.sleep(random.uniform(0.3, 1.0))  # jitter between per-ticker requests
        try:
            info = with_retry(lambda: yf.Ticker(sym).info) or {}
            out[sym] = {
                "beta":       round(float(info.get("beta") or 0), 4),
                "pe_ratio":   round(float(info.get("trailingPE") or 0), 4),
                "avg_volume": float(info.get("averageVolume") or 0),
                "sector":     info.get("sector") or "",
                "name":       info.get("longName") or info.get("shortName") or "",
                "quote_type": info.get("quoteType") or "",
            }
        except Exception as exc:  # noqa: BLE001
            out[sym] = {"error": str(exc)}
    return out


def main():
    real_stdout = sys.stdout
    sys.stdout = sys.stderr  # stray prints from libraries must not corrupt the JSON
    try:
        if len(sys.argv) < 3 or sys.argv[1] not in ("bars", "fundamentals"):
            raise ValueError("usage: calculate.py bars|fundamentals SYMBOL [SYMBOL ...]")
        symbols = []
        for a in sys.argv[2:]:
            a = a.upper()
            if a not in symbols:
                symbols.append(a)
        result = cmd_bars(symbols) if sys.argv[1] == "bars" else cmd_fundamentals(symbols)
    except BaseException as exc:  # noqa: BLE001
        result = {"error": f"{type(exc).__name__}: {exc}"}
    sys.stdout = real_stdout
    sys.stdout.write(json.dumps(result))
    sys.stdout.flush()
    sys.exit(0)


if __name__ == "__main__":
    main()
