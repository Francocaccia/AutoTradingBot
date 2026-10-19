#!/usr/bin/env python3
"""Where did the gap versus SPY come from? A local report: stdout only.

Since TRACK_START_DATE, each day's gap (bot return minus SPY return) is split,
exactly and additively, into

  (a) cash drag and allocation effect: holding bonds and cash instead of equities.
      a1 = what the bot's equity / bond / cash weights would have returned on SPY,
      AGG and BIL, minus SPY (the allocation-matched benchmark minus SPY);
      a2 = the cost of actual cash earning nothing where BIL would have earned a
      little;
  (b) single-stock selection: each stock's weight times its return minus SPY's;
  (c) core ETFs: each index, bond or T-bill ETF's weight times its return minus the
      return of the benchmark of its sleeve (SPY, AGG, BIL);
  (d) trading effects: whatever the end-of-day holdings do not explain, namely
      intraday fills versus closing prices (slippage, spread), fees, positions
      opened and closed within a day, and the difference between selling a holding
      intraday (a stop, an exit) and holding it to the close. Fees and an estimate of the slippage on entries
      are shown separately when Alpaca provides them.

Weights are those held at the previous close, rebuilt from today's positions by
undoing later fills. The parts are summed day by day (arithmetic), so they add up
to the sum of the daily gaps; the compounded gap differs a little, and the report
says by how much.

Symbols are never printed unless --detail is given. Nothing here is published; the
public endpoints carry only the allocation-matched index.

    set -a; . ./.env; set +a
    python scripts/attribution.py [--start 2026-10-13] [--detail]
"""
import argparse
import datetime as dt
import sys

# Broad index ETFs the policy fills the equity floor with.
BROAD_ETFS = {"SPY", "QQQ", "VOO", "IVV", "VTI", "DIA", "IWM"}
TBILL_ETFS = {"SGOV", "BIL"}
BOND_ETFS = {"SHY", "IEI", "IEF", "TLT", "TIP", "AGG", "BND", "LQD", "VCSH", "HYG", "EMB"}


def category(symbol):
    """stock, broad_etf, bond_etf or tbill_etf."""
    if symbol in TBILL_ETFS:
        return "tbill_etf"
    if symbol in BOND_ETFS:
        return "bond_etf"
    if symbol in BROAD_ETFS:
        return "broad_etf"
    return "stock"


def decompose_day(r_bot, r_spy, r_agg, r_bil, positions, w_cash):
    """Split one day's gap (r_bot - r_spy). positions: [(symbol, weight, return)] at
    the previous close, weights as fractions of equity; w_cash is the rest of the book
    (actual cash, earning nothing). Returns a dict whose parts add up to the gap."""
    w = {"stock": 0.0, "broad_etf": 0.0, "bond_etf": 0.0, "tbill_etf": 0.0}
    selection = core = pos_total = 0.0
    per_symbol = {}
    for sym, weight, ret in positions:
        cat = category(sym)
        w[cat] += weight
        pos_total += weight * ret
        if cat == "stock":
            part = weight * (ret - r_spy)
            selection += part
        else:
            bench = {"broad_etf": r_spy, "bond_etf": r_agg, "tbill_etf": r_bil}[cat]
            part = weight * (ret - bench)
            core += part
        per_symbol[sym] = per_symbol.get(sym, 0.0) + part
    w_bond = w["bond_etf"]
    w_cashsleeve = w_cash + w["tbill_etf"]
    a1 = w_bond * (r_agg - r_spy) + w_cashsleeve * (r_bil - r_spy)
    a2 = -w_cash * r_bil
    resid = r_bot - pos_total
    gap = r_bot - r_spy
    return {"gap": gap, "alloc": a1, "cash_zero": a2, "selection": selection, "core": core, "trading": resid,
            "w_equity": w["stock"] + w["broad_etf"], "w_bond": w_bond, "w_cash": w_cashsleeve,
            "per_symbol": per_symbol}


def summarize(days):
    """Sum the daily parts (percentage points) and chain the returns."""
    keys = ("gap", "alloc", "cash_zero", "selection", "core", "trading")
    tot = {k: sum(d[k] for d in days) * 100 for k in keys}
    tot["a"] = tot["alloc"] + tot["cash_zero"]
    return tot


def compounded(returns):
    v = 1.0
    for r in returns:
        v *= 1 + r
    return (v - 1) * 100


def plain_language(tot, bot_ret, spy_ret, alloc_matched_ret, fees_pp=None, slippage_pp=None):
    """A short, honest reading of the numbers (all in percentage points)."""
    gap = tot["gap"]
    parts = [
        ("the allocation (bonds and cash instead of equities, and cash earning nothing)", tot["a"]),
        ("single-stock selection", tot["selection"]),
        ("the core ETFs", tot["core"]),
        ("trading effects (fills versus closes, fees)", tot["trading"]),
    ]
    lines = []
    lines.append(f"The bot returned {bot_ret:+.2f}% and SPY {spy_ret:+.2f}%, a gap of {bot_ret - spy_ret:+.2f} points "
                 f"({gap:+.2f} when the daily gaps are simply added).")
    if abs(gap) < 0.05:
        lines.append("The gap is too small to attribute to anything.")
        return "\n".join(lines)
    biggest = max(parts, key=lambda p: abs(p[1]))
    direction = "behind" if gap < 0 else "ahead of"
    lines.append(f"Being {direction} SPY is explained mostly by {biggest[0]}: {biggest[1]:+.2f} points of the {gap:+.2f}.")
    for name, value in parts:
        if name != biggest[0]:
            lines.append(f"  - {name}: {value:+.2f}")
    if alloc_matched_ret is not None:
        lines.append(f"With the bot's own equity / bond / cash weights applied to SPY, AGG and BIL the result would have been "
                     f"{alloc_matched_ret:+.2f}%: that, not SPY, is the fair yardstick for a portfolio that must hold bonds and cash. "
                     f"Against it the bot is {bot_ret - alloc_matched_ret:+.2f} points, which is selection, core ETFs and trading together.")
    if fees_pp is not None:
        lines.append(f"Fees charged by the broker were about {fees_pp:.3f} points of equity.")
    if slippage_pp is not None:
        word = "cost" if slippage_pp >= 0 else "gained"
        lines.append(f"Estimated slippage on entries (fill versus the quote midpoint) {word} about {abs(slippage_pp):.3f} points of equity.")
    lines.append("A few trading days say very little: treat the split as a description of what happened, not as evidence about skill.")
    return "\n".join(lines)


# --------------------------------------------------------------------------
# data
# --------------------------------------------------------------------------

def load(args):
    import _alpaca as A

    start = args.start
    days_back = (dt.date.today() - dt.date.fromisoformat(start)).days + 12
    hist = A.trading_get("/v2/account/portfolio/history", {"timeframe": "1D", "period": f"{days_back}D"})
    equity = {}
    for ts, eq in zip(hist["timestamp"], hist["equity"]):
        if eq:
            equity[dt.datetime.fromtimestamp(ts, dt.timezone.utc).astimezone(A.NY).strftime("%Y-%m-%d")] = eq
    dates = sorted(equity)
    base = max((d for d in dates if d < start), default=None) or min(d for d in dates if d >= start)
    tracked = [d for d in dates if d > base and d >= start]
    seq = [base] + tracked

    now = {p["symbol"]: float(p["qty"]) for p in A.trading_get("/v2/positions")}
    fills, token = [], None
    while True:
        params = {"after": (dt.date.fromisoformat(base) - dt.timedelta(days=1)).isoformat() + "T00:00:00Z", "direction": "asc", "page_size": 100}
        if token:
            params["page_token"] = token
        page = A.trading_get("/v2/account/activities/FILL", params)
        fills += page
        if len(page) < 100:
            break
        token = page[-1]["id"]
    activities = A.trading_get("/v2/account/activities", {"activity_types": "FEE", "after": base + "T00:00:00Z", "page_size": 100})

    symbols = set(now) | {f["symbol"] for f in fills}
    closes = {}
    for sym in symbols | {"SPY", "AGG", "BIL"}:
        bars = A.daily_bars(sym, (dt.date.fromisoformat(base) - dt.timedelta(days=10)).isoformat())
        closes[sym] = {b["date"]: b["c"] for b in bars}

    def qty_at(d):
        q = dict(now)
        for f in fills:
            fd = A.et_date(f["transaction_time"])
            if fd > d:
                signed = float(f["qty"]) * (1 if f["side"] == "buy" else -1)
                q[f["symbol"]] = q.get(f["symbol"], 0.0) - signed
        return q

    def close_on(sym, d):
        c = closes.get(sym, {})
        usable = [x for x in c if x <= d]
        return c[max(usable)] if usable else None

    out = []
    for prev, cur in zip(seq, seq[1:]):
        q = qty_at(prev)
        positions, invested = [], 0.0
        for sym, n in q.items():
            if abs(n) < 1e-9 or n < 0:
                continue
            p0, p1 = close_on(sym, prev), close_on(sym, cur)
            if not p0 or not p1:
                continue
            value = n * p0
            invested += value
            positions.append((sym, value / equity[prev], p1 / p0 - 1))
        w_cash = 1.0 - invested / equity[prev]
        r = lambda s: close_on(s, cur) / close_on(s, prev) - 1  # noqa: E731
        out.append((cur, equity[cur] / equity[prev] - 1, r("SPY"), r("AGG"), r("BIL"), positions, w_cash))

    fees = 0.0
    if isinstance(activities, list):
        fees = sum(-float(a["net_amount"]) for a in activities if a.get("activity_type") == "FEE" and a["date"] > base)
    slippage = entry_slippage(A, fills, equity, start)
    return seq, out, fees / equity[base] * 100, slippage


def entry_slippage(A, fills, equity, start):
    """Estimated slippage on buys since start, points of equity: (fill - quote mid) x quantity."""
    total, got = 0.0, 0
    for f in fills:
        d = A.et_date(f["transaction_time"])
        if d < start or f["side"] != "buy":
            continue
        try:
            mid = A.quote_mid(f["symbol"], f["transaction_time"])
        except Exception:  # noqa: BLE001
            mid = None
        if not mid:
            continue
        total += (float(f["price"]) - mid) * float(f["qty"]) / equity.get(d, 1) * 100
        got += 1
    return total if got else None


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--start", default="2026-10-13")
    ap.add_argument("--detail", action="store_true", help="also list the contribution of each holding (prints symbols)")
    args = ap.parse_args(argv)

    seq, rows, fees_pp, slippage_pp = load(args)
    days = []
    per_symbol = {}
    matched = [1.0]
    for cur, r_bot, r_spy, r_agg, r_bil, positions, w_cash in rows:
        d = decompose_day(r_bot, r_spy, r_agg, r_bil, positions, w_cash)
        days.append(d)
        for sym, v in d["per_symbol"].items():
            per_symbol[sym] = per_symbol.get(sym, 0.0) + v * 100
        matched.append(matched[-1] * (1 + d["w_equity"] * r_spy + d["w_bond"] * r_agg + d["w_cash"] * r_bil))
    if not days:
        print("no tracked trading day yet")
        return 0
    tot = summarize(days)
    bot_ret = compounded([r[1] for r in rows])
    spy_ret = compounded([r[2] for r in rows])
    matched_ret = (matched[-1] - 1) * 100

    print(f"Attribution of the gap versus SPY, {seq[1]} to {seq[-1]} ({len(days)} trading days), percentage points")
    print(f"  bot {bot_ret:+.2f}%   SPY {spy_ret:+.2f}%   allocation-matched benchmark {matched_ret:+.2f}%")
    print(f"  sum of the daily gaps            {tot['gap']:+.2f}   (compounded gap {bot_ret - spy_ret:+.2f}; the difference is compounding)")
    print(f"  (a) cash drag + allocation       {tot['a']:+.2f}   (allocation {tot['alloc']:+.2f}, cash earning nothing {tot['cash_zero']:+.2f})")
    print(f"  (b) single-stock selection       {tot['selection']:+.2f}")
    print(f"  (c) core ETFs                    {tot['core']:+.2f}")
    print(f"  (d) trading effects              {tot['trading']:+.2f}")
    print(f"      of which broker fees         {-fees_pp:+.3f}")
    if slippage_pp is None:
        print("      estimated entry slippage     not available (no quotes)")
    else:
        print(f"      estimated entry slippage     {-slippage_pp:+.3f}   (fill versus quote midpoint)")
    check = tot["a"] + tot["selection"] + tot["core"] + tot["trading"]
    print(f"  parts add up to                  {check:+.2f}")
    if args.detail:
        print("\nBy holding (local only, contains symbols), points:")
        for sym, v in sorted(per_symbol.items(), key=lambda kv: kv[1]):
            print(f"  {sym:<6} {category(sym):<10} {v:+.2f}")
    print("\nIn plain language:")
    print(plain_language(tot, bot_ret, spy_ret, matched_ret, fees_pp, slippage_pp))
    return 0


if __name__ == "__main__":
    sys.exit(main())
