package stats

import "math"

// pctChange is the percentage change from base to now; 0 when base is not positive.
func pctChange(now, base float64) float64 {
	if base <= 0 {
		return 0
	}
	return (now - base) / base * 100
}

// dailyReturns returns the percentage change between consecutive values.
func dailyReturns(values []float64) []float64 {
	out := make([]float64, 0, len(values))
	for i := 1; i < len(values); i++ {
		out = append(out, pctChange(values[i], values[i-1]))
	}
	return out
}

// maxDrawdown returns the largest peak-to-trough decline in values as a
// non-positive percentage (0 when the series never falls below a prior peak).
func maxDrawdown(values []float64) float64 {
	var peak, worst float64
	for _, v := range values {
		if v > peak {
			peak = v
		}
		if peak > 0 {
			if dd := (v - peak) / peak * 100; dd < worst {
				worst = dd
			}
		}
	}
	return worst
}

func minMax(xs []float64) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, x := range xs {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	return lo, hi
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
