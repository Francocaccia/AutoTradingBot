package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
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

	// DefaultFallbackModel takes over for the rest of the day when the primary
	// model's daily quota is spent (GEMINI_FALLBACK_MODEL overrides it).
	DefaultFallbackModel = "gemini-2.5-flash-lite"

	DefaultMaxRPM = 8
	// DefaultDailyBudget is deliberately small: the free tier allows only a few
	// requests per day per model.
	DefaultDailyBudget = 18
	maxRetryAfter      = 2 * time.Minute
)

var (
	// ErrQuotaExhausted means Gemini returned a daily-quota 429 for every
	// usable model (or the client is still paused from one). Never retried.
	ErrQuotaExhausted = errors.New("gemini daily quota exhausted")
	// ErrBudgetExhausted means the local GEMINI_DAILY_BUDGET (for the caller's
	// priority) was used up. This is a local limit, not a Google quota.
	ErrBudgetExhausted = errors.New("gemini daily call budget exhausted")
)

// Priority orders callers when the daily budget is limited. Lower priorities
// stop earlier, so the budget that remains is kept for the higher ones.
type Priority int

const (
	PrioCandidate Priority = iota // screener candidates
	PrioCore                      // core watchlist symbols
	PrioReview                    // weekly / band-breach portfolio review
	PrioHeld                      // held positions
)

func (p Priority) String() string {
	switch p {
	case PrioHeld:
		return "held"
	case PrioReview:
		return "review"
	case PrioCore:
		return "core"
	}
	return "candidate"
}

// prioShare is the fraction of the daily budget below which a call of that
// priority may still start: candidates stop at 50%, core at 75%, the review at
// 90%, held positions only at 100%.
var prioShare = map[Priority]float64{PrioCandidate: 0.5, PrioCore: 0.75, PrioReview: 0.9, PrioHeld: 1}

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
	window      []time.Time     // request times within the last minute
	dayKey      string          // America/Los_Angeles date the counters belong to
	used        int             // requests sent today (retries included); in memory, resets on restart
	modelOut    map[string]bool // models whose daily quota 429 was seen today
}

// NewClient builds a Gemini client. fallbackModel (e.g. gemini-2.5-flash-lite)
// takes over for the rest of the day when the primary's daily quota is spent,
// and is tried once after the primary exhausts its retries; pass "" to
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
		modelOut:      map[string]bool{},
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

// rollDay resets the daily counters and the per-model quota flags once the LA
// date changes. Caller holds c.mu.
func (c *Client) rollDay(now time.Time) {
	key := now.In(quotaLoc()).Format("2006-01-02")
	if key != c.dayKey {
		c.dayKey, c.used, c.modelOut = key, 0, map[string]bool{}
	}
}

// hasFallback reports whether a distinct fallback model is configured.
func (c *Client) hasFallback() bool { return c.fallbackModel != "" && c.fallbackModel != c.model }

// allOutLocked reports whether no model has quota left today. Caller holds c.mu.
func (c *Client) allOutLocked() bool {
	return c.modelOut[c.model] && (!c.hasFallback() || c.modelOut[c.fallbackModel])
}

// activeModelLocked is the model for new calls: the primary, or the fallback
// once the primary's daily quota is spent. Caller holds c.mu.
func (c *Client) activeModelLocked() string {
	if c.modelOut[c.model] && c.hasFallback() && !c.modelOut[c.fallbackModel] {
		return c.fallbackModel
	}
	return c.model
}

// ActiveModel returns the model new calls go to right now.
func (c *Client) ActiveModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	return c.activeModelLocked()
}

// Paused reports whether Gemini calls must be skipped until the next reset:
// every model's daily quota 429 was hit, or the local budget is spent. The
// reason is empty when not paused.
func (c *Client) Paused() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	switch {
	case c.allOutLocked():
		return true, "daily quota exhausted"
	case c.used >= c.dailyBudget:
		return true, "daily call budget used"
	}
	return false, ""
}

// Usage returns the requests sent today and the daily budget. The count is
// in-memory only: it resets when the process restarts, so the 429 responses,
// not this number, are the source of truth for the real quota.
func (c *Client) Usage() (used, budget int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	return c.used, c.dailyBudget
}

// budgetLimit is how many requests may have been sent today for a call of
// priority p to still start.
func (c *Client) budgetLimit(p Priority) int {
	share, ok := prioShare[p]
	if !ok {
		share = 1
	}
	return int(math.Ceil(float64(c.dailyBudget) * share))
}

// acquire reserves one request: it enforces the daily budget for prio and
// blocks until the per-minute window has room. Every HTTP request, retries
// included, goes through it.
func (c *Client) acquire(prio Priority) error {
	for {
		c.mu.Lock()
		now := c.now()
		c.rollDay(now)
		if c.allOutLocked() {
			c.mu.Unlock()
			return ErrQuotaExhausted
		}
		if limit := c.budgetLimit(prio); c.used >= limit {
			used, budget := c.used, c.dailyBudget
			c.mu.Unlock()
			return fmt.Errorf("%w: %d/%d calls used today, %s priority stops at %d", ErrBudgetExhausted, used, budget, prio, limit)
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

// markModelOut records a daily-quota 429 for model and logs the transition
// once: a switch to the fallback model, or the pause until LA midnight.
func (c *Client) markModelOut(model string, q quotaInfo) {
	c.mu.Lock()
	first := !c.modelOut[model]
	c.modelOut[model] = true
	allOut := c.allOutLocked()
	next := c.activeModelLocked()
	c.mu.Unlock()
	if !first {
		return
	}
	switch {
	case allOut:
		log.Printf("[Gemini] quota: daily quota exhausted on %s (%s); no model left, pausing until 00:00 America/Los_Angeles", model, q.describe())
	case next != model:
		log.Printf("[Gemini] quota: daily quota exhausted on %s (%s); switching to fallback model %s for the rest of the day", model, q.describe(), next)
	default:
		log.Printf("[Gemini] quota: daily quota exhausted on %s (%s)", model, q.describe())
	}
}

// quotaInfo is what a 429 body says about the exhausted quota.
type quotaInfo struct {
	Metric, ID, Limit string
	Daily, PerMinute  bool          // scope; both false only before classification
	RetryDelay        time.Duration // from RetryInfo; 0 if absent
	Message           string        // short error.message, never the raw body
}

// describe is the one clean log fragment for the quota: metric, id, limit, scope.
func (q quotaInfo) describe() string {
	scope := "scope unknown"
	switch {
	case q.Daily:
		scope = "per-day"
	case q.PerMinute:
		scope = "per-minute"
	}
	var parts []string
	if q.Metric != "" {
		parts = append(parts, "metric="+q.Metric)
	}
	if q.ID != "" {
		parts = append(parts, "id="+q.ID)
	}
	if q.Limit != "" {
		parts = append(parts, "limit="+q.Limit)
	}
	if len(parts) == 0 && q.Message != "" {
		parts = append(parts, q.Message)
	}
	parts = append(parts, scope)
	return strings.Join(parts, " ")
}

// apiErrorBody is the part of a Google API error response the client reads.
type apiErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Details []struct {
			Type       string `json:"@type"`
			RetryDelay string `json:"retryDelay"`
			Violations []struct {
				QuotaMetric string `json:"quotaMetric"`
				QuotaID     string `json:"quotaId"`
				QuotaValue  any    `json:"quotaValue"`
			} `json:"violations"`
		} `json:"details"`
	} `json:"error"`
}

// parseQuota429 reads a 429 body: QuotaFailure violations (metric, id, limit)
// and RetryInfo. A per-day id wins over a per-minute one. A body without
// recognizable violations falls back to the old text heuristic.
func parseQuota429(body []byte) quotaInfo {
	var q quotaInfo
	var e apiErrorBody
	_ = json.Unmarshal(body, &e) // not JSON: q stays empty, the heuristic below decides
	q.Message = truncate(strings.Join(strings.Fields(e.Error.Message), " "), 120)
	var anyDay, anyMinute, picked bool
	for _, d := range e.Error.Details {
		if q.RetryDelay == 0 && d.RetryDelay != "" {
			if dur, err := time.ParseDuration(strings.TrimSpace(d.RetryDelay)); err == nil && dur > 0 {
				q.RetryDelay = min(dur, maxRetryAfter)
			}
		}
		for _, v := range d.Violations {
			key := strings.ToLower(v.QuotaID + " " + v.QuotaMetric)
			day := strings.Contains(key, "perday") || strings.Contains(key, "per_day") || strings.Contains(key, "per day") || strings.Contains(key, "daily")
			minute := strings.Contains(key, "perminute") || strings.Contains(key, "per_minute") || strings.Contains(key, "per minute")
			// Report the first violation, but prefer a per-day one over it.
			if !picked || (day && !anyDay) {
				q.Metric, q.ID, q.Limit = v.QuotaMetric, v.QuotaID, quotaValueString(v.QuotaValue)
				picked = true
			}
			anyDay, anyMinute = anyDay || day, anyMinute || minute
		}
	}
	switch {
	case anyDay:
		q.Daily = true
	case anyMinute:
		q.PerMinute = true
	default:
		q.Daily = isDailyQuota(string(body))
		q.PerMinute = !q.Daily
	}
	return q
}

func quotaValueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// isDailyQuota is the fallback classifier for a 429 without structured
// details: the body mentions "quota" without a per-minute marker.
func isDailyQuota(body string) bool {
	b := strings.ToLower(body)
	if !strings.Contains(b, "quota") {
		return false
	}
	return !strings.Contains(b, "perminute") && !strings.Contains(b, "per minute")
}

// errorSummary is a short, clean description of a failed response: Google's
// error.message when the body has one, never the raw JSON.
func errorSummary(body []byte) string {
	var e apiErrorBody
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return truncate(strings.Join(strings.Fields(e.Error.Message), " "), 160)
	}
	return fmt.Sprintf("%d-byte response body", len(body))
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
	retryAfter time.Duration // RetryInfo / Retry-After; 0 if absent
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// Decide asks Gemini for a decision at the highest priority (see DecideAt).
func (c *Client) Decide(prompt string) (*Decision, error) { return c.DecideAt(prompt, PrioHeld) }

// DecideAt asks Gemini for a decision; prio decides how much of the daily
// budget the call may use (see Priority). See generate for retry behavior.
func (c *Client) DecideAt(prompt string, prio Priority) (*Decision, error) {
	text, err := c.generate(prompt, prio)
	if err != nil {
		return nil, err
	}
	return parseDecision(text)
}

// generate sends a prompt and returns the model's text. A per-day quota 429
// switches to the fallback model (if any and not spent) without counting as a
// retry. Transient failures (per-minute 429, 500, 503, empty response) are
// retried up to three times with backoff (2s/5s/10s, or the server's retry
// delay) and then the fallback model is tried once.
func (c *Client) generate(prompt string, prio Priority) (string, error) {
	var err error
	var model string
	switched := false
	for attempt := 0; ; attempt++ {
		model = c.ActiveModel()
		var text string
		text, err = c.callOnce(model, prio, prompt)
		if err == nil {
			return text, nil
		}
		if errors.Is(err, ErrQuotaExhausted) {
			if next := c.ActiveModel(); next != model && !switched {
				switched = true
				attempt-- // the switch is not a retry
				continue
			}
			return "", err
		}
		var re *retryableError
		if !errors.As(err, &re) {
			return "", err // non-retryable: budget, other 4xx, network error
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
	c.mu.Lock()
	fallbackOut := c.modelOut[c.fallbackModel]
	c.mu.Unlock()
	if c.hasFallback() && c.fallbackModel != model && !fallbackOut {
		log.Printf("[Gemini] %s failed after %d retries (%v); trying fallback model %s", model, maxRetries, err, c.fallbackModel)
		text, ferr := c.callOnce(c.fallbackModel, prio, prompt)
		if ferr == nil {
			return text, nil
		}
		return "", fmt.Errorf("%s: %v; fallback %s: %v", model, err, c.fallbackModel, ferr)
	}
	return "", err
}

func (c *Client) callOnce(model string, prio Priority, prompt string) (string, error) {
	if err := c.acquire(prio); err != nil {
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
		q := parseQuota429(body)
		if q.Daily {
			c.markModelOut(model, q)
			return "", fmt.Errorf("%w: HTTP 429 on %s (%s)", ErrQuotaExhausted, model, q.describe())
		}
		delay := q.RetryDelay
		if delay == 0 {
			delay = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		return "", &retryableError{
			status:     "HTTP 429",
			err:        fmt.Errorf("gemini HTTP 429 on %s (%s)", model, q.describe()),
			retryAfter: delay,
		}
	case http.StatusInternalServerError, http.StatusServiceUnavailable:
		return "", &retryableError{
			status: fmt.Sprintf("HTTP %d", resp.StatusCode),
			err:    fmt.Errorf("gemini HTTP %d on %s: %s", resp.StatusCode, model, errorSummary(body)),
		}
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gemini HTTP %d on %s: %s", resp.StatusCode, model, errorSummary(body))
	}

	var apiResp geminiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return "", fmt.Errorf("parse error: %w", err)
	}
	if len(apiResp.Candidates) == 0 || len(apiResp.Candidates[0].Content.Parts) == 0 {
		return "", &retryableError{
			status: "empty response",
			err:    fmt.Errorf("empty Gemini response (%d bytes)", len(body)),
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

// parseRetryAfter reads a Retry-After header in seconds, capped at maxRetryAfter.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
