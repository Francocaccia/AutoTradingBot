#!/usr/bin/env python3
"""Render the README charts from the bot's public endpoints.

Reads  <BOT_URL>/stats/history  and  <BOT_URL>/stats  and writes

    docs/performance.svg   the bot against SPY, QQQ and a 60/40 SPY/AGG mix,
                           every line indexed to 100 at the tracking baseline
    docs/allocation.svg    the current sleeve split against the policy bands

Python standard library only, no paid service. Only percentages and indexed
values are ever drawn: the endpoints carry no dollar amounts, share counts,
symbols held or order ids, and this script adds none.

Until MIN_CHART_DAYS trading days exist both files are a neutral placeholder.
Data read from files (--history / --stats) is treated as ILLUSTRATIVE unless --real
is given: such charts carry a visible "ILLUSTRATIVE DATA" label and can never be
written to the committed docs/ directory.
If the fetch fails the existing files are left untouched, so the last good chart
stays on the README.

Usage:
    python scripts/render_chart.py                      # fetch from BOT_URL
    python scripts/render_chart.py --history h.json --stats s.json --out /tmp/out   # labelled ILLUSTRATIVE
"""
import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from xml.sax.saxutils import escape

DEFAULT_BOT_URL = "https://autotradingbot-kjr0.onrender.com"
DEFAULT_START = "2026-10-13"
DEFAULT_MIN_DAYS = 10
DEFAULT_BANDS = {"equities": (40, 75), "bonds": (15, 40), "cash": (5, 30)}

W, H = 760, 380
FONT = "system-ui, -apple-system, 'Segoe UI', Helvetica, Arial, sans-serif"

# Theme tokens. The SVG switches with the viewer's colour scheme.
STYLE = f"""
  :root {{ --bg:#ffffff; --fg:#1f2328; --muted:#656d76; --grid:#d8dee4; --band:#dbe9f6; --bar:#1f6feb;
          --bot:#1f6feb; --spy:#d1730a; --qqq:#8250df; --mix:#1a7f37; }}
  @media (prefers-color-scheme: dark) {{
    :root {{ --bg:#0d1117; --fg:#e6edf3; --muted:#8b949e; --grid:#30363d; --band:#1c2d44; --bar:#58a6ff;
            --bot:#58a6ff; --spy:#f0883e; --qqq:#bc8cff; --mix:#3fb950; }}
  }}
  text {{ font-family:{FONT}; fill:var(--fg); }}
  .muted {{ fill:var(--muted); }}
"""

SERIES = [  # key in the history rows, label, css variable, dash, width
    ("c", "Bot", "var(--bot)", "", 2.8),
    ("spy", "SPY", "var(--spy)", "", 1.8),
    ("qqq", "QQQ", "var(--qqq)", "", 1.8),
    ("mix_60_40", "60/40 SPY/AGG", "var(--mix)", "6 3", 1.8),
]


ILLUSTRATIVE = "ILLUSTRATIVE DATA"


def illustrative_banner(width):
    """A label that cannot be missed or cropped away: a filled strip across the top."""
    return (
        f'<rect x="0" y="0" width="{width}" height="22" rx="8" fill="#b42318"/>\n'
        f'<rect x="0" y="12" width="{width}" height="10" fill="#b42318"/>\n'
        f'<text x="{width / 2}" y="16" text-anchor="middle" font-size="12" font-weight="700" '
        f'style="fill:#ffffff" letter-spacing="2">{ILLUSTRATIVE} - NOT REAL RESULTS</text>\n'
    )


def svg_open(title, desc, width=W, height=H):
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {width} {height}" width="{width}" height="{height}" '
        f'role="img" aria-labelledby="t d">\n<title id="t">{escape(title)}</title>\n<desc id="d">{escape(desc)}</desc>\n'
        f"<style>{STYLE}</style>\n"
        f'<rect width="{width}" height="{height}" rx="8" fill="var(--bg)"/>\n'
    )


def placeholder_svg(kind, start, min_days):
    msg = f"Tracking since {start}, chart appears after {min_days} trading days"
    title = "Performance chart" if kind == "performance" else "Allocation chart"
    out = svg_open(title, msg)
    out += (
        f'<text x="{W / 2}" y="{H / 2 - 6}" text-anchor="middle" font-size="20" font-weight="600">{escape(title)}</text>\n'
        f'<text x="{W / 2}" y="{H / 2 + 22}" text-anchor="middle" font-size="15" class="muted">{escape(msg)}</text>\n'
        f'<text x="{W / 2}" y="{H - 18}" text-anchor="middle" font-size="12" class="muted">Paper trading only. Not investment advice.</text>\n'
        "</svg>\n"
    )
    return out


def nice_range(lo, hi):
    """Pad a value range and pick a tick step that gives 4-6 ticks."""
    if hi - lo < 1.0:
        mid = (hi + lo) / 2
        lo, hi = mid - 0.5, mid + 0.5
    span = hi - lo
    raw = span / 5
    for step in (0.1, 0.2, 0.25, 0.5, 1, 2, 2.5, 5, 10, 20, 25, 50):
        if raw <= step:
            break
    lo = step * int((lo - span * 0.05) // step)
    hi = step * (int((hi + span * 0.05) // step) + 1)
    ticks = []
    t = lo
    while t <= hi + 1e-9:
        ticks.append(round(t, 6))
        t += step
    return lo, hi, ticks


def fmt_tick(v):
    return f"{v:.0f}" if abs(v - round(v)) < 1e-9 else f"{v:.1f}"


def performance_svg(history, start, illustrative=False):
    days = len(history)
    left, right, top, bottom = 58, 24, 62, 54
    pw, ph = W - left - right, H - top - bottom

    # One point per tracked day, preceded by the baseline (100 for everything).
    xs_dates = ["start"] + [row["date"] for row in history]
    series = {}
    for key, label, color, dash, width in SERIES:
        vals = [100.0]
        ok = True
        for row in history:
            v = row.get(key)
            if v is None:
                ok = False
                break
            vals.append(float(v))
        if ok:
            series[key] = vals
    allv = [v for vals in series.values() for v in vals]
    for row in history:  # include the daily high / low of the bot
        allv += [float(row["h"]), float(row["l"])]
    lo, hi, ticks = nice_range(min(allv), max(allv))

    def x(i):
        return left + pw * i / max(len(xs_dates) - 1, 1)

    def y(v):
        return top + ph * (hi - v) / (hi - lo)

    last = history[-1]
    ret = float(last["c"]) - 100.0
    desc = (f"Portfolio value indexed to 100 over {days} trading days, against SPY, QQQ and a 60/40 SPY/AGG mix. "
            f"Latest bot index {float(last['c']):.2f}.")
    out = svg_open("Performance, indexed to 100" + (" - " + ILLUSTRATIVE if illustrative else ""), (ILLUSTRATIVE + ". " if illustrative else "") + desc)
    if illustrative:
        out += illustrative_banner(W)
    out += f'<text x="{left}" y="28" font-size="18" font-weight="600">Performance, indexed to 100</text>\n'
    out += (f'<text x="{left}" y="46" font-size="12" class="muted">Since {escape(start)} (baseline = last close before); '
            f"{days} trading days; paper trading</text>\n")

    for t in ticks:
        out += f'<line x1="{left}" x2="{W - right}" y1="{y(t):.1f}" y2="{y(t):.1f}" stroke="var(--grid)" stroke-width="{1.6 if abs(t - 100) < 1e-9 else 0.8}"/>\n'
        out += f'<text x="{left - 8}" y="{y(t) + 4:.1f}" text-anchor="end" font-size="11" class="muted">{fmt_tick(t)}</text>\n'

    # Daily range of the bot (high-low) as thin vertical ticks behind the lines.
    for i, row in enumerate(history, start=1):
        out += (f'<line x1="{x(i):.1f}" x2="{x(i):.1f}" y1="{y(float(row["h"])):.1f}" y2="{y(float(row["l"])):.1f}" '
                f'stroke="var(--bot)" stroke-opacity="0.25" stroke-width="3"/>\n')

    n_labels = min(6, len(xs_dates))
    for k in range(n_labels):
        i = round(k * (len(xs_dates) - 1) / max(n_labels - 1, 1))
        label = "start" if i == 0 else xs_dates[i][5:]
        out += f'<text x="{x(i):.1f}" y="{H - bottom + 18}" text-anchor="middle" font-size="11" class="muted">{escape(label)}</text>\n'

    for key, label, color, dash, width in SERIES:
        vals = series.get(key)
        if not vals:
            continue
        pts = " ".join(f"{x(i):.1f},{y(v):.1f}" for i, v in enumerate(vals))
        da = f' stroke-dasharray="{dash}"' if dash else ""
        out += (f'<polyline points="{pts}" fill="none" stroke="{color}" stroke-width="{width}" '
                f'stroke-linejoin="round" stroke-linecap="round"{da}/>\n')

    # Legend with the latest value of each series.
    lx = left
    ly = H - 14
    for key, label, color, dash, width in SERIES:
        vals = series.get(key)
        if not vals:
            continue
        da = f' stroke-dasharray="{dash}"' if dash else ""
        text = f"{label} {vals[-1]:.1f}"
        out += f'<line x1="{lx}" x2="{lx + 22}" y1="{ly - 4}" y2="{ly - 4}" stroke="{color}" stroke-width="{width}"{da}/>\n'
        out += f'<text x="{lx + 28}" y="{ly}" font-size="12">{escape(text)}</text>\n'
        lx += 28 + 7.2 * len(text) + 22
    out += (f'<text x="{W - right}" y="28" text-anchor="end" font-size="14" font-weight="600">Bot {ret:+.2f}%</text>\n')
    out += "</svg>\n"
    return out


def allocation_svg(stats, start, illustrative=False):
    alloc = stats.get("allocation_pct") or {}
    bands = stats.get("policy_bands_pct") or {}
    rows = []
    for key, label in (("equities", "Equities"), ("bonds", "Bonds"), ("cash", "Cash and T-bills")):
        lo, hi = DEFAULT_BANDS[key]
        b = bands.get(key) or {}
        lo, hi = float(b.get("min", lo)), float(b.get("max", hi))
        val = alloc.get(key)
        rows.append((label, None if val is None else float(val), lo, hi))

    left, right, top = 150, 40, 84
    pw = W - left - right
    row_h = 66

    def x(p):
        return left + pw * p / 100.0

    desc = "Current sleeve allocation, in percent of portfolio value, against the policy band of each sleeve."
    out = svg_open("Allocation against policy bands" + (" - " + ILLUSTRATIVE if illustrative else ""),
                   (ILLUSTRATIVE + ". " if illustrative else "") + desc, height=top + row_h * 3 + 56)
    if illustrative:
        out += illustrative_banner(W)
    out += '<text x="28" y="30" font-size="18" font-weight="600">Allocation against policy bands</text>\n'
    out += '<text x="28" y="49" font-size="12" class="muted">Share of portfolio value. Shaded range = allowed band; bar = current.</text>\n'
    for p in (0, 25, 50, 75, 100):
        out += f'<line x1="{x(p):.1f}" x2="{x(p):.1f}" y1="{top - 10}" y2="{top + row_h * 3 - 14}" stroke="var(--grid)" stroke-width="0.8"/>\n'
        out += f'<text x="{x(p):.1f}" y="{top - 16}" text-anchor="middle" font-size="11" class="muted">{p}%</text>\n'
    for i, (label, val, lo, hi) in enumerate(rows):
        yy = top + i * row_h
        out += f'<text x="28" y="{yy + 22}" font-size="14">{escape(label)}</text>\n'
        out += f'<rect x="{x(lo):.1f}" y="{yy}" width="{x(hi) - x(lo):.1f}" height="34" rx="4" fill="var(--band)"/>\n'
        out += f'<text x="{x(lo) + 6:.1f}" y="{yy + 48}" font-size="11" class="muted">band {lo:.0f}-{hi:.0f}%</text>\n'
        if val is None:
            out += f'<text x="{x(0) + 6:.1f}" y="{yy + 22}" font-size="13" class="muted">n/a</text>\n'
            continue
        status = "below minimum" if val < lo else "above maximum" if val > hi else "inside band"
        out += f'<rect x="{x(0):.1f}" y="{yy + 8}" width="{max(x(val) - x(0), 1):.1f}" height="18" rx="3" fill="var(--bar)"/>\n'
        tx = min(x(val) + 8, W - right - 90)
        out += f'<text x="{tx:.1f}" y="{yy + 22}" font-size="13" font-weight="600">{val:.1f}%</text>\n'
        out += f'<text x="{W - right}" y="{yy + 48}" text-anchor="end" font-size="11" class="muted">{status}</text>\n'
    out += (f'<text x="28" y="{top + row_h * 3 + 30}" font-size="12" class="muted">Tracking since {escape(start)}. '
            "Paper trading only. Not investment advice.</text>\n")
    out += "</svg>\n"
    return out


def fetch_json(url, attempts=3, timeout=30):
    last = None
    for i in range(attempts):
        try:
            with urllib.request.urlopen(url, timeout=timeout) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
            last = exc
            time.sleep(2 * (i + 1))
    raise RuntimeError(f"{url}: {last}")


def write_if_changed(path, text):
    try:
        with open(path, encoding="utf-8") as f:
            if f.read() == text:
                return False
    except FileNotFoundError:
        pass
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(text)
    return True


def render(history, stats, min_days, default_start=DEFAULT_START, illustrative=False):
    """Return {filename: svg} for the given endpoint payloads. illustrative=True
    stamps every chart that draws data with a visible ILLUSTRATIVE DATA label."""
    start = (stats or {}).get("start_date") or default_start
    if not isinstance(history, list) or len(history) < min_days:
        return {
            "performance.svg": placeholder_svg("performance", start, min_days),
            "allocation.svg": placeholder_svg("allocation", start, min_days),
        }
    return {"performance.svg": performance_svg(history, start, illustrative),
            "allocation.svg": allocation_svg(stats or {}, start, illustrative)}


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", default=os.environ.get("BOT_URL", DEFAULT_BOT_URL))
    ap.add_argument("--history", help="read /stats/history from this file instead of fetching")
    ap.add_argument("--stats", help="read /stats from this file instead of fetching")
    ap.add_argument("--real", action="store_true", help="declare that --history / --stats hold real endpoint data (not a sample)")
    ap.add_argument("--out", default="docs")
    ap.add_argument("--min-days", type=int, default=int(os.environ.get("MIN_CHART_DAYS", DEFAULT_MIN_DAYS)))
    args = ap.parse_args(argv)

    from_files = bool(args.history and args.stats)
    illustrative = from_files and not args.real
    if args.real and any("sample" in (p or "") for p in (args.history, args.stats)):
        print("refusing --real for a file under a sample directory", file=sys.stderr)
        return 2
    if illustrative and os.path.basename(os.path.normpath(args.out)) == "docs":
        print("refusing to write illustrative charts into docs/: the committed charts stay placeholders until real data exists",
              file=sys.stderr)
        return 2

    try:
        if from_files:
            with open(args.history, encoding="utf-8") as f:
                history = json.load(f)
            with open(args.stats, encoding="utf-8") as f:
                stats = json.load(f)
        else:
            base = args.url.rstrip("/")
            history = fetch_json(base + "/stats/history")
            stats = fetch_json(base + "/stats")
    except Exception as exc:  # noqa: BLE001
        print(f"fetch failed ({exc}); keeping the existing charts", file=sys.stderr)
        # A first run with nothing to keep still gets the placeholders.
        missing = [n for n in ("performance.svg", "allocation.svg") if not os.path.exists(os.path.join(args.out, n))]
        if missing:
            files = render([], {}, args.min_days)
            for n in missing:
                write_if_changed(os.path.join(args.out, n), files[n])
        return 0

    changed = []
    for name, svg in render(history, stats, args.min_days, illustrative=illustrative).items():
        if write_if_changed(os.path.join(args.out, name), svg):
            changed.append(name)
    print("updated: " + (", ".join(changed) if changed else "nothing changed"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
