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
