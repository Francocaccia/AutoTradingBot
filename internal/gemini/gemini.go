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

// Decide asks Gemini for a decision (see generate for retry behavior).
func (c *Client) Decide(prompt string) (*Decision, error) {
	text, err := c.generate(prompt)
	if err != nil {
		return nil, err
	}
	return parseDecision(text)
}

// generate sends a prompt and returns the model's text, retrying transient
// failures up to three times with exponential backoff (2s/5s/10s) and then,
// if configured, trying the fallback model once.
func (c *Client) generate(prompt string) (string, error) {
	var err error
	for attempt := 0; ; attempt++ {
		var text string
		text, err = c.callOnce(c.model, prompt)
		if err == nil {
			return text, nil
		}
		var re *retryableError
		if !errors.As(err, &re) {
			return "", err // non-retryable: daily quota, other 4xx, network error
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
		return "", err
	}
	if c.fallbackModel != "" && c.fallbackModel != c.model {
		log.Printf("[Gemini] %s failed after %d retries (%v); trying fallback model %s", c.model, maxRetries, err, c.fallbackModel)
		text, ferr := c.callOnce(c.fallbackModel, prompt)
		if ferr == nil {
			return text, nil
		}
		return "", fmt.Errorf("%s: %v; fallback %s: %v", c.model, err, c.fallbackModel, ferr)
	}
	return "", err
}

func (c *Client) callOnce(model, prompt string) (string, error) {
	if err := c.acquire(); err != nil {
		return "", err
	}
	reqBody, _ := json.Marshal(geminiRequest{
		Contents: []geminiContent{{Parts: []geminiPart{{Text: prompt}}}},
	})

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/%s:generateContent", c.baseURL, model), bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		if isDailyQuota(string(body)) {
			c.markExhausted()
			return "", fmt.Errorf("%w: %s", ErrQuotaExhausted, truncate(string(body), 300))
		}
		return "", &retryableError{
			status:     "HTTP 429",
			err:        fmt.Errorf("gemini HTTP 429: %s", truncate(string(body), 300)),
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	case http.StatusInternalServerError, http.StatusServiceUnavailable:
		return "", &retryableError{
			status: fmt.Sprintf("HTTP %d", resp.StatusCode),
			err:    fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, truncate(string(body), 300)),
		}
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var apiResp geminiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return "", fmt.Errorf("parse error: %w", err)
	}
	if len(apiResp.Candidates) == 0 || len(apiResp.Candidates[0].Content.Parts) == 0 {
		return "", &retryableError{
			status: "empty response",
			err:    fmt.Errorf("empty Gemini response: %s", truncate(string(body), 300)),
		}
	}

	return apiResp.Candidates[0].Content.Parts[0].Text, nil
}

// extractJSON strips code fences and surrounding prose from a model reply.
func extractJSON(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		text = text[i : j+1]
	}
	return text
}

func parseDecision(text string) (*Decision, error) {
	text = extractJSON(text)
	var decision Decision
	if err := json.Unmarshal([]byte(text), &decision); err != nil {
		return nil, fmt.Errorf("invalid JSON from Gemini: %s", text)
	}
	decision.Action = strings.ToUpper(strings.TrimSpace(decision.Action))
	decision.Role = strings.ToLower(strings.TrimSpace(decision.Role))
	decision.Replaces = strings.ToUpper(strings.TrimSpace(decision.Replaces))
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
