package gemini

import (
	"fmt"
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

func TestPromptHasNoRawIntradayVolume(t *testing.T) {
	ind := &indicators.Result{Price: 100, ATR: 2, Volume: 123456, AvgVolume: 987654, RelVolume: 0.85}
	p := BuildPrompt(PromptInput{Symbol: "SPY", Indicators: ind})
	for _, bad := range []string{"123456", "987654", "Avg daily volume", "Volume:"} {
		if strings.Contains(p, bad) {
			t.Errorf("prompt must not contain %q", bad)
		}
	}
	for _, want := range []string{
		"Last completed session volume vs 60-session average: 0.85x",
		"Intraday volume is not available. Do not infer low volume from the current session.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// Unknown relative volume prints nothing rather than 0.00x.
	ind.RelVolume = 0
	if p = BuildPrompt(PromptInput{Symbol: "SPY", Indicators: ind}); strings.Contains(p, "60-session average") {
		t.Error("unknown relative volume must not be printed")
	}
}

func TestPromptSelectivityRules(t *testing.T) {
	p := BuildPrompt(PromptInput{Symbol: "SPY", Indicators: &indicators.Result{Price: 100, ATR: 2}})
	for _, gone := range []string{"Idle cash has an opportunity cost", "Choose BUY when conviction >= 7"} {
		if strings.Contains(p, gone) {
			t.Errorf("old rule still present: %q", gone)
		}
	}
	for _, want := range []string{
		"Doing nothing is a valid and often the best decision. HOLD, or staying in cash, is a position. There is no target number of trades and many sessions should end with no trades. Do not trade to be active.",
		"BUY only if BOTH hold: (a) a specific catalyst quoted from the RECENT NEWS section, and (b) a technical setup consistent with a multi-week swing and at least 2:1 reward/risk. An oversold or overbought reading alone is never enough. Do not buy short-term bounces.",
		"SELL only if the thesis is broken, the news turned against the position, or the reward/risk no longer justifies holding. Do not sell only because oscillators are overbought.",
		"Do not reverse a recent decision unless something material changed. If nothing new, HOLD.",
		"Conviction scale: 5 means no edge. 8 or more must be rare. Use 7 or below when unsure.",
		`"catalyst":`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestPromptShowsPositionAgeAndLastDecision(t *testing.T) {
	pos := &portfolio.Position{Qty: 10, AvgPrice: 95.5, PLPercent: 0.047}
	ind := &indicators.Result{Price: 100, ATR: 2}
	p := BuildPrompt(PromptInput{Symbol: "AAPL", Indicators: ind, Position: pos, DaysHeld: 2,
		Last: &LastDecision{Action: "HOLD", Thesis: "range-bound, no catalyst", Date: "2026-10-12"}})
	for _, want := range []string{"Trading days since opened: 2", "## YOUR LAST DECISION ON THIS SYMBOL", "HOLD on 2026-10-12: range-bound, no catalyst"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// Unknown age (legacy position) and no history print neither section.
	p = BuildPrompt(PromptInput{Symbol: "AAPL", Indicators: ind, Position: pos, DaysHeld: -1})
	if strings.Contains(p, "Trading days since opened") || strings.Contains(p, "LAST DECISION") {
		t.Errorf("unknown age / no last decision must print nothing:\n%s", p)
	}
	// A flat symbol never shows an age.
	if p = BuildPrompt(PromptInput{Symbol: "AAPL", Indicators: ind, DaysHeld: 3}); strings.Contains(p, "Trading days since opened") {
		t.Error("flat symbol must not show a position age")
	}
}

func TestPromptHeadlineLimitMatchesExported(t *testing.T) {
	var arts []news.Article
	for i := 0; i < MaxPromptHeadlines+3; i++ {
		arts = append(arts, news.Article{Source: "S", Headline: fmt.Sprintf("headline number %02d", i)})
	}
	p := BuildPrompt(PromptInput{Symbol: "SPY", News: arts})
	if !strings.Contains(p, fmt.Sprintf("headline number %02d", MaxPromptHeadlines-1)) || strings.Contains(p, fmt.Sprintf("headline number %02d", MaxPromptHeadlines)) {
		t.Errorf("prompt must show exactly %d headlines", MaxPromptHeadlines)
	}
}

func TestParseDecisionReadsCatalyst(t *testing.T) {
	d, err := parseDecision(`{"action":"buy","conviction":8,"thesis":"t","reasoning":"r","catalyst":"ACME wins contract","take_profit":0,"suggested_tickers":[]}`)
	if err != nil || d.Action != "BUY" || d.Catalyst != "ACME wins contract" {
		t.Fatalf("got %+v, %v", d, err)
	}
	if d, _ = parseDecision(`{"action":"HOLD","conviction":5}`); d.Catalyst != "" {
		t.Errorf("missing catalyst must be empty, got %q", d.Catalyst)
	}
}
