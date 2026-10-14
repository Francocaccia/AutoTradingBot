package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

// Roles a position can play in the portfolio.
var Roles = []string{"core", "growth", "income", "hedge", "cash_parking"}

// ValidRole reports whether role is one of Roles.
func ValidRole(role string) bool {
	for _, r := range Roles {
		if r == role {
			return true
		}
	}
	return false
}

type Decision struct {
	Action           string   `json:"action"`
	Conviction       int      `json:"conviction"`
	Role             string   `json:"role"` // core | growth | income | hedge | cash_parking
	Thesis           string   `json:"thesis"`
	Reasoning        string   `json:"reasoning"`
	Catalyst         string   `json:"catalyst"`            // quoted headline (stocks) or PORTFOLIO datapoint (bond ETFs)
	Rationale        string   `json:"portfolio_rationale"` // why this fits the portfolio
	Replaces         string   `json:"replaces"`            // optional symbol being rotated out
	TakeProfit       float64  `json:"take_profit"`
	SuggestedTickers []string `json:"suggested_tickers"`
}

type PromptInput struct {
	Symbol     string
	Indicators *indicators.Result
	Position   *portfolio.Position
	News       []news.Article
	// StopLoss is the stop price the bot will attach to a BUY (0 if unknown).
	StopLoss float64
	// DaysHeld is the number of trading days since the position was opened;
	// negative means unknown. Only shown when a position is held.
	DaysHeld int
	// Last is the bot's previous decision for this symbol, if remembered.
	Last *LastDecision
	// Portfolio is the rendered PORTFOLIO block (weights, risk, regime and the
	// candidate's fit). Computed in Go; empty when unavailable.
	Portfolio string
	// BondETF marks a curated bond ETF: sized by the bot toward the sleeve
	// target, with a wider stop and no take-profit (cash-like ETFs: no stop).
	BondETF  bool
	CashLike bool
}

// LastDecision is the bot's previous answer for a symbol (best effort, in memory).
type LastDecision struct {
	Action string
	Thesis string
	Date   string // YYYY-MM-DD, ET
}

// MaxPromptHeadlines is how many RECENT NEWS headlines BuildPrompt shows. The
// catalyst check must use the same list.
const MaxPromptHeadlines = 10

// goal is the portfolio-manager framing shared by every prompt.
const goal = `GOAL: you manage a small multi-asset investor portfolio (stocks, equity ETFs, bond ETFs and cash), not a stock picker. The objective is to maximize risk-adjusted return (Sharpe-like) subject to a drawdown limit, benchmarked against SPY and a 60/40 (SPY/AGG) portfolio. "Highest return with lowest risk" is a trade-off: never take extra risk just to chase return, and never trade just to be active.

`

const rules = `RULES:
- LONG-ONLY cash account. No shorting, no margin, no leverage, no options, no crypto. BUY opens or adds to a long position. SELL only closes an existing long position (the whole position); if "Current position: none", SELL is invalid, so choose BUY or HOLD.
- Swing trading: holding periods of days to weeks.
- Ground every claim in the data provided below. Do not invent prices, earnings, events, news or macro data that are not shown.
- Intraday volume is not available. Use rel_volume (last completed session vs 60-session average) only.
- Doing nothing is a valid and often the best decision. Most days have no trade worth making. Two or three trades in a week is better than ten in a day. HOLD, or staying in cash, is a position. Do not trade to be active.
- Single-stock BUY only with a catalyst quoted from the provided news (exact headline text) PLUS a multi-week swing setup with at least 2:1 reward/risk. An oversold or overbought reading alone is never enough. Do not buy short-term bounces.
- Bond ETF BUY: the "catalyst" must be ONE verbatim line copied from the PORTFOLIO block (for example "VIX: 27.3"), or the regime label. Copy it exactly. Never join several lines or phrases together: a concatenation is rejected. You must also justify the duration and credit choice with the regime data. Bonds are not free: in a rising-rate regime long duration loses.
- SELL only if the thesis is broken (stop logic, catalyst invalidated, fundamental news), not because of one red day or overbought oscillators.
- Do not reverse a position without a material change since the last action. If nothing new, HOLD.
- Conviction scale: 5 = no edge, 6-7 = interesting but not actionable, 8+ = rare and actionable. Position size for stocks is derived from conviction by the bot, so be honest.
- You are managing an investor's portfolio. Every BUY or SELL must state its role in the portfolio (core, growth, income, hedge or cash_parking) and how it changes risk. Do not buy something just because the chart looks good.
- Prefer the highest risk-adjusted return, not the highest return. If equity risk is elevated (high VIX, SPY below its 200d MA, inverted curve), lean toward shorter-duration bonds and cash rather than forcing equity exposure. Cash inside its band is not a problem.
- Do not add a position highly correlated with what is already held unless it replaces it (name the held symbol in "replaces").
- The bot sets the stop-loss from ATR and rejects stock trades with reward:risk below 2:1. Bond ETF purchases are sized by the bot toward the sleeve target, not by conviction.

`

// BuildPrompt renders the per-symbol decision prompt.
func BuildPrompt(input PromptInput) string {
	var sb strings.Builder
	sb.WriteString("You are QuantEngine, an autonomous AI paper-trading portfolio manager.\n")
	sb.WriteString(goal)
	sb.WriteString(fmt.Sprintf("Make a decision for %s based ONLY on the context below.\n\n", input.Symbol))
	sb.WriteString(rules)

	if input.Indicators != nil {
		ind := input.Indicators
		sb.WriteString("## TECHNICALS\n")
		sb.WriteString(fmt.Sprintf("Price: $%.2f | RSI: %.2f\n", ind.Price, ind.RSI))
		if ind.RelVolume > 0 {
			sb.WriteString(fmt.Sprintf("rel_volume: %.2fx (last completed session vs 60-session average)\n", ind.RelVolume))
		}
		sb.WriteString(fmt.Sprintf("ATR(14): $%.2f | Beta: %.2f | P/E: %.2f\n", ind.ATR, ind.Beta, ind.PE))
		sb.WriteString(fmt.Sprintf("Ichimoku — Tenkan: %.2f | Kijun: %.2f | SpanA: %.2f | SpanB: %.2f | Chikou: %.2f\n", ind.Ichimoku.Tenkan, ind.Ichimoku.Kijun, ind.Ichimoku.SpanA, ind.Ichimoku.SpanB, ind.Ichimoku.Chikou))
		sb.WriteString(fmt.Sprintf("MACD: %.4f | Signal: %.4f | Histogram: %.4f\n", ind.MACD.MACD, ind.MACD.Signal, ind.MACD.Histogram))
		sb.WriteString(fmt.Sprintf("Stoch Fast — K: %.2f | D: %.2f\n", ind.Stochastic.K, ind.Stochastic.D))
		sb.WriteString(fmt.Sprintf("Keltner — Upper: %.2f | Mid: %.2f | Lower: %.2f\n\n", ind.Keltner.Upper, ind.Keltner.Middle, ind.Keltner.Lower))
	}

	sb.WriteString("## CURRENT POSITION\n")
	if input.Position == nil {
		sb.WriteString("Current position: none\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("Current position: LONG %d shares | avg entry $%.2f | unrealized P&L %+.2f%%\n",
			input.Position.Qty, input.Position.AvgPrice, input.Position.PLPercent*100))
		if input.DaysHeld >= 0 {
			sb.WriteString(fmt.Sprintf("Trading days since opened: %d\n", input.DaysHeld))
		}
		sb.WriteString("\n")
	}
	if l := input.Last; l != nil && l.Action != "" {
		sb.WriteString("## YOUR LAST DECISION ON THIS SYMBOL\n")
		sb.WriteString(fmt.Sprintf("%s on %s: %s\nDo not reverse it unless something material changed.\n\n", l.Action, l.Date, l.Thesis))
	}

	if input.Portfolio != "" {
		sb.WriteString(input.Portfolio)
		sb.WriteString("\n")
	}

	if input.Indicators != nil && input.StopLoss > 0 && input.StopLoss < input.Indicators.Price {
		price := input.Indicators.Price
		risk := price - input.StopLoss
		sb.WriteString("## RISK\n")
		switch {
		case input.CashLike:
			sb.WriteString("This is a cash-like ETF: no stop-loss or take-profit is attached.\n\n")
		case input.BondETF:
			sb.WriteString(fmt.Sprintf("If you BUY, the stop-loss will be $%.2f (risk $%.2f/share, wider ATR stop) with no take-profit; the position is reviewed weekly. The bot sizes the purchase.\n\n",
				input.StopLoss, risk))
		default:
			sb.WriteString(fmt.Sprintf("If you BUY, the stop-loss will be $%.2f (risk $%.2f/share). A take_profit must be >= $%.2f (2:1) or the trade is rejected; use 0 to let the bot set it.\n\n",
				input.StopLoss, risk, price+2*risk))
		}
	}

	sb.WriteString("## RECENT NEWS\n")
	if len(input.News) == 0 {
		sb.WriteString("(none available)\n")
	}
	for i, a := range input.News {
		if i >= MaxPromptHeadlines {
			break
		}
		sb.WriteString(fmt.Sprintf("- [%s] %s\n", a.Source, a.Headline))
	}

	sb.WriteString(`
## RESPONSE FORMAT
Respond ONLY with valid JSON, no markdown, no backticks:
{
  "action": "BUY" or "SELL" or "HOLD",
  "conviction": 1-10 (integer; confidence in the chosen action),
  "role": "core" or "growth" or "income" or "hedge" or "cash_parking" (required for BUY and SELL),
  "thesis": "one sentence",
  "reasoning": "your reasoning, citing the data above",
  "catalyst": "stock: the exact headline text from RECENT NEWS that supports a BUY; bond ETF: ONE verbatim line from the PORTFOLIO block (never several joined); otherwise the word none",
  "portfolio_rationale": "one or two sentences: why this fits the portfolio and how it changes risk, not just why the asset looks good (required for BUY and SELL)",
  "replaces": "held symbol being rotated out, or empty",
  "take_profit": 0.0,
  "suggested_tickers": []
}
"suggested_tickers": optional, up to 3 other US-listed stock/ETF symbols from the news above worth evaluating next cycle. They are validated by code before anything is traded.`)
	return sb.String()
}

// BondOption is one buyable bond ETF shown to the weekly review.
type BondOption struct {
	Symbol   string
	Kind     string
	Duration float64
	Credit   string
	CashLike bool
}

// ReviewInput is the context of the weekly portfolio review.
type ReviewInput struct {
	Portfolio string // rendered PORTFOLIO block, no candidate
	Reason    string // why the review runs now
	Bonds     []BondOption
	// EquityETFs are the core equity ETFs (SPY, QQQ) the review may also buy.
	EquityETFs []string
	Held       []string
}

// BuildReviewPrompt renders the weekly review prompt.
func BuildReviewPrompt(in ReviewInput) string {
	var sb strings.Builder
	sb.WriteString("You are QuantEngine, an autonomous AI paper-trading portfolio manager, doing the periodic portfolio review.\n")
	sb.WriteString(goal)
	sb.WriteString("Review the whole portfolio below (" + in.Reason + ") and propose target sleeve weights plus at most 2 actions.\n\n")
	sb.WriteString(`RULES:
- LONG-ONLY cash account. No shorting, margin, leverage or options. SELL closes a whole existing position.
- Targets are % of equity for equities, bonds and cash and must sum to 100. They must stay inside the IPS bands shown; the bot clips or rejects anything outside.
- Doing nothing is valid and often best. Return an empty actions list unless a move clearly improves risk-adjusted return. Cash inside its band is NOT a problem and must not trigger buying. If cash is above its band maximum you may propose parking it in SGOV, BIL or SHY.
- Every action needs a role, a catalyst that is ONE verbatim line copied from the PORTFOLIO block (never several joined together) and a portfolio_rationale saying how it changes risk. When a band breach triggered this review the bot sets the catalyst itself, but the rationale is still required. Justify any duration or credit choice with the regime data.
- A sleeve far outside its band is closed gradually: each BUY is capped at 5% of equity and the bot allows only a few trades a day, so propose only today's moves and expect to continue on later days.
- If equity risk is elevated (high VIX, SPY below its 200d MA, inverted curve), lean toward shorter-duration bonds and cash rather than forcing equity exposure. Bonds are not free: in a rising-rate regime long duration loses.
- BUY only the bond ETFs listed (T-bill ETFs park cash) or the core equity ETFs listed (to raise an equity sleeve that is under its band or target); SELL only held symbols. Do not add a position highly correlated with what is held unless it replaces it.
- Prefer the highest risk-adjusted return, not the highest return.

`)
	sb.WriteString(in.Portfolio)
	sb.WriteString("\n## BOND ETFS YOU MAY BUY\n")
	for _, b := range in.Bonds {
		cash := ""
		if b.CashLike {
			cash = " | cash-like"
		}
		sb.WriteString(fmt.Sprintf("- %s: %s | duration %.1fy | credit %s%s\n", b.Symbol, b.Kind, b.Duration, b.Credit, cash))
	}
	if len(in.EquityETFs) > 0 {
		sb.WriteString("\n## CORE EQUITY ETFS YOU MAY BUY\n")
		sb.WriteString(strings.Join(in.EquityETFs, ", ") + "\n")
	}
	sb.WriteString("\n## HELD SYMBOLS\n")
	if len(in.Held) == 0 {
		sb.WriteString("none\n")
	} else {
		sb.WriteString(strings.Join(in.Held, ", ") + "\n")
	}
	sb.WriteString(`
## RESPONSE FORMAT
Respond ONLY with valid JSON, no markdown, no backticks:
{
  "targets": {"equities": 60, "bonds": 30, "cash": 10},
  "actions": [
    {"action": "BUY" or "SELL", "symbol": "SGOV", "conviction": 1-10, "role": "core" or "growth" or "income" or "hedge" or "cash_parking",
     "catalyst": "ONE verbatim line from the PORTFOLIO block", "portfolio_rationale": "one or two sentences", "replaces": ""}
  ],
  "summary": "one sentence"
}`)
	return sb.String()
}

// ReviewAction is one recommended move from the weekly review.
type ReviewAction struct {
	Action     string `json:"action"`
	Symbol     string `json:"symbol"`
	Conviction int    `json:"conviction"`
	Role       string `json:"role"`
	Catalyst   string `json:"catalyst"`
	Rationale  string `json:"portfolio_rationale"`
	Replaces   string `json:"replaces"`
}

// Review is the weekly review answer. Targets are pointers so a missing
// field is distinguishable from 0.
type Review struct {
	Targets struct {
		Equities *float64 `json:"equities"`
		Bonds    *float64 `json:"bonds"`
		Cash     *float64 `json:"cash"`
	} `json:"targets"`
	Actions []ReviewAction `json:"actions"`
	Summary string         `json:"summary"`
}

func parseReview(text string) (*Review, error) {
	text = extractJSON(text)
	var r Review
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return nil, fmt.Errorf("invalid JSON from Gemini: %s", text)
	}
	for i := range r.Actions {
		a := &r.Actions[i]
		a.Action = strings.ToUpper(strings.TrimSpace(a.Action))
		a.Symbol = strings.ToUpper(strings.TrimSpace(a.Symbol))
		a.Role = strings.ToLower(strings.TrimSpace(a.Role))
		a.Replaces = strings.ToUpper(strings.TrimSpace(a.Replaces))
	}
	return &r, nil
}

// Review asks Gemini for the weekly portfolio review (one call, same retry
// and quota handling as Decide).
func (c *Client) Review(prompt string) (*Review, error) {
	text, err := c.generate(prompt, PrioReview)
	if err != nil {
		return nil, err
	}
	return parseReview(text)
}
