package universe

import (
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/indicators"
)

// good is a candidate that passes every filter at the default limits.
func good() *indicators.Result {
	return &indicators.Result{Price: 20, ATR: 0.8, AvgVolume: 3_000_000, DayChangePct: 2, RangePct: 4,
		MarketCap: 5e9, QuoteType: "EQUITY", VolumeSource: "yahoo"}
}

func TestCandidateQualityFilters(t *testing.T) {
	lim := DefaultLimits()
	if _, err := CheckCandidate(nil, good(), lim); err != nil {
		t.Fatalf("baseline candidate rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*indicators.Result)
		want   string // substring of the rejection, which must carry the measured value
	}{
		{"crash like AEHL (-52% in a session)", func(r *indicators.Result) { r.DayChangePct = -51.9 }, "-51.9%"},
		{"spike", func(r *indicators.Result) { r.DayChangePct = 24.3 }, "+24.3%"},
		{"wide intraday range", func(r *indicators.Result) { r.RangePct = 16.2 }, "range 16.2%"},
		{"high ATR", func(r *indicators.Result) { r.ATR = 1.6 }, "ATR(14) 8.0%"}, // 1.6 / 20
		{"small cap", func(r *indicators.Result) { r.MarketCap = 1.2e9 }, "market cap $1.20B"},
	} {
		r := good()
		tc.mutate(r)
		_, err := CheckCandidate(nil, r, lim)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

func TestCandidateQualityBoundariesPass(t *testing.T) {
	lim := DefaultLimits()
	r := good()
	r.DayChangePct, r.RangePct, r.ATR, r.MarketCap = -20, 15, 20*0.07, 2e9 // exactly at every limit
	if _, err := CheckCandidate(nil, r, lim); err != nil {
		t.Errorf("values exactly at the limits must pass: %v", err)
	}
}

func TestMarketCapCheckIsSkippedWhenUnknown(t *testing.T) {
	r := good()
	r.MarketCap = 0
	if _, err := CheckCandidate(nil, r, DefaultLimits()); err != nil {
		t.Errorf("unknown market cap must skip the check, not reject: %v", err)
	}
}

func TestZeroLimitsDisableEachCheck(t *testing.T) {
	r := good()
	r.DayChangePct, r.RangePct, r.ATR, r.MarketCap = -60, 40, 9, 1e8
	if _, err := CheckCandidate(nil, r, Limits{}); err != nil {
		t.Errorf("a zero Limits must disable the quality checks: %v", err)
	}
	if err := CheckMarket(nil, r); err != nil {
		t.Errorf("CheckMarket has no quality limits: %v", err)
	}
}

// Core symbols and bond ETFs are exempt from the quality limits; candidates are not.
func TestQualityLimitsApplyToCandidatesOnly(t *testing.T) {
	wild := func(string) (*indicators.Result, error) {
		r := good()
		r.DayChangePct, r.RangePct, r.ATR, r.MarketCap = -30, 25, 3, 5e8
		return r, nil
	}
	assets := fakeAssets{"SPY": asset("SPY", "ARCA"), "SGOV": asset("SGOV", "ARCA"), "CAND": asset("CAND", "NASDAQ")}
	u := New([]string{"SPY"}, 1, assets, nil, wild)
	u.Exempt = func(s string) bool { return s == "SGOV" }
	for _, sym := range []string{"SPY", "SGOV"} {
		if _, err := u.Validate(sym); err != nil {
			t.Errorf("%s must be exempt from the quality limits: %v", sym, err)
		}
	}
	if _, err := u.Validate("CAND"); err == nil || !strings.Contains(err.Error(), "1-day price change") {
		t.Errorf("candidate must be rejected for its day move, err = %v", err)
	}
}

func TestDollarVolumeFallbackWhenYahooVolumeIsMissing(t *testing.T) {
	lim := DefaultLimits()
	r := good()
	r.AvgVolume, r.VolumeSource, r.AvgDollarVolume = 0, "", 45_000_000
	liq, err := CheckCandidate(nil, r, lim)
	if err != nil || !strings.Contains(liq, "Alpaca bars fallback") || !strings.Contains(liq, "$45.0M") {
		t.Errorf("fallback should accept and name its source: %q %v", liq, err)
	}

	r.AvgDollarVolume = 19_900_000
	if _, err := CheckCandidate(nil, r, lim); err == nil || !strings.Contains(err.Error(), "$19.9M below $20M") {
		t.Errorf("below the dollar-volume minimum must reject with the value: %v", err)
	}

	r.AvgDollarVolume = 20_000_000
	if _, err := CheckCandidate(nil, r, lim); err != nil {
		t.Errorf("exactly the minimum passes: %v", err)
	}

	// Both sources failed: rejected as before.
	r.AvgDollarVolume = 0
	if _, err := CheckCandidate(nil, r, lim); err == nil || !strings.Contains(err.Error(), "volume unavailable") {
		t.Errorf("both sources unavailable must reject: %v", err)
	}

	// The fallback can be disabled.
	r.AvgDollarVolume = 90_000_000
	if _, err := CheckCandidate(nil, r, Limits{}); err == nil {
		t.Error("with MinDollarVol 0 the fallback is off")
	}
}

func TestYahooVolumeStillDecidesWhenAvailable(t *testing.T) {
	// A thin Yahoo volume is not rescued by a large dollar volume.
	r := good()
	r.AvgVolume, r.AvgDollarVolume = 500_000, 90_000_000
	if _, err := CheckCandidate(nil, r, DefaultLimits()); err == nil {
		t.Error("the fallback is only for a missing Yahoo volume")
	}
	// The log names a cached value as such.
	r = good()
	r.VolumeSource = "cache"
	if liq, err := CheckCandidate(nil, r, DefaultLimits()); err != nil || !strings.Contains(liq, "(cache)") {
		t.Errorf("liquidity source = %q %v", liq, err)
	}
}

// Core is never rescued or judged by the dollar-volume fallback: it stays exempt.
func TestCoreWithoutAnyVolumeIsKept(t *testing.T) {
	assets := fakeAssets{"SPY": asset("SPY", "ARCA")}
	u := New([]string{"SPY"}, 0, assets, nil, func(string) (*indicators.Result, error) {
		return &indicators.Result{Price: 500, QuoteType: "ETF", AvgDollarVolume: 1}, nil
	})
	if _, err := u.Validate("SPY"); err != nil {
		t.Errorf("core must be kept: %v", err)
	}
}
