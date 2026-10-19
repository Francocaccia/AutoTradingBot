package portfolio

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestPolicyFromEnvDefaultsAndOverrides(t *testing.T) {
	p, warn := PolicyFromEnv(env(nil), 12)
	if len(warn) != 0 || p.EquityMax != 75 || p.MaxStockPct != 12 || p.MaxBondETFPct != 15 || p.CreditMaxPct != 10 || p.MaxDuration != 8 {
		t.Errorf("defaults wrong: %+v %v", p, warn)
	}
	p, warn = PolicyFromEnv(env(map[string]string{
		"IPS_EQUITY_MIN": "30", "IPS_EQUITY_MAX": "70", "IPS_BOND_MAX": "45", "IPS_MAX_DURATION": "5", "IPS_CREDIT_MAX_PCT": "5",
		"IPS_TARGET_EQUITY": "50", "IPS_TARGET_BOND": "35", "IPS_TARGET_CASH": "15",
	}), 10)
	if len(warn) != 0 || p.EquityMin != 30 || p.EquityMax != 70 || p.BondMax != 45 || p.MaxDuration != 5 || p.CreditMaxPct != 5 || p.TargetBond != 35 {
		t.Errorf("overrides wrong: %+v %v", p, warn)
	}
}

func TestBroadETFHasItsOwnSingleNameLimit(t *testing.T) {
	p, _ := PolicyFromEnv(env(nil), 10)
	if p.MaxBroadETFPct != 20 || p.MaxStockPct != 10 {
		t.Fatalf("defaults: %+v", p)
	}
	b := Book{Equity: 100_000, Cash: 60_000, Holdings: []Holding{{Symbol: "SPY", Value: 15_000}, {Symbol: "AAPL", Value: 9_000}, {Symbol: "XYZ", Value: 16_000, Sector: "Tech"}}}
	// An index ETF may grow to 20%, a stock stops at 10%.
	if room, bind := p.Room(b, "SPY", ""); room != 5_000 || !strings.Contains(bind, "index ETF") {
		t.Errorf("SPY room %.0f binding %q", room, bind)
	}
	if room, bind := p.Room(b, "AAPL", "Tech"); room != 1_000 || !strings.Contains(bind, "single position") {
		t.Errorf("AAPL room %.0f binding %q", room, bind)
	}
	if room, _ := p.Room(b, "QQQ", ""); room != 20_000 {
		t.Errorf("QQQ room %.0f", room)
	}
	p, _ = PolicyFromEnv(env(map[string]string{"IPS_MAX_BROAD_ETF_PCT": "12"}), 10)
	if p.MaxBroadETFPct != 12 {
		t.Errorf("override: %+v", p)
	}
}

func TestPolicyFromEnvRejectsBadValuesAndIncoherentBands(t *testing.T) {
	p, warn := PolicyFromEnv(env(map[string]string{"IPS_EQUITY_MAX": "abc", "IPS_BOND_MIN": "-3"}), 10)
	if len(warn) != 2 || p.EquityMax != 75 || p.BondMin != 15 {
		t.Errorf("bad values must be ignored with a warning: %+v %v", p, warn)
	}
	// Incoherent: equity min above its max -> whole policy falls back to the defaults.
	p, warn = PolicyFromEnv(env(map[string]string{"IPS_EQUITY_MIN": "80"}), 9)
	if len(warn) != 1 || !strings.Contains(warn[0], "invalid") || p.EquityMin != 40 || p.MaxStockPct != 9 {
		t.Errorf("incoherent bands: %+v %v", p, warn)
	}
}
