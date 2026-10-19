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

// blend6040 builds a 60% SPY / 40% AGG portfolio rebalanced daily, starting
// from the first value of the SPY series. Both inputs must be the same length.
func blend6040(spy, agg []float64) []float64 {
	out := make([]float64, len(spy))
	if len(spy) == 0 {
		return out
	}
	out[0] = spy[0]
	for i := 1; i < len(spy); i++ {
		r := 0.6*(spy[i]/spy[i-1]-1) + 0.4*(agg[i]/agg[i-1]-1)
		out[i] = out[i-1] * (1 + r)
	}
	return out
}

// ratio is mean daily return / stdev of daily returns * sqrt(252), or nil when
// the series is flat or too short.
func ratio(values []float64) *float64 {
	rets := dailyReturns(values)
	if len(rets) < 2 {
		return nil
	}
	var sum float64
	for _, r := range rets {
		sum += r
	}
	mean := sum / float64(len(rets))
	var ss float64
	for _, r := range rets {
		ss += (r - mean) * (r - mean)
	}
	sd := math.Sqrt(ss / float64(len(rets)-1))
	if sd == 0 {
		return nil
	}
	return ptr(round2(mean / sd * math.Sqrt(252)))
}

// stdev is the sample standard deviation (n-1); 0 for fewer than two values.
func stdev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// annualise scales a daily standard deviation to a yearly one (252 sessions).
func annualise(daily float64) float64 { return daily * math.Sqrt(252) }

// betaCorr returns the beta of a to market and their correlation, from two
// equally long daily-return series. ok is false when the series are shorter
// than two values or the market (or a) does not vary.
func betaCorr(a, market []float64) (beta, corr float64, ok bool) {
	if len(a) != len(market) || len(a) < 2 {
		return 0, 0, false
	}
	var sa, sm float64
	for i := range a {
		sa += a[i]
		sm += market[i]
	}
	ma, mm := sa/float64(len(a)), sm/float64(len(a))
	var cov, va, vm float64
	for i := range a {
		cov += (a[i] - ma) * (market[i] - mm)
		va += (a[i] - ma) * (a[i] - ma)
		vm += (market[i] - mm) * (market[i] - mm)
	}
	if va == 0 || vm == 0 {
		return 0, 0, false
	}
	return cov / vm, cov / math.Sqrt(va*vm), true
}

// activeReturns is the per-day difference between two return series of equal length.
func activeReturns(a, b []float64) []float64 {
	out := make([]float64, 0, len(a))
	for i := range a {
		out = append(out, a[i]-b[i])
	}
	return out
}

// indexTo100 rescales values so the first one is 100. A non-positive first
// value yields nil: there is no meaningful base.
func indexTo100(values []float64) []float64 {
	if len(values) == 0 || values[0] <= 0 {
		return nil
	}
	out := make([]float64, len(values))
	for i, v := range values {
		out[i] = v / values[0] * 100
	}
	return out
}
