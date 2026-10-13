package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

const (
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1/models"
	primaryModel   = "gemini-2.5-flash"
	maxRetries     = 3

	DefaultMaxRPM      = 8
	DefaultDailyBudget = 200
	maxRetryAfter      = 60 * time.Second
)

var (
	// ErrQuotaExhausted means Gemini returned a daily-quota 429 (or the
	// client is still paused from one). Never retried.
	ErrQuotaExhausted = errors.New("gemini daily quota exhausted")
	// ErrBudgetExhausted means the local GEMINI_DAILY_BUDGET was used up.
	ErrBudgetExhausted = errors.New("gemini daily call budget exhausted")
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
	now           func() time.Time    // overridden in tests

	mu          sync.Mutex
	maxRPM      int
	dailyBudget int
	window      []time.Time // request times within the last minute
	dayKey      string      // America/Los_Angeles date the counters belong to
	used        int         // requests sent today (retries included)
	exhausted   bool        // daily quota 429 seen; cleared at the next LA midnight
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
		now:           time.Now,
		maxRPM:        DefaultMaxRPM,
		dailyBudget:   DefaultDailyBudget,
	}
}

// SetLimits sets the global requests-per-minute cap and the daily call
// budget. Non-positive values keep the current setting.
func (c *Client) SetLimits(maxRPM, dailyBudget int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if maxRPM > 0 {
		c.maxRPM = maxRPM
	}
	if dailyBudget > 0 {
		c.dailyBudget = dailyBudget
	}
}

// quotaLoc is the zone in which Google resets Gemini's daily quota.
func quotaLoc() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.FixedZone("PST", -8*3600)
}

// rollDay resets the daily counters and the exhausted flag once the LA date
// changes. Caller holds c.mu.
func (c *Client) rollDay(now time.Time) {
	key := now.In(quotaLoc()).Format("2006-01-02")
	if key != c.dayKey {
		c.dayKey, c.used, c.exhausted = key, 0, false
	}
}

// Paused reports whether Gemini calls must be skipped until the next reset:
// the daily quota 429 was hit or the local budget is spent. The reason is
// empty when not paused.
func (c *Client) Paused() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	switch {
	case c.exhausted:
		return true, "daily quota exhausted"
	case c.used >= c.dailyBudget:
		return true, "daily call budget used"
	}
	return false, ""
}

// Usage returns the requests sent today and the daily budget.
func (c *Client) Usage() (used, budget int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	return c.used, c.dailyBudget
}

// acquire reserves one request: it enforces the daily budget and blocks until
// the per-minute window has room. Every HTTP request, retries included, goes
// through it.
func (c *Client) acquire() error {
	for {
		c.mu.Lock()
		now := c.now()
		c.rollDay(now)
		if c.exhausted {
			c.mu.Unlock()
			return ErrQuotaExhausted
		}
		if c.used >= c.dailyBudget {
			c.mu.Unlock()
			return ErrBudgetExhausted
		}
		cut := now.Add(-time.Minute)
		i := 0
		for i < len(c.window) && !c.window[i].After(cut) {
			i++
		}
		c.window = c.window[i:]
		if len(c.window) < c.maxRPM {
			c.window = append(c.window, now)
			c.used++
			c.mu.Unlock()
			return nil
		}
		wait := c.window[0].Add(time.Minute).Sub(now)
		c.mu.Unlock()
		c.sleep(wait + 50*time.Millisecond)
	}
}

// markExhausted sets the daily-quota flag, logging only on the transition.
func (c *Client) markExhausted() {
	c.mu.Lock()
	first := !c.exhausted
	c.exhausted = true
	c.mu.Unlock()
	if first {
		log.Printf("[Gemini] daily quota exhausted, pausing until reset (00:00 America/Los_Angeles)")
	}
}

// isDailyQuota tells a daily-limit 429 from a per-minute rate limit: the body
// mentions "quota" without a per-minute marker.
func isDailyQuota(body string) bool {
	b := strings.ToLower(body)
	if !strings.Contains(b, "quota") {
		return false
	}
	return !strings.Contains(b, "perminute") && !strings.Contains(b, "per minute")
}

type Decision struct {
	Action           string   `json:"action"`
	Conviction       int      `json:"conviction"`
	Thesis           string   `json:"thesis"`
	Reasoning        string   `json:"reasoning"`
	Catalyst         string   `json:"catalyst"` // exact RECENT NEWS headline supporting a BUY, or "none"
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

func BuildPrompt(input PromptInput) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`You are QuantEngine, an autonomous AI paper-trading bot.
Make a trading decision for %s based ONLY on the context below.

RULES:
- LONG-ONLY cash account. No shorting, no margin, no leverage, no options, no crypto. BUY opens or adds to a long position. SELL only closes an existing long position (the whole position); if "Current position: none", SELL is invalid, so choose BUY or HOLD.
- Swing trading: holding periods of days to weeks.
- Ground every claim in the indicators and news provided below. Do not invent prices, earnings, events or news that are not shown.
- Intraday volume is not available. Do not infer low volume from the current session.
- Doing nothing is a valid and often the best decision. HOLD, or staying in cash, is a position. There is no target number of trades and many sessions should end with no trades. Do not trade to be active.
- BUY only if BOTH hold: (a) a specific catalyst quoted from the RECENT NEWS section, and (b) a technical setup consistent with a multi-week swing and at least 2:1 reward/risk. An oversold or overbought reading alone is never enough. Do not buy short-term bounces.
- SELL only if the thesis is broken, the news turned against the position, or the reward/risk no longer justifies holding. Do not sell only because oscillators are overbought.
- Do not reverse a recent decision unless something material changed. If nothing new, HOLD.
- Conviction scale: 5 means no edge. 8 or more must be rare. Use 7 or below when unsure. Position size is derived from conviction by the bot, so be honest.
- The bot sets the stop-loss from ATR and rejects trades with reward:risk below 2:1.

`, input.Symbol))

	if input.Indicators != nil {
		ind := input.Indicators
		sb.WriteString("## TECHNICALS\n")
		sb.WriteString(fmt.Sprintf("Price: $%.2f | RSI: %.2f\n", ind.Price, ind.RSI))
		if ind.RelVolume > 0 {
			sb.WriteString(fmt.Sprintf("Last completed session volume vs 60-session average: %.2fx\n", ind.RelVolume))
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
  "thesis": "one sentence",
  "reasoning": "your reasoning, citing the indicators/news above",
  "catalyst": "the exact headline text from RECENT NEWS that supports a BUY, otherwise the word none",
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
	status     string
	err        error
	retryAfter time.Duration // from Retry-After; 0 if absent
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
			return nil, err // non-retryable: daily quota, other 4xx, bad JSON, network error
		}
		if attempt >= maxRetries {
			break
		}
		delay := retryDelays[attempt]
		if re.retryAfter > 0 {
			delay = re.retryAfter
		}
		log.Printf("[Gemini] retry %d/%d after %s (waiting %s)", attempt+1, maxRetries, re.status, delay)
		c.sleep(delay)
	}

	if p, _ := c.Paused(); p {
		return nil, err
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
	if err := c.acquire(); err != nil {
		return nil, err
	}
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
	case http.StatusTooManyRequests:
		if isDailyQuota(string(body)) {
			c.markExhausted()
			return nil, fmt.Errorf("%w: %s", ErrQuotaExhausted, truncate(string(body), 300))
		}
		return nil, &retryableError{
			status:     "HTTP 429",
			err:        fmt.Errorf("gemini HTTP 429: %s", truncate(string(body), 300)),
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	case http.StatusInternalServerError, http.StatusServiceUnavailable:
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

// parseRetryAfter reads a Retry-After header in seconds, capped at a minute.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
