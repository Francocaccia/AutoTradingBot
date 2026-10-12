package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

const (
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1/models"
	primaryModel   = "gemini-2.5-flash"
	maxRetries     = 3
)

// retryDelays are the waits before retry 1, 2 and 3.
var retryDelays = [maxRetries]time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

type Client struct {
	apiKey        string
	model         string
	fallbackModel string // empty disables the fallback
	baseURL       string
	http          *http.Client
	sleep         func(time.Duration) // overridden in tests
}

// NewClient builds a Gemini client. fallbackModel (e.g. gemini-2.5-flash-lite)
// is tried once after the primary model exhausts its retries; pass "" to
// disable it.
func NewClient(apiKey, fallbackModel string) *Client {
	return &Client{
		apiKey:        apiKey,
		model:         primaryModel,
		fallbackModel: fallbackModel,
		baseURL:       defaultBaseURL,
		http:          &http.Client{Timeout: 90 * time.Second},
		sleep:         time.Sleep,
	}
}

type Decision struct {
	Action           string   `json:"action"`
	Conviction       int      `json:"conviction"`
	Thesis           string   `json:"thesis"`
	Reasoning        string   `json:"reasoning"`
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
}

func BuildPrompt(input PromptInput) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`You are QuantEngine, an autonomous AI paper-trading bot.
Make a trading decision for %s based ONLY on the context below.

RULES:
- LONG-ONLY cash account. No shorting, no margin, no leverage, no options, no crypto. BUY opens or adds to a long position. SELL only closes an existing long position (the whole position); if "Current position: none", SELL is invalid, so choose BUY or HOLD.
- Swing trading: holding periods of days to weeks.
- Ground every claim in the indicators and news provided below. Do not invent prices, earnings, events or news that are not shown.
- Rate your conviction from 1 to 10. Choose BUY when conviction >= 7 and you can point to a catalyst in the news or a clear technical setup. Position size is derived from conviction by the bot (7 = smaller, 9-10 = largest), so be honest.
- Idle cash has an opportunity cost: do not default to HOLD out of caution when a setup is present. But if conviction is below 7, choose HOLD (or SELL if you hold a position and the thesis is broken).
- The bot sets the stop-loss from ATR and rejects trades with reward:risk below 2:1.

`, input.Symbol))

	if input.Indicators != nil {
		ind := input.Indicators
		sb.WriteString("## TECHNICALS\n")
		sb.WriteString(fmt.Sprintf("Price: $%.2f | RSI: %.2f | Volume: %d | Avg daily volume: %.0f\n", ind.Price, ind.RSI, ind.Volume, ind.AvgVolume))
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
		sb.WriteString(fmt.Sprintf("Current position: LONG %d shares | avg entry $%.2f | unrealized P&L %+.2f%%\n\n",
			input.Position.Qty, input.Position.AvgPrice, input.Position.PLPercent*100))
	}

	if input.Indicators != nil && input.StopLoss > 0 && input.StopLoss < input.Indicators.Price {
		price := input.Indicators.Price
		risk := price - input.StopLoss
		sb.WriteString("## RISK\n")
		sb.WriteString(fmt.Sprintf("If you BUY, the stop-loss will be $%.2f (risk $%.2f/share). A take_profit must be >= $%.2f (2:1) or the trade is rejected; use 0 to let the bot set it.\n\n",
			input.StopLoss, risk, price+2*risk))
	}

	sb.WriteString("## RECENT NEWS\n")
	if len(input.News) == 0 {
		sb.WriteString("(none available)\n")
	}
	for i, a := range input.News {
		if i >= 10 {
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
  "thesis": "one sentence",
  "reasoning": "your reasoning, citing the indicators/news above",
  "take_profit": 0.0,
  "suggested_tickers": []
}
"suggested_tickers": optional, up to 3 other US-listed stock/ETF symbols from the news above worth evaluating next cycle. They are validated by code before anything is traded.`)
	return sb.String()
}

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// retryableError marks failures worth retrying: HTTP 429/500/503 and an
// empty response. status is the label used in the retry log line.
type retryableError struct {
	status string
	err    error
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// Decide asks Gemini for a decision, retrying transient failures up to three
// times with exponential backoff (2s/5s/10s) and then, if configured, trying
// the fallback model once.
func (c *Client) Decide(prompt string) (*Decision, error) {
	var err error
	for attempt := 0; ; attempt++ {
		var d *Decision
		d, err = c.callOnce(c.model, prompt)
		if err == nil {
			return d, nil
		}
		var re *retryableError
		if !errors.As(err, &re) {
			return nil, err // non-retryable: other 4xx, bad JSON, network error
		}
		if attempt >= maxRetries {
			break
		}
		log.Printf("[Gemini] retry %d/%d after %s", attempt+1, maxRetries, re.status)
		c.sleep(retryDelays[attempt])
	}

	if c.fallbackModel != "" && c.fallbackModel != c.model {
		log.Printf("[Gemini] %s failed after %d retries (%v); trying fallback model %s", c.model, maxRetries, err, c.fallbackModel)
		d, ferr := c.callOnce(c.fallbackModel, prompt)
		if ferr == nil {
			return d, nil
		}
		return nil, fmt.Errorf("%s: %v; fallback %s: %v", c.model, err, c.fallbackModel, ferr)
	}
	return nil, err
}

func (c *Client) callOnce(model, prompt string) (*Decision, error) {
	reqBody, _ := json.Marshal(geminiRequest{
		Contents: []geminiContent{{Parts: []geminiPart{{Text: prompt}}}},
	})

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/%s:generateContent", c.baseURL, model), bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable:
		return nil, &retryableError{
			status: fmt.Sprintf("HTTP %d", resp.StatusCode),
			err:    fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, truncate(string(body), 300)),
		}
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var apiResp geminiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	if len(apiResp.Candidates) == 0 || len(apiResp.Candidates[0].Content.Parts) == 0 {
		return nil, &retryableError{
			status: "empty response",
			err:    fmt.Errorf("empty Gemini response: %s", truncate(string(body), 300)),
		}
	}

	return parseDecision(apiResp.Candidates[0].Content.Parts[0].Text)
}

func parseDecision(text string) (*Decision, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		text = text[i : j+1]
	}

	var decision Decision
	if err := json.Unmarshal([]byte(text), &decision); err != nil {
		return nil, fmt.Errorf("invalid JSON from Gemini: %s", text)
	}
	decision.Action = strings.ToUpper(strings.TrimSpace(decision.Action))
	return &decision, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
