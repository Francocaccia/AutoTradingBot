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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultBaseURL is the v1beta API: structured output (responseSchema) and
	// the model list live there.
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	maxRetries     = 3

	// DefaultModel is the primary model (GEMINI_MODEL overrides it).
	DefaultModel = "gemini-2.5-flash"
	// DefaultFallbackModel takes over when the primary is unavailable or its
	// daily quota is spent (GEMINI_FALLBACK_MODEL overrides it). Google retired
	// gemini-2.5-flash-lite for new users and named this as the replacement.
	DefaultFallbackModel = "gemini-3.5-flash-lite"

	DefaultMaxRPM = 8
	// DefaultDailyBudget is the per-model daily call budget used until a 429
	// reports the real per-day limit. gemini-2.5-flash's free tier allows 20.
	DefaultDailyBudget = 18
	maxRetryAfter      = 2 * time.Minute
)

var (
	// ErrQuotaExhausted means Gemini returned a daily-quota 429 for every
	// usable model (or the client is still paused from one). Never retried.
	ErrQuotaExhausted = errors.New("gemini daily quota exhausted")
	// ErrBudgetExhausted means the local per-model daily budget (for the
	// caller's priority) was used up. This is a local limit, not a Google quota.
	ErrBudgetExhausted = errors.New("gemini daily call budget exhausted")
	// ErrNoModel means no configured model is usable: each was rejected with
	// 404/400/403 or is missing from the models list.
	ErrNoModel = errors.New("no usable gemini model")

	// errModelUnavailable is a callOnce result: the model answered 404/400/403
	// and was dropped from the chain. Internal: generate falls through.
	errModelUnavailable = errors.New("gemini model unavailable")
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

// prioShare is the fraction of a model's daily budget below which a call of
// that priority may still start: candidates stop at 50%, core at 75%, the
// review at 90%, held positions only at 100%.
var prioShare = map[Priority]float64{PrioCandidate: 0.5, PrioCore: 0.75, PrioReview: 0.9, PrioHeld: 1}

// retryDelays are the waits before retry 1, 2 and 3.
var retryDelays = [maxRetries]time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

type Client struct {
	apiKey  string
	models  []string // the chain, in order: primary first
	baseURL string
	http    *http.Client
	sleep   func(time.Duration) // overridden in tests
	now     func() time.Time    // overridden in tests

	mu          sync.Mutex
	maxRPM      int
	dailyBudget int             // per-model default until a 429 reports the real limit
	window      []time.Time     // request times within the last minute
	unavailable map[string]bool // models that answered 404/400/403: out for the process
	dayKey      string          // America/Los_Angeles date the counters belong to
	used        map[string]int  // successful (HTTP 200) calls today per model; in memory, resets on restart
	limits      map[string]int  // per-day limit a 429 reported for a model today
	modelOut    map[string]bool // models whose daily quota 429 was seen today
}

// NewClient builds a Gemini client with the chain model -> fallbackModel. An
// empty model means DefaultModel; an empty fallbackModel (or one equal to
// model) disables the fallback.
func NewClient(apiKey, model, fallbackModel string) *Client {
	if model == "" {
		model = DefaultModel
	}
	chain := []string{model}
	if fallbackModel != "" && fallbackModel != model {
		chain = append(chain, fallbackModel)
	}
	return &Client{
		apiKey:      apiKey,
		models:      chain,
		baseURL:     defaultBaseURL,
		http:        &http.Client{Timeout: 90 * time.Second},
		sleep:       time.Sleep,
		now:         time.Now,
		maxRPM:      DefaultMaxRPM,
		dailyBudget: DefaultDailyBudget,
		unavailable: map[string]bool{},
		used:        map[string]int{},
		limits:      map[string]int{},
		modelOut:    map[string]bool{},
	}
}

// SetLimits sets the global requests-per-minute cap and the per-model daily
// call budget. Non-positive values keep the current setting.
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

// rollDay resets the daily counters, learned limits and per-model quota flags
// once the LA date changes. Models marked unavailable stay so. Caller holds c.mu.
func (c *Client) rollDay(now time.Time) {
	key := now.In(quotaLoc()).Format("2006-01-02")
	if key != c.dayKey {
		c.dayKey = key
		c.used, c.limits, c.modelOut = map[string]int{}, map[string]int{}, map[string]bool{}
	}
}

// limitFor is the daily call limit for model: what a 429 reported today, else
// the configured budget. Caller holds c.mu.
func (c *Client) limitFor(model string) int {
	if l := c.limits[model]; l > 0 {
		return l
	}
	return c.dailyBudget
}

// budgetLimit is how many successful calls model may have made today for a
// call of priority p to still start. Caller holds c.mu.
func (c *Client) budgetLimit(model string, p Priority) int {
	share, ok := prioShare[p]
	if !ok {
		share = 1
	}
	return int(math.Ceil(float64(c.limitFor(model)) * share))
}

// usableLocked reports whether model can take a call right now (any priority
// still has to pass budgetLimit). Caller holds c.mu.
func (c *Client) usableLocked(model string) bool {
	return !c.unavailable[model] && !c.modelOut[model] && c.used[model] < c.limitFor(model)
}

// activeLocked is the first usable model in the chain, "" if none. Caller holds c.mu.
func (c *Client) activeLocked() string {
	for _, m := range c.models {
		if c.usableLocked(m) {
			return m
		}
	}
	return ""
}

// ActiveModel returns the model new calls go to right now ("" if none is usable).
func (c *Client) ActiveModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	return c.activeLocked()
}

// Chain returns the configured models that are still usable in the process
// (not rejected with 404/400/403), in call order.
func (c *Client) Chain() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.models {
		if !c.unavailable[m] {
			out = append(out, m)
		}
	}
	return out
}

// Paused reports whether Gemini calls must be skipped until the next reset:
// no model is usable, every model's daily quota 429 was hit, or the local
// budget is spent. The reason is empty when not paused.
func (c *Client) Paused() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	if c.activeLocked() != "" {
		return false, ""
	}
	avail, out := 0, 0
	for _, m := range c.models {
		if c.unavailable[m] {
			continue
		}
		avail++
		if c.modelOut[m] {
			out++
		}
	}
	switch {
	case avail == 0:
		return true, "no usable model"
	case out == avail:
		return true, "daily quota exhausted"
	}
	return true, "daily call budget used"
}

// ModelUsage is one model's state for today.
type ModelUsage struct {
	Model       string
	Used, Limit int  // successful calls today, and the per-day limit in force
	Unavailable bool // rejected with 404/400/403 or missing from the models list
	Out         bool // a daily-quota 429 was seen today
}

func (u ModelUsage) String() string {
	s := fmt.Sprintf("%s %d/%d", u.Model, u.Used, u.Limit)
	switch {
	case u.Unavailable:
		s += " (unavailable)"
	case u.Out:
		s += " (quota out)"
	}
	return s
}

// UsageByModel lists the chain with each model's calls today. The counts are
// in-memory only: they reset when the process restarts, so the 429 responses,
// not these numbers, are the source of truth for the real quota.
func (c *Client) UsageByModel() []ModelUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	out := make([]ModelUsage, 0, len(c.models))
	for _, m := range c.models {
		out = append(out, ModelUsage{Model: m, Used: c.used[m], Limit: c.limitFor(m), Unavailable: c.unavailable[m], Out: c.modelOut[m]})
	}
	return out
}

// UsageSummary is UsageByModel on one line.
func (c *Client) UsageSummary() string {
	var parts []string
	for _, u := range c.UsageByModel() {
		parts = append(parts, u.String())
	}
	return strings.Join(parts, ", ")
}

// Usage returns the successful calls today and the daily limit of the model
// new calls go to (the first usable one, else the first available).
func (c *Client) Usage() (used, budget int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	m := c.activeLocked()
	if m == "" {
		for _, cand := range c.models {
			if !c.unavailable[cand] {
				m = cand
				break
			}
		}
	}
	if m == "" {
		m = c.models[0]
	}
	return c.used[m], c.limitFor(m)
}

// Remaining is the number of calls left today across every usable model.
func (c *Client) Remaining() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	n := 0
	for _, m := range c.models {
		if !c.unavailable[m] && !c.modelOut[m] {
			n += max(c.limitFor(m)-c.used[m], 0)
		}
	}
	return n
}

// pick chooses the model for a call of priority prio: the first one in the
// chain that is available, has quota, has budget for prio, and is not in skip.
// With none, the error says why (ErrNoModel, ErrQuotaExhausted or ErrBudgetExhausted).
func (c *Client) pick(prio Priority, skip map[string]bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	var budgetErr error
	quota, avail := false, false
	for _, m := range c.models {
		if c.unavailable[m] || skip[m] {
			continue
		}
		avail = true
		switch {
		case c.modelOut[m]:
			quota = true
		case c.used[m] >= c.budgetLimit(m, prio):
			if budgetErr == nil {
				budgetErr = fmt.Errorf("%w: %s has %d/%d calls used today, %s priority stops at %d", ErrBudgetExhausted, m, c.used[m], c.limitFor(m), prio, c.budgetLimit(m, prio))
			}
		default:
			return m, nil
		}
	}
	switch {
	case budgetErr != nil:
		return "", budgetErr
	case quota:
		return "", ErrQuotaExhausted
	case !avail:
		return "", ErrNoModel
	}
	return "", ErrQuotaExhausted
}

// waitRPM blocks until the per-minute window has room, then reserves a slot.
// Every HTTP request, retries included, goes through it.
func (c *Client) waitRPM() {
	for {
		c.mu.Lock()
		now := c.now()
		cut := now.Add(-time.Minute)
		i := 0
		for i < len(c.window) && !c.window[i].After(cut) {
			i++
		}
		c.window = c.window[i:]
		if len(c.window) < c.maxRPM {
			c.window = append(c.window, now)
			c.mu.Unlock()
			return
		}
		wait := c.window[0].Add(time.Minute).Sub(now)
		c.mu.Unlock()
		c.sleep(wait + 50*time.Millisecond)
	}
}

// countCall records one successful (HTTP 200) generate call for model. Only
// these count against the daily budget: 404s, 429s and 5xx do not.
func (c *Client) countCall(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDay(c.now())
	c.used[model]++
}

// dropModel takes model out of the chain for the rest of the process.
func (c *Client) dropModel(model, why string) {
	c.mu.Lock()
	first := !c.unavailable[model]
	c.unavailable[model] = true
	c.mu.Unlock()
	if first {
		log.Printf("[Gemini] WARN: model %s is unavailable (%s); dropped from the chain for this process", model, why)
	}
}

// markModelOut records a daily-quota 429 for model and logs the transition
// once: a switch to the next model, or the pause until LA midnight. A per-day
// limit reported by the 429 becomes the model's limit for the rest of the day.
func (c *Client) markModelOut(model string, q quotaInfo) {
	c.mu.Lock()
	c.rollDay(c.now())
	first := !c.modelOut[model]
	c.modelOut[model] = true
	if n, err := strconv.Atoi(strings.TrimSpace(q.Limit)); err == nil && n > 0 {
		c.limits[model] = n
		c.used[model] = max(c.used[model], n) // keep the counter in step with Google's
	}
	next := c.activeLocked()
	c.mu.Unlock()
	if !first {
		return
	}
	if next == "" {
		log.Printf("[Gemini] quota: daily quota exhausted on %s (%s); no model left, pausing until 00:00 America/Los_Angeles", model, q.describe())
		return
	}
	log.Printf("[Gemini] quota: daily quota exhausted on %s (%s); switching to fallback model %s for the rest of the day", model, q.describe(), next)
}

// modelList is the GET /models response.
type modelList struct {
	Models []struct {
		Name    string   `json:"name"`
		Methods []string `json:"supportedGenerationMethods"`
	} `json:"models"`
	NextPageToken string `json:"nextPageToken"`
}

// ListModels returns the model ids (without the "models/" prefix) this key can
// call generateContent on. Listing does not consume generate quota.
func (c *Client) ListModels() (map[string]bool, error) {
	out := map[string]bool{}
	token := ""
	for page := 0; page < 20; page++ {
		u := c.baseURL + "/models?pageSize=1000"
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-goog-api-key", c.apiKey)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list models: HTTP %d: %s", resp.StatusCode, errorSummary(body))
		}
		var l modelList
		if err := json.Unmarshal(body, &l); err != nil {
			return nil, fmt.Errorf("list models: parse error: %w", err)
		}
		for _, m := range l.Models {
			generates := len(m.Methods) == 0
			for _, method := range m.Methods {
				generates = generates || method == "generateContent"
			}
			if generates {
				out[strings.TrimPrefix(m.Name, "models/")] = true
			}
		}
		if l.NextPageToken == "" {
			return out, nil
		}
		token = l.NextPageToken
	}
	return out, nil
}

// VerifyModels checks the configured chain against the models list and drops
// any model that is not there, with a WARN, instead of ever calling it. If the
// list cannot be fetched the chain is kept as configured (runtime 404 handling
// still applies).
func (c *Client) VerifyModels() {
	avail, err := c.ListModels()
	if err != nil {
		log.Printf("[Gemini] WARN: could not list models (%v); model chain %s is unverified", err, strings.Join(c.Chain(), " -> "))
		return
	}
	for _, m := range c.Chain() {
		if !avail[m] {
			c.dropModel(m, "not in the models list for this API key")
		}
	}
	if chain := c.Chain(); len(chain) == 0 {
		log.Printf("[Gemini] WARN: none of the configured models exist for this API key; no Gemini decisions will be made (set GEMINI_MODEL / GEMINI_FALLBACK_MODEL)")
	} else {
		log.Printf("[Gemini] model chain: %s", strings.Join(chain, " -> "))
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
	Contents         []geminiContent   `json:"contents"`
	GenerationConfig *generationConfig `json:"generationConfig,omitempty"`
}

type generationConfig struct {
	ResponseMimeType string `json:"responseMimeType,omitempty"`
	ResponseSchema   any    `json:"responseSchema,omitempty"`
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

// DecideAt asks Gemini for one symbol's decision; prio decides how much of the
// daily budget the call may use (see Priority). See generate for retry behavior.
func (c *Client) DecideAt(prompt string, prio Priority) (*Decision, error) {
	text, err := c.generate(prompt, prio, nil)
	if err != nil {
		return nil, err
	}
	return parseDecision(text)
}

// DecideBatch asks Gemini for a decision on every symbol in the prompt (see
// BuildBatchPrompt) in one call, with structured output enforcing a JSON
// array. Each item is parsed on its own; see BatchResult.
func (c *Client) DecideBatch(prompt string, prio Priority) ([]BatchResult, error) {
	text, err := c.generate(prompt, prio, batchSchema())
	if err != nil {
		return nil, err
	}
	return parseBatch(text)
}

// generate sends a prompt and returns the model's text, walking the model
// chain:
//   - a per-day quota 429 marks the model out for the day and moves on to the
//     next model without backoff;
//   - a 404/400/403 drops the model for the process and moves on, uncounted;
//   - transient failures (per-minute 429, 500, 503, empty response) are
//     retried up to three times with backoff (2s/5s/10s, or the server's retry
//     delay), then the next model is tried once, without retries.
//
// schema, when non-nil, is the responseSchema for structured JSON output.
func (c *Client) generate(prompt string, prio Priority, schema any) (string, error) {
	skip := map[string]bool{}
	var transient, last error
	for {
		model, err := c.pick(prio, skip)
		if err != nil {
			switch {
			case transient != nil:
				return "", transient
			case last != nil:
				return "", fmt.Errorf("%w (%v)", err, last)
			}
			return "", err
		}
		retries := maxRetries
		if len(skip) > 0 {
			log.Printf("[Gemini] trying fallback model %s", model)
			retries = 0 // a model reached by falling back is tried once
		}
		text, err := c.callWithRetry(model, prompt, schema, retries)
		var re *retryableError
		switch {
		case err == nil:
			return text, nil
		case errors.Is(err, errModelUnavailable), errors.Is(err, ErrQuotaExhausted):
			last = err // already flagged: pick moves on
		case errors.As(err, &re):
			skip[model] = true
			if transient == nil {
				transient = err
			} else {
				transient = fmt.Errorf("%w; fallback %s: %v", transient, model, err)
			}
			log.Printf("[Gemini] %s failed (%v)", model, err)
		default:
			return "", err // other 4xx, network error, bad response
		}
	}
}

// callWithRetry calls model, retrying transient failures up to retries times.
func (c *Client) callWithRetry(model, prompt string, schema any, retries int) (string, error) {
	for attempt := 0; ; attempt++ {
		text, err := c.callOnce(model, prompt, schema)
		if err == nil {
			return text, nil
		}
		var re *retryableError
		if !errors.As(err, &re) || attempt >= retries {
			return "", err
		}
		delay := retryDelays[attempt]
		if re.retryAfter > 0 {
			delay = re.retryAfter
		}
		log.Printf("[Gemini] retry %d/%d after %s on %s (waiting %s)", attempt+1, retries, re.status, model, delay)
		c.sleep(delay)
	}
}

func (c *Client) callOnce(model, prompt string, schema any) (string, error) {
	c.waitRPM()
	greq := geminiRequest{Contents: []geminiContent{{Parts: []geminiPart{{Text: prompt}}}}}
	if schema != nil {
		greq.GenerationConfig = &generationConfig{ResponseMimeType: "application/json", ResponseSchema: schema}
	}
	reqBody, _ := json.Marshal(greq)

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/models/%s:generateContent", c.baseURL, model), bytes.NewBuffer(reqBody))
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
	case http.StatusNotFound, http.StatusBadRequest, http.StatusForbidden:
		// The model is gone, retired for this key, or rejects the request: never
		// retried, never counted, and not called again by this process.
		why := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, errorSummary(body))
		c.dropModel(model, why)
		return "", fmt.Errorf("%w: %s: %s", errModelUnavailable, model, why)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gemini HTTP %d on %s: %s", resp.StatusCode, model, errorSummary(body))
	}
	c.countCall(model) // a 200: the call used quota whatever the body holds

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

// extractJSONArray is extractJSON for a top-level array.
func extractJSONArray(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)
	if i, j := strings.Index(text, "["), strings.LastIndex(text, "]"); i >= 0 && j > i {
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
	decision.normalize()
	return &decision, nil
}

// normalize canonicalizes the model's free-text enums.
func (d *Decision) normalize() {
	d.Action = strings.ToUpper(strings.TrimSpace(d.Action))
	d.Role = strings.ToLower(strings.TrimSpace(d.Role))
	d.Replaces = strings.ToUpper(strings.TrimSpace(d.Replaces))
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
