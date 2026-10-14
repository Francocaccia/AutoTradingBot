package gemini

import (
	"encoding/json"
	"errors"
	"github.com/francocaccia/quantengine/internal/news"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// modelServer answers each model's generateContent from its own script (the
// last step repeats) and records the models called and the request bodies.
type modelServer struct {
	mu     sync.Mutex
	steps  map[string][]step
	calls  []string
	bodies []string
	listed string // JSON served for GET /models
}

func newModelServer(t *testing.T, steps map[string][]step) (*Client, *modelServer) {
	t.Helper()
	ms := &modelServer{steps: steps}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ms.mu.Lock()
		defer ms.mu.Unlock()
		if r.Method == http.MethodGet {
			w.Write([]byte(ms.listed))
			return
		}
		model := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/models/"), ":generateContent")
		b, _ := io.ReadAll(r.Body)
		ms.calls = append(ms.calls, model)
		ms.bodies = append(ms.bodies, string(b))
		list := ms.steps[model]
		s := step{404, `{"error":{"code":404,"message":"models/` + model + ` is not found"}}`}
		if len(list) > 0 {
			s, ms.steps[model] = list[0], list[1:]
			if len(list) == 1 {
				ms.steps[model] = list
			}
		}
		w.WriteHeader(s.status)
		w.Write([]byte(s.body))
	}))
	t.Cleanup(srv.Close)
	c := NewClient("key", "primary", "fallback")
	c.baseURL = srv.URL
	c.sleep = func(time.Duration) {}
	c.now = func() time.Time { return time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC) }
	return c, ms
}

const notFoundBody = `{"error":{"code":404,"message":"This model is no longer available to new users. Please use gemini-3.5-flash-lite."}}`

func TestNotFoundDoesNotCountRemovesModelAndFallsThrough(t *testing.T) {
	logs := captureLog(t)
	c, ms := newModelServer(t, map[string][]step{
		"primary":  {{404, notFoundBody}},
		"fallback": {{200, okBody}},
	})
	if _, err := c.Decide("p"); err != nil {
		t.Fatalf("fallback should serve the call: %v", err)
	}
	if strings.Join(ms.calls, ",") != "primary,fallback" {
		t.Fatalf("calls = %v, want primary once (no retry) then fallback", ms.calls)
	}
	byModel := map[string]ModelUsage{}
	for _, u := range c.UsageByModel() {
		byModel[u.Model] = u
	}
	if u := byModel["primary"]; u.Used != 0 || !u.Unavailable {
		t.Errorf("primary: a 404 must not count and must remove the model: %+v", u)
	}
	if u := byModel["fallback"]; u.Used != 1 {
		t.Errorf("fallback should have one counted call: %+v", u)
	}
	if got := strings.Join(c.Chain(), ","); got != "fallback" {
		t.Errorf("chain = %s", got)
	}
	if !strings.Contains(logs.String(), "WARN: model primary is unavailable") {
		t.Errorf("expected a WARN line:\n%s", logs)
	}

	// The dead model is never called again by this process, not even next day.
	c.now = func() time.Time { return time.Date(2026, 10, 14, 18, 0, 0, 0, time.UTC) }
	if _, err := c.Decide("p"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ms.calls, ",") != "primary,fallback,fallback" {
		t.Fatalf("calls = %v", ms.calls)
	}
}

func TestBadRequestAndForbiddenAlsoDropTheModel(t *testing.T) {
	for _, status := range []int{400, 403} {
		c, ms := newModelServer(t, map[string][]step{
			"primary":  {{status, `{"error":{"message":"nope"}}`}},
			"fallback": {{200, okBody}},
		})
		if _, err := c.Decide("p"); err != nil {
			t.Fatalf("HTTP %d: %v", status, err)
		}
		if strings.Join(ms.calls, ",") != "primary,fallback" || len(c.Chain()) != 1 {
			t.Errorf("HTTP %d: calls %v chain %v", status, ms.calls, c.Chain())
		}
	}
}

func TestBadAPIKeyDoesNotDropModels(t *testing.T) {
	body := `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`
	c, ms := newModelServer(t, map[string][]step{"primary": {{400, body}}, "fallback": {{400, body}}})
	if _, err := c.Decide("p"); err == nil || errors.Is(err, ErrNoModel) {
		t.Fatalf("a bad key is a plain error, not a missing model: %v", err)
	}
	if len(ms.calls) != 1 || len(c.Chain()) != 2 {
		t.Errorf("calls %v chain %v: a bad key must neither fall through nor drop models", ms.calls, c.Chain())
	}
}

func TestAllModelsUnavailableIsErrNoModel(t *testing.T) {
	c, ms := newModelServer(t, map[string][]step{}) // every model answers 404
	_, err := c.Decide("p")
	if !errors.Is(err, ErrNoModel) {
		t.Fatalf("want ErrNoModel, got %v", err)
	}
	if len(ms.calls) != 2 {
		t.Fatalf("calls = %v, want one per model", ms.calls)
	}
	if p, why := c.Paused(); !p || !strings.Contains(why, "no usable model") {
		t.Errorf("paused = %v %q", p, why)
	}
	if _, err := c.Decide("p"); !errors.Is(err, ErrNoModel) || len(ms.calls) != 2 {
		t.Fatalf("a client with no usable model must not call out: %v %v", err, ms.calls)
	}
}

func TestFallbackChainOrder(t *testing.T) {
	c, ms := newModelServer(t, map[string][]step{
		"primary":  {{200, okBody}},
		"fallback": {{200, okBody}},
	})
	c.SetLimits(100, 2)
	for i := 0; i < 4; i++ {
		if _, err := c.Decide("p"); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if got := strings.Join(ms.calls, ","); got != "primary,primary,fallback,fallback" {
		t.Fatalf("calls = %s: the primary is used until its budget is spent, then the fallback", got)
	}
	if _, err := c.Decide("p"); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("both budgets spent: %v", err)
	}
	if c.Remaining() != 0 {
		t.Errorf("remaining = %d", c.Remaining())
	}
}

func TestBudgetIsPerModel(t *testing.T) {
	c, _ := newModelServer(t, map[string][]step{
		"primary":  {{200, okBody}},
		"fallback": {{200, okBody}},
	})
	c.SetLimits(100, 3)
	for i := 0; i < 3; i++ {
		if _, err := c.Decide("p"); err != nil {
			t.Fatal(err)
		}
	}
	us := c.UsageByModel()
	if us[0].Used != 3 || us[0].Limit != 3 || us[1].Used != 0 {
		t.Fatalf("usage = %v", c.UsageSummary())
	}
	if c.ActiveModel() != "fallback" {
		t.Errorf("active model = %s", c.ActiveModel())
	}
	if c.Remaining() != 3 {
		t.Errorf("remaining = %d, want the fallback's 3", c.Remaining())
	}
	if p, _ := c.Paused(); p {
		t.Error("the fallback still has budget")
	}
}

func TestDailyQuota429SetsTheModelLimit(t *testing.T) {
	c, _ := newModelServer(t, map[string][]step{
		"primary":  {{200, okBody}, {429, quotaDetailsBody(dayID, dayMetric, "20", "34s")}},
		"fallback": {{200, okBody}},
	})
	c.SetLimits(100, 50) // hardcoded budget far above Google's real limit
	if _, err := c.Decide("p"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Decide("p"); err != nil { // 429 on primary, served by the fallback
		t.Fatal(err)
	}
	us := c.UsageByModel()
	if us[0].Limit != 20 || us[0].Used != 20 || !us[0].Out {
		t.Fatalf("primary should show 20/20 from the 429: %+v", us[0])
	}
	if us[1].Limit != 50 || us[1].Used != 1 {
		t.Fatalf("fallback keeps the configured budget: %+v", us[1])
	}
	// The learned limit lasts for the rest of the day only.
	c.now = func() time.Time { return time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC) }
	if us := c.UsageByModel(); us[0].Limit != 50 || us[0].Used != 0 || us[0].Out {
		t.Fatalf("next day primary = %+v", us[0])
	}
}

func TestVerifyModelsDropsMissingModels(t *testing.T) {
	logs := captureLog(t)
	c, ms := newModelServer(t, map[string][]step{"primary": {{200, okBody}}, "fallback": {{200, okBody}}})
	ms.listed = `{"models":[{"name":"models/fallback","supportedGenerationMethods":["generateContent"]},{"name":"models/primary","supportedGenerationMethods":["embedContent"]}]}`
	c.VerifyModels()
	// primary is listed but cannot generateContent; it counts as missing.
	if got := strings.Join(c.Chain(), ","); got != "fallback" {
		t.Fatalf("chain = %s", got)
	}
	if !strings.Contains(logs.String(), "WARN: model primary is unavailable") {
		t.Errorf("expected a WARN:\n%s", logs)
	}
	if _, err := c.Decide("p"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ms.calls, ",") != "fallback" {
		t.Fatalf("a dropped model must not be called: %v", ms.calls)
	}
}

func TestVerifyModelsKeepsChainWhenListFails(t *testing.T) {
	logs := captureLog(t)
	c, _ := newModelServer(t, nil)
	c.baseURL = "http://127.0.0.1:1" // nothing listens
	c.VerifyModels()
	if len(c.Chain()) != 2 || !strings.Contains(logs.String(), "unverified") {
		t.Fatalf("chain %v logs:\n%s", c.Chain(), logs)
	}
}

const batchOK = `[
 {"symbol":"AAPL","action":"buy","conviction":8,"role":"Growth","thesis":"t","catalyst":"c","portfolio_rationale":"r","take_profit":0},
 {"symbol":"msft","action":"HOLD","conviction":5,"role":"none","thesis":"t","catalyst":"none","portfolio_rationale":""}
]`

func batchBody(text string) string {
	b, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": text}}}}}})
	return string(b)
}

func TestDecideBatchSendsSchemaAndParsesItems(t *testing.T) {
	c, ms := newModelServer(t, map[string][]step{"primary": {{200, batchBody(batchOK)}}})
	res, err := c.DecideBatch("p", PrioHeld)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(ms.bodies[0]), &req); err != nil {
		t.Fatal(err)
	}
	gc, _ := req["generationConfig"].(map[string]any)
	if gc["responseMimeType"] != "application/json" || gc["responseSchema"] == nil {
		t.Fatalf("structured output not requested: %v", gc)
	}
	if len(res) != 2 || res[0].Symbol != "AAPL" || res[0].Decision.Action != "BUY" || res[0].Decision.Role != "growth" || res[1].Symbol != "MSFT" {
		t.Fatalf("results = %+v", res)
	}
	if used, _ := c.Usage(); used != 1 {
		t.Errorf("a batch is one call: used %d", used)
	}
}

func TestParseBatchOneInvalidItemDoesNotSpoilTheOthers(t *testing.T) {
	text := "```json\n" + `[
 {"symbol":"AAPL","action":"BUY","conviction":8,"role":"growth","thesis":"t","catalyst":"c","portfolio_rationale":"r"},
 {"symbol":"MSFT","action":"BUY","conviction":"very high","thesis":"t"},
 {"action":"HOLD","conviction":5},
 {"symbol":"NVDA","action":"SELL","conviction":7,"role":"core","thesis":"t","catalyst":"none","portfolio_rationale":"r"}
]` + "\n```"
	res, err := parseBatch(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 4 {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Err != nil || res[0].Decision.Conviction != 8 {
		t.Errorf("AAPL = %+v", res[0])
	}
	if res[1].Err == nil || res[1].Symbol != "MSFT" || res[1].Decision != nil {
		t.Errorf("MSFT should be flagged invalid but keep its symbol: %+v", res[1])
	}
	if res[2].Err == nil || res[2].Symbol != "" {
		t.Errorf("symbol-less item should be invalid: %+v", res[2])
	}
	if res[3].Err != nil || res[3].Decision.Action != "SELL" {
		t.Errorf("NVDA = %+v", res[3])
	}
	if _, err := parseBatch(`{"action":"BUY"}`); err == nil {
		t.Error("a non-array reply is a whole-batch failure")
	}
}

func TestBuildBatchPromptSharesPortfolioOnce(t *testing.T) {
	shared := "## PORTFOLIO\nVIX: 27.3\n"
	var items []PromptInput
	for _, s := range []string{"AAPL", "MSFT"} {
		in := PromptInput{Symbol: s, Portfolio: "## CANDIDATE FIT: " + s + "\n"}
		for i := 0; i < 8; i++ {
			in.News = append(in.News, newsItem("headline "+s+string(rune('a'+i))))
		}
		items = append(items, in)
	}
	p := BuildBatchPrompt(shared, items)
	if n := strings.Count(p, "## PORTFOLIO"); n != 1 {
		t.Errorf("shared PORTFOLIO block appears %d times", n)
	}
	for _, want := range []string{"# SYMBOL: AAPL", "# SYMBOL: MSFT", "CANDIDATE FIT: AAPL", "CANDIDATE FIT: MSFT", "headline AAPLe", "LONG-ONLY"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "headline AAPLf") {
		t.Errorf("only the %d most recent headlines may be shown", BatchHeadlines)
	}
}

func newsItem(headline string) news.Article { return news.Article{Source: "wire", Headline: headline} }
