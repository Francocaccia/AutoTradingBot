package risk

import "testing"

func TestCapQtyByRisk(t *testing.T) {
	tests := []struct {
		name                            string
		qty                             int
		price, stop, equity, maxRiskPct float64
		wantQty                         int
		wantStopPctMin, wantStopPctMax  float64
	}{
		// 2026-10-13: CIFR, 644 shares (7% of ~102k equity), stop 23% away.
		{"CIFR-like wide stop is cut to ~400 shares", 644, 11.13, 8.56, 102_000, 1.0, 396, 23.0, 23.2},
		{"CIFR at a flat 100k gives ~390, not 644", 644, 11.13, 8.56, 100_000, 1.0, 389, 23.0, 23.2},
		{"tight stop is unchanged", 70, 100, 98, 100_000, 1.0, 70, 2.0, 2.0},
		{"exactly at the cap is unchanged", 400, 100, 97.5, 100_000, 1.0, 400, 2.5, 2.5},
		{"one share over the cap is trimmed", 401, 100, 97.5, 100_000, 1.0, 400, 2.5, 2.5},
		{"less than one share fits", 10, 50, 10, 100, 1.0, 0, 80, 80},
		{"higher MAX_RISK_PCT allows more", 644, 11.13, 8.56, 102_000, 2.0, 644, 23.0, 23.2},
		{"zero disables the cap", 644, 11.13, 8.56, 102_000, 0, 644, 23.0, 23.2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, stopPct := CapQtyByRisk(tc.qty, tc.price, tc.stop, tc.equity, tc.maxRiskPct)
			if got != tc.wantQty {
				t.Errorf("qty = %d, want %d", got, tc.wantQty)
			}
			if stopPct < tc.wantStopPctMin || stopPct > tc.wantStopPctMax {
				t.Errorf("stopPct = %.2f, want in [%.1f, %.1f]", stopPct, tc.wantStopPctMin, tc.wantStopPctMax)
			}
			if tc.maxRiskPct > 0 && got > 0 {
				if risk := float64(got) * (tc.price - tc.stop); risk > tc.equity*tc.maxRiskPct/100+1e-6 {
					t.Errorf("risk $%.2f exceeds %.1f%% of equity", risk, tc.maxRiskPct)
				}
			}
		})
	}
}

func TestCapQtyByRiskIgnoresInvalidStops(t *testing.T) {
	if got, _ := CapQtyByRisk(100, 10, 10, 1000, 1); got != 100 {
		t.Errorf("stop at price must leave qty alone, got %d", got)
	}
	if got, _ := CapQtyByRisk(100, 10, 12, 1000, 1); got != 100 {
		t.Errorf("stop above price must leave qty alone, got %d", got)
	}
	if got, _ := CapQtyByRisk(100, 10, 9, 0, 1); got != 100 {
		t.Errorf("unknown equity must leave qty alone, got %d", got)
	}
}
