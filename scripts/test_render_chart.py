import json
import os
import re
import sys
import tempfile
import unittest
import xml.etree.ElementTree as ET

sys.path.insert(0, os.path.dirname(__file__))

import render_chart

HERE = os.path.dirname(__file__)


def load(name):
    with open(os.path.join(HERE, "sample", name), encoding="utf-8") as f:
        return json.load(f)


class RenderChartTest(unittest.TestCase):
    def test_placeholder_until_enough_trading_days(self):
        history = load("history.json")
        for n in (0, 1, 9):
            files = render_chart.render(history[:n], load("stats.json"), 10)
            for name in ("performance.svg", "allocation.svg"):
                self.assertIn("Tracking since 2026-10-13, chart appears after 10 trading days", files[name])
                self.assertNotIn("<polyline", files[name])
        files = render_chart.render(history[:10], load("stats.json"), 10)
        self.assertIn("<polyline", files["performance.svg"])

    def test_placeholder_uses_the_configured_minimum_and_start(self):
        stats = dict(load("stats.json"), start_date="2027-01-04")
        files = render_chart.render([], stats, 20)
        self.assertIn("Tracking since 2027-01-04, chart appears after 20 trading days", files["performance.svg"])

    def test_svgs_are_well_formed_xml(self):
        for svg in list(render_chart.render(load("history.json"), load("stats.json"), 10).values()) + list(
            render_chart.render([], {}, 10).values()
        ):
            ET.fromstring(svg)

    def test_performance_chart_has_all_series_and_a_legend(self):
        svg = render_chart.render(load("history.json"), load("stats.json"), 10)["performance.svg"]
        self.assertEqual(svg.count("<polyline"), 4)
        for label in ("Bot ", "SPY ", "QQQ ", "60/40 SPY/AGG "):
            self.assertIn(label, svg)
        self.assertIn("indexed to 100", svg)

    def test_a_missing_benchmark_is_left_out_not_invented(self):
        history = load("history.json")
        for row in history:
            row["qqq"] = None
        svg = render_chart.render(history, load("stats.json"), 10)["performance.svg"]
        self.assertEqual(svg.count("<polyline"), 3)
        self.assertNotIn(">QQQ ", svg)  # no legend entry

    def test_allocation_chart_shows_bands_and_status(self):
        stats = load("stats.json")
        stats["allocation_pct"] = {"equities": 28.2, "bonds": 0.0, "cash": 71.8}
        svg = render_chart.render(load("history.json"), stats, 10)["allocation.svg"]
        self.assertIn("28.2%", svg)
        self.assertIn("below minimum", svg)
        self.assertIn("above maximum", svg)
        self.assertIn("band 40-75%", svg)
        # Falls back to the default bands when the endpoint does not publish them.
        del stats["policy_bands_pct"]
        self.assertIn("band 15-40%", render_chart.render(load("history.json"), stats, 10)["allocation.svg"])

    def test_no_dollar_amounts_or_private_data_are_drawn(self):
        for svg in render_chart.render(load("history.json"), load("stats.json"), 10).values():
            self.assertNotIn("$", svg)
            text = " ".join(re.findall(r">([^<]+)<", svg))
            self.assertNotRegex(text, r"\b\d{5,}\b")  # nothing that looks like a balance

    def test_output_is_deterministic_so_unchanged_data_makes_no_commit(self):
        a = render_chart.render(load("history.json"), load("stats.json"), 10)
        b = render_chart.render(load("history.json"), load("stats.json"), 10)
        self.assertEqual(a, b)
        with tempfile.TemporaryDirectory() as d:
            self.assertTrue(render_chart.write_if_changed(os.path.join(d, "x.svg"), a["performance.svg"]))
            self.assertFalse(render_chart.write_if_changed(os.path.join(d, "x.svg"), a["performance.svg"]))

    def test_a_failed_fetch_keeps_the_last_good_charts(self):
        with tempfile.TemporaryDirectory() as d:
            good = os.path.join(d, "performance.svg")
            with open(good, "w", encoding="utf-8") as f:
                f.write("<svg>last good</svg>")
            with open(os.path.join(d, "allocation.svg"), "w", encoding="utf-8") as f:
                f.write("<svg>last good</svg>")
            orig = render_chart.fetch_json
            render_chart.fetch_json = lambda url, **kw: (_ for _ in ()).throw(RuntimeError("down"))
            try:
                self.assertEqual(render_chart.main(["--url", "http://x.invalid", "--out", d]), 0)
            finally:
                render_chart.fetch_json = orig
            with open(good, encoding="utf-8") as f:
                self.assertEqual(f.read(), "<svg>last good</svg>")

    def test_a_failed_first_fetch_writes_the_placeholders(self):
        with tempfile.TemporaryDirectory() as d:
            orig = render_chart.fetch_json
            render_chart.fetch_json = lambda url, **kw: (_ for _ in ()).throw(RuntimeError("down"))
            try:
                render_chart.main(["--url", "http://x.invalid", "--out", d])
            finally:
                render_chart.fetch_json = orig
            with open(os.path.join(d, "performance.svg"), encoding="utf-8") as f:
                self.assertIn("chart appears after 10 trading days", f.read())

    def test_main_reads_sample_files(self):
        with tempfile.TemporaryDirectory() as d:
            rc = render_chart.main(["--history", os.path.join(HERE, "sample", "history.json"),
                                    "--stats", os.path.join(HERE, "sample", "stats.json"), "--out", d])
            self.assertEqual(rc, 0)
            self.assertEqual(sorted(os.listdir(d)), ["allocation.svg", "performance.svg"])


class IllustrativeLabelTest(unittest.TestCase):
    def test_charts_from_sample_files_always_carry_the_label(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertEqual(render_chart.main(["--history", os.path.join(HERE, "sample", "history.json"),
                                                "--stats", os.path.join(HERE, "sample", "stats.json"), "--out", d]), 0)
            for name in ("performance.svg", "allocation.svg"):
                with open(os.path.join(d, name), encoding="utf-8") as f:
                    svg = f.read()
                ET.fromstring(svg)
                self.assertIn("ILLUSTRATIVE DATA", svg)
                self.assertIn("NOT REAL RESULTS", svg)
                self.assertIn("ILLUSTRATIVE DATA", svg.split("</title>")[0])  # also in the accessible title

    def test_illustrative_charts_can_never_be_written_into_docs(self):
        with tempfile.TemporaryDirectory() as d:
            docs = os.path.join(d, "docs")
            rc = render_chart.main(["--history", os.path.join(HERE, "sample", "history.json"),
                                    "--stats", os.path.join(HERE, "sample", "stats.json"), "--out", docs])
            self.assertEqual(rc, 2)
            self.assertFalse(os.path.exists(docs))

    def test_sample_files_cannot_be_declared_real(self):
        with tempfile.TemporaryDirectory() as d:
            rc = render_chart.main(["--history", os.path.join(HERE, "sample", "history.json"),
                                    "--stats", os.path.join(HERE, "sample", "stats.json"), "--real", "--out", d])
            self.assertEqual(rc, 2)

    def test_real_data_is_not_labelled(self):
        svg = render_chart.render(load("history.json"), load("stats.json"), 10)["performance.svg"]
        self.assertNotIn("ILLUSTRATIVE", svg)
        # A real file saved from the endpoints can be rendered with --real, outside the sample directory.
        with tempfile.TemporaryDirectory() as d:
            h, s = os.path.join(d, "history.json"), os.path.join(d, "stats.json")
            json.dump(load("history.json"), open(h, "w"))
            json.dump(load("stats.json"), open(s, "w"))
            out = os.path.join(d, "out")
            self.assertEqual(render_chart.main(["--history", h, "--stats", s, "--real", "--out", out]), 0)
            with open(os.path.join(out, "performance.svg"), encoding="utf-8") as f:
                self.assertNotIn("ILLUSTRATIVE", f.read())

    def test_the_committed_charts_are_placeholders(self):
        docs = os.path.join(HERE, "..", "docs")
        for name in ("performance.svg", "allocation.svg"):
            path = os.path.join(docs, name)
            if not os.path.exists(path):
                continue
            with open(path, encoding="utf-8") as f:
                svg = f.read()
            self.assertIn("chart appears after", svg, name + " must stay a placeholder until real data exists")
            self.assertNotIn("<polyline", svg)
            self.assertNotIn("<rect x=\"", svg.replace('<rect width="760"', ""), name)


if __name__ == "__main__":
    unittest.main()
