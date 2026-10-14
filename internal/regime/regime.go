// Package regime labels the macro environment from a handful of market series
// with simple deterministic rules. The LLM is given both the numbers and the
// label; it does not compute either.
package regime

import (
	"fmt"
	"strings"

	"github.com/francocaccia/quantengine/internal/indicators"
)

const (
	// VIXHigh and VIXLow bound the volatility regimes.
	VIXHigh = 25.0
	VIXLow  = 20.0
)

// Regime is the classified macro environment.
type Regime struct {
	Available bool
	Label     string // "risk-off, inverted curve", ..., or "unavailable"

	Y13W, Y5Y, Y10Y, Y30Y float64 // percent
	Slope                 float64 // 10y minus 13w, percentage points
	VIX                   float64
	SPY, SPYMA            float64
	TLT, TLTMA            float64

	RiskOff     bool // VIX >= 25 or SPY below its 200d MA
	RiskOn      bool // VIX < 20 and SPY above its 200d MA
	RatesRising bool // TLT below its 200d MA: long duration is in a downtrend
	Inverted    bool // 10y-3m slope < 0
	// RiskElevated is the condition under which equity risk is considered
	// high: high VIX, SPY below its 200d MA, or an inverted curve.
	RiskElevated bool
}

// Unavailable is the regime used when macro data cannot be fetched.
func Unavailable() Regime { return Regime{Label: "unavailable"} }

// Classify labels the regime. Every input is required: with any series
// missing the result is Unavailable, because a half-known regime would give
// false confidence.
func Classify(m indicators.Macro) Regime {
	get := func(sym string) (float64, bool) { return m.Value(sym) }
	y13, ok1 := get("^IRX")
	y5, ok2 := get("^FVX")
	y10, ok3 := get("^TNX")
	y30, ok4 := get("^TYX")
	vix, ok5 := get("^VIX")
	spy, ok6 := get("SPY")
	spyMA, ok7 := m.MA200("SPY")
	tlt, ok8 := get("TLT")
	tltMA, ok9 := m.MA200("TLT")
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7 && ok8 && ok9) || spyMA <= 0 || tltMA <= 0 {
		return Unavailable()
	}

	r := Regime{
		Available: true,
		Y13W:      y13, Y5Y: y5, Y10Y: y10, Y30Y: y30, Slope: y10 - y13, VIX: vix,
		SPY: spy, SPYMA: spyMA, TLT: tlt, TLTMA: tltMA,
	}
	r.RiskOff = vix >= VIXHigh || spy < spyMA
	r.RiskOn = vix < VIXLow && spy >= spyMA
	r.RatesRising = tlt < tltMA
	r.Inverted = r.Slope < 0
	r.RiskElevated = r.RiskOff || r.Inverted

	parts := []string{"neutral"}
	switch {
	case r.RiskOff:
		parts[0] = "risk-off"
	case r.RiskOn:
		parts[0] = "risk-on"
	}
	if r.RatesRising {
		parts = append(parts, "rates-rising")
	}
	if r.Inverted {
		parts = append(parts, "inverted curve")
	}
	r.Label = strings.Join(parts, ", ")
	return r
}

func relation(price, ma float64) string {
	pct := (price/ma - 1) * 100
	side := "above"
	if price < ma {
		side = "below"
	}
	return fmt.Sprintf("%.2f vs 200d MA %.2f (%+.1f%%, %s)", price, ma, pct, side)
}

// Text renders the regime block for the prompt. The numbers and the label are
// what the model may quote as a macro catalyst.
func (r Regime) Text() string {
	if !r.Available {
		return "Macro regime: unavailable (macro data could not be fetched)\n"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Macro regime: %s\n", r.Label)
	fmt.Fprintf(&sb, "Yields: 13w %.2f%% | 5y %.2f%% | 10y %.2f%% | 30y %.2f%%\n", r.Y13W, r.Y5Y, r.Y10Y, r.Y30Y)
	curve := "normal"
	if r.Inverted {
		curve = "inverted"
	}
	fmt.Fprintf(&sb, "10y-3m slope: %+.2f pts (%s)\n", r.Slope, curve)
	fmt.Fprintf(&sb, "VIX: %.1f\n", r.VIX)
	fmt.Fprintf(&sb, "SPY %s\n", relation(r.SPY, r.SPYMA))
	fmt.Fprintf(&sb, "TLT %s\n", relation(r.TLT, r.TLTMA))
	return sb.String()
}
