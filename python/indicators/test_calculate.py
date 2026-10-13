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

    def test_short_history_uses_what_exists(self):
        df = frame([100, 100, 200, 7])
        self.assertEqual(calculate.rel_volume(df), 2.0)

    def test_degenerate_inputs(self):
        self.assertEqual(calculate.rel_volume(frame([5, 5])), 0.0)
        self.assertEqual(calculate.rel_volume(frame([0, 0, 100, 3])), 0.0)

    def test_avg_volume_unchanged(self):
        df = frame([1000] * 60 + [850] + [5])
        self.assertEqual(calculate.avg_volume(df), round((1000 * 59 + 850) / 60, 0))


if __name__ == "__main__":
    unittest.main()
