#!/usr/bin/env python3
"""Entry and stop quality report. Prints to stdout only; symbols appear, so keep it local.

For every entry (a filled buy) since --since and every open position it lists:
when the order filled (minutes after the open), ATR% and the day's range % around
it, the stop distance in ATR and in %, the quote spread at the fill, how it was
exited and the P&L. The summary at the end answers four questions: are stops inside
normal daily noise, do entries cluster at the open or the close, were market orders
used on wide spreads, and how did the stopped trades end.

Not available from Alpaca, hence not in the report: the model's conviction and the
catalyst quoted for each entry (they exist only in the bot's logs).

    set -a; . ./.env; set +a
    python scripts/entry_audit.py --since 2026-10-12
"""
import argparse
import datetime as dt
import sys

import _alpaca as A

OPEN_MIN = 9 * 60 + 30
CLOSE_MIN = 16 * 60


def flatten(orders):
    for o in orders:
        yield o
        for leg in o.get("legs") or []:
            yield leg


def f(x, default=0.0):
    try:
        return float(x)
    except (TypeError, ValueError):
        return default


def quote_spread(symbol, fill_ts):
    """Spread (%) of the last IEX quote at or before the fill, and its mid price."""
    t = A.parse_ts(fill_ts)
    start = (t - dt.timedelta(seconds=45)).strftime("%Y-%m-%dT%H:%M:%SZ")
    end = (t + dt.timedelta(seconds=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    try:
        d = A.data_get(f"/v2/stocks/{symbol}/quotes", {"start": start, "end": end, "feed": "iex", "limit": 1000})
    except Exception:  # noqa: BLE001
        return None, None
    quotes = [q for q in d.get("quotes") or [] if q.get("bp", 0) > 0 and q.get("ap", 0) > 0]
    if not quotes:
        return None, None
    q = quotes[-1]
    mid = (q["ap"] + q["bp"]) / 2
    return (q["ap"] - q["bp"]) / mid * 100, mid


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--since", default="2026-10-12")
    ap.add_argument("--track-start", default="2026-10-13", help="entries before this date are marked (legacy)")
    args = ap.parse_args()

    orders = A.trading_get("/v2/orders", {"status": "all", "nested": "true", "direction": "asc", "limit": 500,
                                          "after": args.since + "T00:00:00Z"})
    positions = {p["symbol"]: p for p in A.trading_get("/v2/positions")}
    allo = list(flatten(orders))
    sells = [o for o in allo if o["side"] == "sell" and o["status"] == "filled" and o.get("filled_at")]
    entries = [o for o in orders if o["side"] == "buy" and o["status"] == "filled" and o.get("filled_at")]

    rows = []
    bars_cache = {}
    for o in entries:
        sym = o["symbol"]
        if sym not in bars_cache:
            bars_cache[sym] = A.daily_bars(sym, (dt.date.fromisoformat(args.since) - dt.timedelta(days=120)).isoformat())
        bars = bars_cache[sym]
        t = A.parse_ts(o["filled_at"]).astimezone(A.NY)
        day = t.strftime("%Y-%m-%d")
        prior = [b for b in bars if b["date"] < day]
        atr = A.wilder_atr(prior) if len(prior) >= 15 else None
        today = next((b for b in bars if b["date"] == day), None)
        px = f(o["filled_avg_price"])
        qty = f(o["filled_qty"])
        stop = take = None
        for leg in o.get("legs") or []:
            if leg.get("stop_price"):
                stop = f(leg["stop_price"])
            if leg.get("limit_price"):
                take = f(leg["limit_price"])
        # Exit: filled sells of the symbol after this entry, until the quantity is covered.
        remaining, proceeds, covered, reason = qty, 0.0, 0.0, "open"
        for s in sells:
            if s["symbol"] != sym or A.parse_ts(s["filled_at"]) <= A.parse_ts(o["filled_at"]):
                continue
            q = min(f(s["filled_qty"]), remaining)
            proceeds += q * f(s["filled_avg_price"])
            covered += q
            remaining -= q
            reason = {"stop": "stop", "stop_limit": "stop", "limit": "take-profit"}.get(s["type"], "discretionary sell")
            if remaining <= 1e-9:
                break
        if covered > 0 and remaining > 1e-9:
            reason += " (partly open)"
        if covered > 0:
            pnl = (proceeds / covered - px) / px * 100
        elif sym in positions:
            pnl = (f(positions[sym]["current_price"]) - px) / px * 100
        else:
            pnl = None
        spread, mid = quote_spread(sym, o["filled_at"])
        minutes = t.hour * 60 + t.minute - OPEN_MIN
        rows.append({
            "sym": sym, "day": day, "time": t.strftime("%H:%M"), "after_open": minutes, "before_close": CLOSE_MIN - (t.hour * 60 + t.minute),
            "legacy": day < args.track_start, "bot": o["client_order_id"].startswith("atb-"), "type": o["type"], "klass": o.get("order_class") or "simple",
            "atr_pct": atr / px * 100 if atr else None,
            "range_pct": (today["h"] - today["l"]) / today["c"] * 100 if today else None,
            "stop": stop, "stop_pct": (px - stop) / px * 100 if stop else None,
            "stop_atr": (px - stop) / atr if stop and atr else None,
            "reason": reason, "pnl": pnl, "spread": spread,
            "slip": (px - mid) / mid * 100 if mid else None, "open": sym in positions and remaining > 1e-9,
        })

    def n(v, fmt="{:.2f}"):
        return "-" if v is None else fmt.format(v)

    print(f"{'entry':<7}{'day':<11}{'t(ET)':<6}{'+min':>5}{'-min':>6} {'ATR%':>5}{'range%':>7}{'stop%':>6}{'stopATR':>8}{'spread%':>8}{'slip%':>7}  {'exit':<22}{'P&L%':>7}  notes")
    for r in rows:
        notes = []
        if r["legacy"]:
            notes.append("before tracking")
        if not r["bot"]:
            notes.append("old code")
        if r["type"] != "market":
            notes.append(r["type"])
        print(f"{r['sym']:<7}{r['day']:<11}{r['time']:<6}{r['after_open']:>5}{r['before_close']:>6} {n(r['atr_pct']):>5}{n(r['range_pct']):>7}"
              f"{n(r['stop_pct'], '{:.1f}'):>6}{n(r['stop_atr']):>8}{n(r['spread'], '{:.3f}'):>8}{n(r['slip'], '{:.3f}'):>7}  {r['reason']:<22}{n(r['pnl'], '{:+.1f}'):>7}  {', '.join(notes)}")

    # ---- findings ----
    with_stop = [r for r in rows if r["stop_atr"] is not None]
    tight = [r for r in with_stop if r["stop_atr"] < 1.5]
    below_range = [r for r in with_stop if r["range_pct"] is not None and r["stop_pct"] < r["range_pct"]]
    early = [r for r in rows if r["after_open"] < 15]
    late = [r for r in rows if r["before_close"] <= 10]
    wide = [r for r in rows if r["spread"] is not None and r["spread"] > 0.3]
    stopped = [r for r in rows if r["reason"].startswith("stop")]
    print("\nSummary")
    print(f"  entries listed: {len(rows)} (bot-tagged {sum(r['bot'] for r in rows)}, before tracking {sum(r['legacy'] for r in rows)})")
    print(f"  stops under 1.5 ATR: {len(tight)} of {len(with_stop)}; stop closer than that day's range: {len(below_range)} of {len(with_stop)}")
    if with_stop:
        print(f"  stop distance in ATR: min {min(r['stop_atr'] for r in with_stop):.2f}, max {max(r['stop_atr'] for r in with_stop):.2f}")
    print(f"  entries in the first 15 minutes: {len(early)}; in the last 10 minutes: {len(late)}")
    sp = [r["spread"] for r in rows if r["spread"] is not None]
    if sp:
        print(f"  quote spread at the fill: median {sorted(sp)[len(sp) // 2]:.3f}%, max {max(sp):.3f}%; wider than 0.3%: {len(wide)}; market orders: {sum(r['type'] == 'market' for r in rows)} of {len(rows)}")
    print(f"  stopped out: {len(stopped)}; of those, a gap through the stop (fill worse than the stop level) is visible in the order list, not here")
    print("  not available: conviction and catalyst per entry (kept only in the bot's logs)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
