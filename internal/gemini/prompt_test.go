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
		"rel_volume: 0.85x (last completed session vs 60-session average)",
		"Intraday volume is not available. Use rel_volume (last completed session vs 60-session average) only.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// Unknown relative volume (fewer than 60 baseline sessions) prints no value.
	ind.RelVolume = 0
	if p = BuildPrompt(PromptInput{Symbol: "SPY", Indicators: ind}); strings.Contains(p, "rel_volume: ") {
		t.Error("unknown relative volume must be omitted")
	}
}

func TestPromptGoalAndSelectivityRules(t *testing.T) {
	p := BuildPrompt(PromptInput{Symbol: "SPY", Indicators: &indicators.Result{Price: 100, ATR: 2}})
	for _, gone := range []string{"Idle cash has an opportunity cost", "Choose BUY when conviction >= 7", "do not default to HOLD"} {
		if strings.Contains(p, gone) {
			t.Errorf("old rule still present: %q", gone)
		}
	}
	for _, want := range []string{
		"multi-asset investor portfolio",
		"risk-adjusted return (Sharpe-like) subject to a drawdown limit, benchmarked against SPY and a 60/40 (SPY/AGG) portfolio",
		"never take extra risk just to chase return, and never trade just to be active",
		"Doing nothing is a valid and often the best decision. Most days have no trade worth making. Two or three trades in a week is better than ten in a day.",
		"Single-stock BUY only with a catalyst quoted from the provided news (exact headline text) PLUS a multi-week swing setup with at least 2:1 reward/risk.",
		"SELL only if the thesis is broken (stop logic, catalyst invalidated, fundamental news), not because of one red day",
		"Do not reverse a position without a material change since the last action.",
		"Conviction scale: 5 = no edge, 6-7 = interesting but not actionable, 8+ = rare and actionable.",
		"Every BUY or SELL must state its role in the portfolio",
		"Do not buy something just because the chart looks good.",
		"Prefer the highest risk-adjusted return, not the highest return. If equity risk is elevated (high VIX, SPY below its 200d MA, inverted curve), lean toward shorter-duration bonds and cash",
		"Do not add a position highly correlated with what is already held unless it replaces it",
		"Bonds are not free: in a rising-rate regime long duration loses.",
		`"catalyst":`, `"role":`, `"portfolio_rationale":`, `"replaces":`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestPromptIncludesPortfolioBlockAndBondRisk(t *testing.T) {
	ind := &indicators.Result{Price: 90, ATR: 1}
	block := "## PORTFOLIO\nMacro regime: risk-off\nVIX: 27.3\n"
	p := BuildPrompt(PromptInput{Symbol: "SHY", Indicators: ind, Portfolio: block, BondETF: true, StopLoss: 87})
	if !strings.Contains(p, "VIX: 27.3") || !strings.Contains(p, "wider ATR stop) with no take-profit") || strings.Contains(p, "take_profit must be >=") {
		t.Errorf("bond ETF prompt wrong:\n%s", p)
	}
	p = BuildPrompt(PromptInput{Symbol: "SGOV", Indicators: ind, BondETF: true, CashLike: true, StopLoss: 87})
	if !strings.Contains(p, "no stop-loss or take-profit is attached") {
		t.Error("cash-like ETF must say no bracket")
	}
	if p = BuildPrompt(PromptInput{Symbol: "AAPL", Indicators: ind}); strings.Contains(p, "## PORTFOLIO") {
		t.Error("no portfolio block was supplied")
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

func TestParseDecisionReadsPortfolioFields(t *testing.T) {
	d, err := parseDecision(`{"action":"buy","conviction":8,"role":" Growth ","thesis":"t","reasoning":"r","catalyst":"ACME wins contract","portfolio_rationale":"adds uncorrelated growth","replaces":" msft ","take_profit":0,"suggested_tickers":[]}`)
	if err != nil || d.Action != "BUY" || d.Catalyst != "ACME wins contract" || d.Role != "growth" || d.Rationale != "adds uncorrelated growth" || d.Replaces != "MSFT" {
		t.Fatalf("got %+v, %v", d, err)
	}
	if !ValidRole(d.Role) || ValidRole("moonshot") || ValidRole("") {
		t.Error("role validation wrong")
	}
	if d, _ = parseDecision(`{"action":"HOLD","conviction":5}`); d.Catalyst != "" {
		t.Errorf("missing catalyst must be empty, got %q", d.Catalyst)
	}
}

func TestReviewPromptAndParse(t *testing.T) {
	p := BuildReviewPrompt(ReviewInput{
		Portfolio: "## PORTFOLIO\nMacro regime: neutral\n", Reason: "weekly review",
		Bonds: []BondOption{{Symbol: "SGOV", Kind: "cash-like", Duration: 0.1, Credit: "none", CashLike: true}, {Symbol: "TLT", Kind: "long Treasury", Duration: 16.5, Credit: "none"}},
		Held:  []string{"AAPL", "IEF"},
	})
	for _, want := range []string{
		"at most 2 actions", "Cash inside its band is NOT a problem and must not trigger buying", "parking it in SGOV, BIL or SHY",
		"Macro regime: neutral", "- SGOV: cash-like | duration 0.1y | credit none | cash-like", "- TLT: long Treasury | duration 16.5y", "AAPL, IEF", `"targets"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("review prompt missing %q", want)
		}
	}
	r, err := parseReview("```json\n" + `{"targets":{"equities":55,"bonds":30,"cash":15},"actions":[{"action":"buy","symbol":" sgov ","conviction":8,"role":"Cash_Parking","catalyst":"cash 41.0%","portfolio_rationale":"park","replaces":""}],"summary":"s"}` + "\n```")
	if err != nil || r.Targets.Equities == nil || *r.Targets.Equities != 55 || len(r.Actions) != 1 {
		t.Fatalf("got %+v, %v", r, err)
	}
	if a := r.Actions[0]; a.Action != "BUY" || a.Symbol != "SGOV" || a.Role != "cash_parking" {
		t.Errorf("action not normalised: %+v", a)
	}
	if r, _ = parseReview(`{"actions":[]}`); r.Targets.Equities != nil {
		t.Error("missing targets must stay nil")
	}
	if _, err = parseReview("not json"); err == nil {
		t.Error("garbage must fail")
	}
}
