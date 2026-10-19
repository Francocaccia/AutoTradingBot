import os
import random
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))

import attribution as at


class DecomposeTest(unittest.TestCase):
    def test_parts_add_up_to_the_gap_exactly(self):
        rnd = random.Random(7)
        syms = ["AAPL", "NVDA", "SPY", "QQQ", "AGG", "SHY", "SGOV"]
        for _ in range(200):
            held = rnd.sample(syms, rnd.randint(0, 5))
            weights = [rnd.uniform(0, 0.15) for _ in held]
            positions = [(s, w, rnd.uniform(-0.05, 0.05)) for s, w in zip(held, weights)]
            w_cash = 1 - sum(weights)
            r_spy, r_agg, r_bil = rnd.uniform(-0.03, 0.03), rnd.uniform(-0.01, 0.01), rnd.uniform(0, 0.0005)
            r_bot = rnd.uniform(-0.04, 0.04)
            d = at.decompose_day(r_bot, r_spy, r_agg, r_bil, positions, w_cash)
            total = d["alloc"] + d["cash_zero"] + d["selection"] + d["core"] + d["trading"]
            self.assertAlmostEqual(total, d["gap"], places=12)

    def test_all_cash_book_lags_a_rising_market_through_allocation_only(self):
        d = at.decompose_day(0.0, 0.01, 0.001, 0.0002, [], 1.0)
        self.assertAlmostEqual(d["gap"], -0.01)
        self.assertAlmostEqual(d["alloc"] + d["cash_zero"], -0.01)  # all of it is cash drag
        self.assertAlmostEqual(d["selection"], 0.0)
        self.assertAlmostEqual(d["core"], 0.0)
        self.assertAlmostEqual(d["trading"], 0.0)

    def test_stock_selection_is_return_relative_to_spy(self):
        # 10% in one stock that beats SPY by 3 points; the rest equities-free cash.
        d = at.decompose_day(0.0, 0.01, 0.0, 0.0, [("AAPL", 0.10, 0.04)], 0.90)
        self.assertAlmostEqual(d["selection"], 0.10 * 0.03)
        self.assertAlmostEqual(d["core"], 0.0)

    def test_core_etfs_are_measured_against_their_own_sleeve_benchmark(self):
        positions = [("SPY", 0.10, 0.02), ("AGG", 0.20, 0.003), ("SGOV", 0.10, 0.0004)]
        d = at.decompose_day(0.0, 0.01, 0.002, 0.0003, positions, 0.60)
        self.assertAlmostEqual(d["core"], 0.10 * (0.02 - 0.01) + 0.20 * (0.003 - 0.002) + 0.10 * (0.0004 - 0.0003))
        self.assertAlmostEqual(d["selection"], 0.0)

    def test_trading_effects_are_what_the_closing_holdings_do_not_explain(self):
        # Holdings earned 0.2% of equity; the account earned 0.1%: 0.1 points were lost trading.
        d = at.decompose_day(0.001, 0.0, 0.0, 0.0, [("AAPL", 0.10, 0.02)], 0.90)
        self.assertAlmostEqual(d["trading"], 0.001 - 0.002)

    def test_categories(self):
        self.assertEqual(at.category("SPY"), "broad_etf")
        self.assertEqual(at.category("QQQ"), "broad_etf")
        self.assertEqual(at.category("AGG"), "bond_etf")
        self.assertEqual(at.category("SGOV"), "tbill_etf")
        self.assertEqual(at.category("NVDA"), "stock")


class ReportTest(unittest.TestCase):
    def test_summary_sums_the_days_and_plain_language_names_the_biggest_part(self):
        days = [at.decompose_day(0.0, 0.01, 0.0, 0.0, [], 1.0), at.decompose_day(-0.002, 0.0, 0.0, 0.0, [("AAPL", 0.1, -0.02)], 0.9)]
        tot = at.summarize(days)
        self.assertAlmostEqual(tot["a"] + tot["selection"] + tot["core"] + tot["trading"], tot["gap"], places=9)
        text = at.plain_language(tot, -0.2, 1.0, -0.1, fees_pp=0.001, slippage_pp=0.002)
        self.assertIn("allocation", text.splitlines()[1])
        self.assertIn("own equity / bond / cash weights", text)
        self.assertIn("few trading days", text)

    def test_the_default_text_never_contains_a_symbol(self):
        tot = at.summarize([at.decompose_day(0.0, 0.01, 0.0, 0.0, [("NVDA", 0.1, 0.03)], 0.9)])
        text = at.plain_language(tot, 0.1, 1.0, 0.5)
        for sym in ("NVDA", "AAPL", "CCC", "MSFT"):
            self.assertNotIn(sym, text)

    def test_compounding(self):
        self.assertAlmostEqual(at.compounded([0.1, 0.1]), 21.0)
        self.assertEqual(at.compounded([]), 0.0)

    def test_tiny_gap_is_not_over_explained(self):
        tot = at.summarize([at.decompose_day(0.0001, 0.0001, 0, 0, [], 1.0)])
        self.assertIn("too small", at.plain_language(tot, 0.01, 0.01, 0.0))


if __name__ == "__main__":
    unittest.main()
