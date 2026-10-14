package universe

import (
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/indicators"
)

// The liquidity filter reads AvgVolume (Yahoo, consolidated) only. The raw
// intraday Volume and the new RelVolume must not change the outcome.
func TestLiquidityFilterIgnoresIntradayAndRelativeVolume(t *testing.T) {
	liquid := &indicators.Result{Price: 50, AvgVolume: 2_000_000, Volume: 3, RelVolume: 0.01, QuoteType: "EQUITY"}
	if err := CheckMarket(nil, liquid); err != nil {
		t.Errorf("liquid symbol with tiny intraday/relative volume was rejected: %v", err)
	}
	thin := &indicators.Result{Price: 50, AvgVolume: 999_999, Volume: 90_000_000, RelVolume: 40, QuoteType: "EQUITY"}
	if err := CheckMarket(nil, thin); err == nil {
		t.Error("thin symbol must stay rejected however large its intraday/relative volume")
	}
	if err := CheckMarket(nil, &indicators.Result{Price: 50, RelVolume: 3, Volume: 5_000_000, QuoteType: "EQUITY"}); err == nil {
		t.Error("unknown average volume must still be rejected")
	}
	edge := &indicators.Result{Price: 50, AvgVolume: MinAvgVolume, QuoteType: "EQUITY"}
	if err := CheckMarket(nil, edge); err != nil {
		t.Errorf("exactly %.0f must pass: %v", MinAvgVolume, err)
	}
}

// Curated bond ETFs skip the price and volume filters; the leveraged check stays.
func TestBondETFsAreExemptFromPriceAndVolumeFilters(t *testing.T) {
	thin := &indicators.Result{Price: 3.5, AvgVolume: 10_000, QuoteType: "ETF", Name: "Some Bond Fund"}
	if err := CheckMarket(nil, thin); err == nil {
		t.Fatal("a thin cheap symbol fails the normal filter")
	}
	if err := CheckMarketExempt(nil, thin); err != nil {
		t.Errorf("exempt check rejected a bond ETF: %v", err)
	}
	if err := CheckMarketExempt(nil, &indicators.Result{Price: 100, QuoteType: "ETF", Name: "Ultra 3x Treasury Bull"}); err == nil {
		t.Error("leveraged products are never allowed, exempt or not")
	}
	if err := CheckMarketExempt(nil, nil); err == nil {
		t.Error("no data must fail")
	}
	if err := CheckMarketExempt(nil, &indicators.Result{Price: 0}); err == nil {
		t.Error("no price must fail")
	}

	// End to end through Validate with Exempt set.
	u := New([]string{"SPY"}, 0, fakeAssets{"SGOV": {Symbol: "SGOV", Class: "us_equity", Exchange: "ARCA", Status: "active", Tradable: true, Name: "iShares 0-3 Month Treasury Bond ETF"}},
		nil, func(string) (*indicators.Result, error) {
			return &indicators.Result{Price: 100.4, AvgVolume: 400_000, QuoteType: "ETF"}, nil
		})
	if _, err := u.Validate("SGOV"); err == nil {
		t.Fatal("without the exemption 400k average volume fails")
	}
	u.Exempt = func(sym string) bool { return sym == "SGOV" }
	if _, err := u.Validate("SGOV"); err != nil {
		t.Errorf("exempt bond ETF rejected: %v", err)
	}
}

// A core symbol whose average volume is unavailable (Yahoo failing) is kept;
// a screener candidate with the same gap is rejected (fails closed).
func TestCoreKeptAndCandidateRejectedWhenVolumeUnavailable(t *testing.T) {
	assets := fakeAssets{"SPY": asset("SPY", "ARCA"), "CAND": asset("CAND", "NASDAQ")}
	noVol := func(price float64) *indicators.Result {
		return &indicators.Result{Price: price, AvgVolume: 0, QuoteType: "EQUITY"}
	}
	data := map[string]*indicators.Result{"SPY": noVol(500), "CAND": noVol(20)}
	u, _ := newTestUniverse(1, assets, data, fakeScreener{actives: []string{"CAND"}})

	items := u.Build(nil)
	got := symbols(items)
	if len(got) != 1 || got[0] != "SPY" || !items[0].Core {
		t.Fatalf("core SPY must be kept and CAND rejected, got %v", got)
	}
	if _, err := u.Validate("CAND"); err == nil || !strings.Contains(err.Error(), "volume unavailable") {
		t.Errorf("candidate with unknown volume must be rejected, err = %v", err)
	}
}

func TestCoreIsExemptFromAvgVolumeButNotFromPriceOrLeverage(t *testing.T) {
	assets := fakeAssets{"SPY": asset("SPY", "ARCA")}
	check := func(r *indicators.Result) error {
		u, _ := newTestUniverse(0, assets, map[string]*indicators.Result{"SPY": r}, nil)
		_, err := u.Validate("SPY")
		return err
	}
	if err := check(&indicators.Result{Price: 500, AvgVolume: 10_000, QuoteType: "ETF"}); err != nil {
		t.Errorf("thin-looking core must be kept: %v", err)
	}
	if err := check(&indicators.Result{Price: 2, QuoteType: "ETF"}); err == nil {
		t.Error("the price filter still applies to core")
	}
	if err := check(&indicators.Result{Price: 500, QuoteType: "ETF", Name: "Ultra 3x S&P"}); err == nil {
		t.Error("leveraged products are never allowed, even in core")
	}
	if err := CheckMarketCore(nil, nil); err == nil {
		t.Error("no data must fail")
	}
}

// A bond ETF with no volume reading is kept as well.
func TestExemptBondETFKeptWithoutVolume(t *testing.T) {
	assets := fakeAssets{"SGOV": {Symbol: "SGOV", Name: "iShares 0-3 Month Treasury Bond ETF", Class: "us_equity", Exchange: "ARCA", Status: "active", Tradable: true}}
	u := New([]string{"SPY"}, 0, assets, nil, func(string) (*indicators.Result, error) {
		return &indicators.Result{Price: 100.4, QuoteType: "ETF"}, nil
	})
	u.Exempt = func(sym string) bool { return sym == "SGOV" }
	if _, err := u.Validate("SGOV"); err != nil {
		t.Errorf("bond ETF without volume data must be kept: %v", err)
	}
}
