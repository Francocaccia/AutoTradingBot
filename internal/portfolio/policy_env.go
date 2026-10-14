package portfolio

import (
	"fmt"
	"strconv"
	"strings"
)

// PolicyFromEnv builds the IPS from defaults overridden by environment
// variables (looked up through getenv). Unparseable values are ignored with a
// warning. If the resulting bands are incoherent the defaults are used.
//
//	IPS_EQUITY_MIN / IPS_EQUITY_MAX   IPS_BOND_MIN / IPS_BOND_MAX   IPS_CASH_MIN / IPS_CASH_MAX
//	IPS_TARGET_EQUITY / IPS_TARGET_BOND / IPS_TARGET_CASH
//	IPS_MAX_BOND_ETF_PCT  IPS_SECTOR_MAX_PCT  IPS_CREDIT_MAX_PCT  IPS_MAX_DURATION  IPS_MIN_REAL_CASH_PCT
//
// maxStockPct (MAX_POSITION_PCT) is the single-stock limit.
func PolicyFromEnv(getenv func(string) string, maxStockPct float64) (Policy, []string) {
	p := DefaultPolicy()
	var warnings []string
	if maxStockPct > 0 {
		p.MaxStockPct = maxStockPct
	}
	fields := []struct {
		name string
		dst  *float64
	}{
		{"IPS_EQUITY_MIN", &p.EquityMin}, {"IPS_EQUITY_MAX", &p.EquityMax},
		{"IPS_BOND_MIN", &p.BondMin}, {"IPS_BOND_MAX", &p.BondMax},
		{"IPS_CASH_MIN", &p.CashMin}, {"IPS_CASH_MAX", &p.CashMax},
		{"IPS_TARGET_EQUITY", &p.TargetEquity}, {"IPS_TARGET_BOND", &p.TargetBond}, {"IPS_TARGET_CASH", &p.TargetCash},
		{"IPS_MAX_BOND_ETF_PCT", &p.MaxBondETFPct}, {"IPS_SECTOR_MAX_PCT", &p.SectorMaxPct},
		{"IPS_CREDIT_MAX_PCT", &p.CreditMaxPct}, {"IPS_MAX_DURATION", &p.MaxDuration},
		{"IPS_MIN_REAL_CASH_PCT", &p.MinRealCash},
	}
	for _, f := range fields {
		v := strings.TrimSpace(getenv(f.name))
		if v == "" {
			continue
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 {
			warnings = append(warnings, fmt.Sprintf("invalid %s %q ignored", f.name, v))
			continue
		}
		*f.dst = n
	}
	if err := p.Validate(); err != nil {
		warnings = append(warnings, fmt.Sprintf("investment policy from the environment is invalid (%v); using the defaults", err))
		d := DefaultPolicy()
		if maxStockPct > 0 {
			d.MaxStockPct = maxStockPct
		}
		return d, warnings
	}
	return p, warnings
}
