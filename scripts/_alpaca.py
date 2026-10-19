"""Small Alpaca helpers shared by the report scripts (standard library only).

The credentials are read from the environment (ALPACA_API_KEY, ALPACA_API_SECRET,
ALPACA_BASE_URL), never printed. These scripts are local reports: they read your
paper account and print to stdout; nothing is written or sent anywhere.
"""
import datetime as dt
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from zoneinfo import ZoneInfo

NY = ZoneInfo("America/New_York")
DATA = "https://data.alpaca.markets"


def base_url():
    return os.environ.get("ALPACA_BASE_URL", "https://paper-api.alpaca.markets").rstrip("/")


def headers():
    key, secret = os.environ.get("ALPACA_API_KEY", ""), os.environ.get("ALPACA_API_SECRET", "")
    if not key or not secret:
        raise SystemExit("set ALPACA_API_KEY and ALPACA_API_SECRET (for example: set -a; . ./.env; set +a)")
    return {"APCA-API-KEY-ID": key, "APCA-API-SECRET-KEY": secret}


def get(url, params=None, attempts=3):
    if params:
        url += ("&" if "?" in url else "?") + urllib.parse.urlencode(params)
    last = None
    for i in range(attempts):
        try:
            req = urllib.request.Request(url, headers=headers())
            with urllib.request.urlopen(req, timeout=30) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            if exc.code != 429 and exc.code < 500:
                raise
            last = exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            last = exc
        time.sleep(1.5 * (i + 1))
    raise last


def trading_get(path, params=None):
    return get(base_url() + path, params)


def data_get(path, params=None):
    return get(DATA + path, params)


def et_date(ts):
    """ET date (YYYY-MM-DD) of an RFC 3339 timestamp."""
    return parse_ts(ts).astimezone(NY).strftime("%Y-%m-%d")


def parse_ts(ts):
    ts = ts.replace("Z", "+00:00")
    if "." in ts:  # Alpaca sends nanoseconds; datetime takes microseconds
        head, rest = ts.split(".", 1)
        frac = ""
        i = 0
        while i < len(rest) and rest[i].isdigit():
            frac += rest[i]
            i += 1
        ts = head + "." + frac[:6].ljust(6, "0") + rest[i:]
    return dt.datetime.fromisoformat(ts)


def daily_bars(symbol, start, adjustment="split"):
    """[{date, o, h, l, c}] IEX daily bars from start (YYYY-MM-DD)."""
    out, token = [], None
    while True:
        params = {"timeframe": "1Day", "adjustment": adjustment, "feed": "iex", "limit": 1000, "start": start}
        if token:
            params["page_token"] = token
        d = data_get(f"/v2/stocks/{symbol}/bars", params)
        for b in d.get("bars") or []:
            out.append({"date": b["t"][:10], "o": b["o"], "h": b["h"], "l": b["l"], "c": b["c"]})
        token = d.get("next_page_token")
        if not token:
            return out


def wilder_atr(bars, length=14):
    """ATR as the worker computes it: exponentially weighted true range, alpha 1/length."""
    atr, prev_close = None, None
    for b in bars:
        tr = b["h"] - b["l"]
        if prev_close is not None:
            tr = max(tr, abs(b["h"] - prev_close), abs(b["l"] - prev_close))
        atr = tr if atr is None else atr + (tr - atr) / length
        prev_close = b["c"]
    return atr


def quote_mid(symbol, ts, window_s=45):
    """Midpoint of the last IEX quote at or before ts (an RFC 3339 time), or None."""
    t = parse_ts(ts)
    start = (t - dt.timedelta(seconds=window_s)).strftime("%Y-%m-%dT%H:%M:%SZ")
    end = (t + dt.timedelta(seconds=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    d = data_get(f"/v2/stocks/{symbol}/quotes", {"start": start, "end": end, "feed": "iex", "limit": 1000})
    quotes = [q for q in d.get("quotes") or [] if q.get("bp", 0) > 0 and q.get("ap", 0) > 0]
    if not quotes:
        return None
    return (quotes[-1]["ap"] + quotes[-1]["bp"]) / 2
