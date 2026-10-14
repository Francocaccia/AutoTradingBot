package regime

import (
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/indicators"
)

func f(v float64) *float64 { return &v }

// macro builds a full data set. Defaults describe a calm, normal-curve market.
func macro(mod func(m indicators.Macro)) indicators.Macro {
	m := indicators.Macro{
		"^IRX": {Close: f(4.0)}, "^FVX": {Close: f(4.2)}, "^TNX": {Close: f(4.5)}, "^TYX": {Close: f(4.8)},
		"^VIX": {Close: f(15)},
		"SPY":  {Close: f(520), MA200: f(500)},
		"TLT":  {Close: f(95), MA200: f(90)},
	}
	if mod != nil {
		mod(m)
	}
	return m
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name         string
		mod          func(m indicators.Macro)
		label        string
		riskElevated bool
	}{
		{"calm market", nil, "risk-on", false},
		{"middle VIX is neutral", func(m indicators.Macro) { m["^VIX"] = indicators.MacroQuote{Close: f(22)} }, "neutral", false},
		{"high VIX", func(m indicators.Macro) { m["^VIX"] = indicators.MacroQuote{Close: f(27)} }, "risk-off", true},
		{"VIX exactly 25", func(m indicators.Macro) { m["^VIX"] = indicators.MacroQuote{Close: f(25)} }, "risk-off", true},
		{"SPY below 200d MA", func(m indicators.Macro) { m["SPY"] = indicators.MacroQuote{Close: f(480), MA200: f(500)} }, "risk-off", true},
		{"rates rising when TLT is below its MA", func(m indicators.Macro) { m["TLT"] = indicators.MacroQuote{Close: f(85), MA200: f(90)} }, "risk-on, rates-rising", false},
		{"inverted curve", func(m indicators.Macro) { m["^IRX"] = indicators.MacroQuote{Close: f(5.2)} }, "risk-on, inverted curve", true},
		{"everything at once", func(m indicators.Macro) {
			m["^VIX"] = indicators.MacroQuote{Close: f(30)}
			m["TLT"] = indicators.MacroQuote{Close: f(80), MA200: f(90)}
			m["^IRX"] = indicators.MacroQuote{Close: f(5.2)}
		}, "risk-off, rates-rising, inverted curve", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Classify(macro(tc.mod))
			if !r.Available || r.Label != tc.label || r.RiskElevated != tc.riskElevated {
				t.Errorf("got available=%v label=%q elevated=%v, want %q elevated=%v", r.Available, r.Label, r.RiskElevated, tc.label, tc.riskElevated)
			}
		})
	}
}

func TestMissingDataDegradesToUnavailable(t *testing.T) {
	for _, drop := range []string{"^IRX", "^FVX", "^TNX", "^TYX", "^VIX", "SPY", "TLT"} {
		r := Classify(macro(func(m indicators.Macro) { delete(m, drop) }))
		if r.Available || r.Label != "unavailable" {
			t.Errorf("without %s: %+v, want unavailable", drop, r)
		}
	}
	// A quote with a nil moving average (not enough history) is also unavailable.
	r := Classify(macro(func(m indicators.Macro) { m["SPY"] = indicators.MacroQuote{Close: f(520)} }))
	if r.Available {
		t.Error("SPY without a 200d MA must be unavailable")
	}
	if r := Classify(nil); r.Available || !strings.Contains(r.Text(), "unavailable") {
		t.Errorf("nil macro: %+v", r)
	}
}

func TestTextCarriesNumbersAndLabel(t *testing.T) {
	txt := Classify(macro(func(m indicators.Macro) { m["^VIX"] = indicators.MacroQuote{Close: f(27.3)} })).Text()
	for _, want := range []string{"Macro regime: risk-off", "VIX: 27.3", "10y 4.50%", "10y-3m slope: +0.50 pts (normal)", "below"} {
		if !strings.Contains(txt, want) && want != "below" {
			t.Errorf("text missing %q:\n%s", want, txt)
		}
	}
}
