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


class WorkerResilienceTest(unittest.TestCase):
    def test_http_get_json_retries_429_and_5xx_then_succeeds(self):
        import io
        import urllib.error

        calls = []

        class Resp:
            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

            def read(self):
                return b'{"ok": true}'

        def opener(req, timeout):
            calls.append(timeout)
            if len(calls) == 1:
                raise urllib.error.HTTPError("u", 429, "slow down", {}, io.BytesIO(b""))
            if len(calls) == 2:
                raise urllib.error.URLError("connection reset")
            return Resp()

        sleeps = []
        out = calculate.http_get_json("req", opener=opener, sleep=sleeps.append)
        self.assertEqual(out, {"ok": True})
        self.assertEqual(len(calls), 3)
        self.assertEqual(len(sleeps), 2)
        self.assertTrue(all(t == calculate.REQUEST_TIMEOUT_S for t in calls))

    def test_http_get_json_does_not_retry_real_errors(self):
        import io
        import urllib.error

        calls = []

        def opener(req, timeout):
            calls.append(1)
            raise urllib.error.HTTPError("u", 403, "forbidden", {}, io.BytesIO(b""))

        with self.assertRaises(urllib.error.HTTPError):
            calculate.http_get_json("req", opener=opener, sleep=lambda d: None)
        self.assertEqual(len(calls), 1)

    def test_http_get_json_gives_up_after_three_attempts(self):
        import urllib.error

        calls = []

        def opener(req, timeout):
            calls.append(1)
            raise urllib.error.URLError("down")

        with self.assertRaises(urllib.error.URLError):
            calculate.http_get_json("req", opener=opener, sleep=lambda d: None)
        self.assertEqual(len(calls), 3)

    def test_fundamentals_stop_at_the_time_budget(self):
        now = [0.0]

        def clock():
            return now[0]

        def get_info(sym):
            now[0] += 30.0  # every symbol takes 30 seconds
            return {"averageVolume": 1000, "quoteType": "EQUITY", "beta": 1.1}

        out = calculate.collect_fundamentals(["A", "B", "C", "D"], get_info, 60.0, clock=clock, sleep=lambda d: None)
        self.assertEqual(out["A"]["avg_volume"], 1000.0)
        self.assertEqual(out["B"]["avg_volume"], 1000.0)
        self.assertIn("time budget", out["C"]["error"])
        self.assertIn("time budget", out["D"]["error"])

    def test_empty_yahoo_answer_is_a_failure_not_zeros(self):
        out = calculate.collect_fundamentals(["A"], lambda s: {}, 60.0, sleep=lambda d: None)
        self.assertIn("no data", out["A"]["error"])

    def test_budget_comes_from_the_environment(self):
        os.environ["WORKER_BUDGET_S"] = "42"
        try:
            self.assertEqual(calculate.budget_seconds(), 42.0)
            os.environ["WORKER_BUDGET_S"] = "junk"
            self.assertEqual(calculate.budget_seconds(), calculate.DEFAULT_BUDGET_S)
        finally:
            del os.environ["WORKER_BUDGET_S"]


if __name__ == "__main__":
    unittest.main()
