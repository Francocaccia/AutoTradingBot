import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))

import pandas as pd

import calculate


def frame(volumes):
    return pd.DataFrame({"Volume": volumes})


class RelVolumeTest(unittest.TestCase):
    def test_partial_last_bar_is_excluded(self):
        # 60 completed sessions of 1000, one completed session of 850, then a
        # tiny in-progress bar that must not influence the ratio.
        df = frame([1000] * 60 + [850] + [5])
        self.assertEqual(calculate.rel_volume(df), 0.85)

    def test_baseline_excludes_the_measured_session(self):
        # Baseline is the 60 sessions BEFORE the last completed one.
        df = frame([500] * 10 + [1000] * 60 + [2000] + [1])
        self.assertEqual(calculate.rel_volume(df), 2.0)

    def test_fewer_than_60_baseline_sessions_is_null(self):
        # 59 baseline sessions + last completed + partial = 61 bars: not enough.
        self.assertIsNone(calculate.rel_volume(frame([100] * 59 + [200, 7])))
        self.assertIsNone(calculate.rel_volume(frame([100, 100, 200, 7])))

    def test_exactly_60_baseline_sessions_is_enough(self):
        self.assertEqual(calculate.rel_volume(frame([100] * 60 + [150, 7])), 1.5)

    def test_zero_baseline_is_null(self):
        self.assertIsNone(calculate.rel_volume(frame([0] * 60 + [100, 3])))

    def test_avg_volume_unchanged(self):
        df = frame([1000] * 60 + [850] + [5])
        self.assertEqual(calculate.avg_volume(df), round((1000 * 59 + 850) / 60, 0))


class CandidateMetricsTest(unittest.TestCase):
    def test_day_change_is_last_bar_vs_previous_close(self):
        df = pd.DataFrame({"Close": [26.0, 12.5]})
        self.assertEqual(calculate.day_change_pct(df), -51.92)

    def test_day_change_unknown_with_one_bar(self):
        self.assertEqual(calculate.day_change_pct(pd.DataFrame({"Close": [10.0]})), 0.0)

    def test_range_pct_uses_last_bar(self):
        df = pd.DataFrame({"Close": [10.0, 10.0], "High": [11.0, 11.0], "Low": [9.0, 9.5]})
        self.assertEqual(calculate.range_pct(df), 15.0)

    def test_avg_dollar_volume_excludes_partial_bar(self):
        df = pd.DataFrame({"Close": [10.0] * 60 + [99.0], "Volume": [1000] * 60 + [1]})
        self.assertEqual(calculate.avg_dollar_volume(df), 10000.0)


if __name__ == "__main__":
    unittest.main()
