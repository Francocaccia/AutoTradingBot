import sys
import json
import warnings
warnings.filterwarnings("ignore")

import yfinance as yf
import pandas as pd


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


def rsi(close, length=14):
    delta = close.diff()
    gain  = delta.clip(lower=0).ewm(span=length).mean()
    loss  = (-delta.clip(upper=0)).ewm(span=length).mean()
    rs    = gain / loss

    return round(safe((100 - 100 / (1 + rs)).iloc[-1]), 4)


def main():
    if len(sys.argv) < 2:
        print(json.dumps({"error": "no symbol"}))
        sys.exit(1)

    symbol = sys.argv[1].upper()

    try:
        ticker = yf.Ticker(symbol)
        df = ticker.history(period="3mo", interval="1d")

        if df.empty or len(df) < 52:
            print(json.dumps({"error": f"not enough data for {symbol}"}))
            sys.exit(1)

        info  = ticker.info
        price = round(safe(df["Close"].iloc[-1]), 4)
        vol   = int(df["Volume"].iloc[-1])

        print(json.dumps({
            "symbol":   symbol,
            "price":    price,
            "volume":   vol,
            "rsi":      rsi(df["Close"]),
            "beta":     round(float(info.get("beta") or 0), 4),
            "pe_ratio": round(float(info.get("trailingPE") or 0), 4),
            "ichimoku":   ichimoku(df),
            "macd":       macd(df["Close"]),
            "stochastic": stochastic(df),
            "keltner":    keltner(df),
        }))

    except Exception as e:
        print(json.dumps({"error": str(e)}))
        sys.exit(1)


if __name__ == "__main__":
    main()