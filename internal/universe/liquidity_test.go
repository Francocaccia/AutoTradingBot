package universe

import (
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
	u := New([]string{"SGOV"}, 0, fakeAssets{"SGOV": {Symbol: "SGOV", Class: "us_equity", Exchange: "ARCA", Status: "active", Tradable: true, Name: "iShares 0-3 Month Treasury Bond ETF"}},
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
