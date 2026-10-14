package gemini

import (
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// A realistic Gemini free-tier 429 body (QuotaFailure + RetryInfo).
func quotaDetailsBody(id, metric, value, retry string) string {
	return `{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details.","status":"RESOURCE_EXHAUSTED","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"` + metric + `","quotaId":"` + id + `","quotaDimensions":{"model":"gemini-2.5-flash"},"quotaValue":"` + value + `"}]},` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"` + retry + `"}]}}`
}

const (
	dayMetric = "generativelanguage.googleapis.com/generate_content_free_tier_requests"
	dayID     = "GenerateRequestsPerDayPerProjectPerModel-FreeTier"
	minID     = "GenerateRequestsPerMinutePerProjectPerModel-FreeTier"
)

func captureLog(t *testing.T) *strings.Builder {
	t.Helper()
	var sb strings.Builder
	log.SetOutput(&sb)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &sb
}

func TestParseQuota429(t *testing.T) {
	q := parseQuota429([]byte(quotaDetailsBody(dayID, dayMetric, "20", "34s")))
	if !q.Daily || q.PerMinute || q.ID != dayID || q.Metric != dayMetric || q.Limit != "20" || q.RetryDelay != 34*time.Second {
		t.Errorf("per-day parse = %+v", q)
	}
	q = parseQuota429([]byte(quotaDetailsBody(minID, dayMetric, "5", "12.5s")))
	if q.Daily || !q.PerMinute || q.Limit != "5" || q.RetryDelay != 12500*time.Millisecond {
		t.Errorf("per-minute parse = %+v", q)
	}
	// Both violations present: the daily one decides and is the one reported.
	both := `{"error":{"details":[{"@type":"x.QuotaFailure","violations":[{"quotaId":"` + minID + `","quotaValue":"5"},{"quotaId":"` + dayID + `","quotaValue":"20"}]}]}}`
	if q := parseQuota429([]byte(both)); !q.Daily || q.ID != dayID || q.Limit != "20" {
		t.Errorf("mixed violations = %+v", q)
	}
	// Unstructured bodies keep the old text heuristic.
	if q := parseQuota429([]byte(quotaBody)); !q.Daily {
		t.Errorf("plain quota message should read as daily: %+v", q)
	}
	if q := parseQuota429([]byte("not json at all")); q.Daily || !q.PerMinute {
		t.Errorf("garbage body = %+v", q)
	}
}

func TestPerMinute429UsesRetryInfoDelay(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "7") // RetryInfo is preferred over the header
			w.WriteHeader(429)
			w.Write([]byte(quotaDetailsBody(minID, dayMetric, "5", "34s")))
			return
		}
		w.Write([]byte(okBody))
	}))
	defer srv.Close()
	logs := captureLog(t)
	var sleeps []time.Duration
	c := NewClient("key", "", "")
	c.baseURL = srv.URL
	c.sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	if _, err := c.Decide("p"); err != nil {
		t.Fatal(err)
	}
	if !equalDurations(sleeps, []time.Duration{34 * time.Second}) {
		t.Fatalf("sleeps %v, want [34s]", sleeps)
	}
	if p, _ := c.Paused(); p {
		t.Fatal("a per-minute limit must not pause the client")
	}
	if strings.Contains(logs.String(), "{") {
		t.Errorf("raw JSON leaked into the log:\n%s", logs)
	}
}

func TestDailyQuotaLogsOneCleanLine(t *testing.T) {
	logs := captureLog(t)
	c, _, _ := newTestClient(t, "", []step{{429, quotaDetailsBody(dayID, dayMetric, "20", "34s")}})
	_, err := c.Decide("p")
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("want ErrQuotaExhausted, got %v", err)
	}
	out := logs.String() + err.Error()
	for _, want := range []string{dayMetric, dayID, "limit=20", "per-day", "00:00 America/Los_Angeles"} {
		if !strings.Contains(out, want) {
			t.Errorf("log/error missing %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "{}") || strings.Contains(out, "RESOURCE_EXHAUSTED") {
		t.Errorf("raw JSON body leaked:\n%s", out)
	}
	if n := strings.Count(logs.String(), "daily quota exhausted"); n != 1 {
		t.Errorf("want exactly one quota log line, got %d:\n%s", n, logs)
	}
}

func TestDailyQuotaSwitchesToFallbackForTheRestOfTheDay(t *testing.T) {
	body := quotaDetailsBody(dayID, dayMetric, "20", "34s")
	c, paths, sleeps := newTestClient(t, "gemini-2.5-flash-lite", []step{{429, body}, {200, okBody}})
	clock := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }

	// Primary hits its daily quota; the same call is served by the fallback, without a backoff.
	if _, err := c.Decide("p"); err != nil {
		t.Fatalf("fallback should serve the call: %v", err)
	}
	if len(*paths) != 2 || !strings.Contains((*paths)[0], DefaultModel+":") || !strings.Contains((*paths)[1], "gemini-2.5-flash-lite") || len(*sleeps) != 0 {
		t.Fatalf("paths %v sleeps %v", *paths, *sleeps)
	}
	if p, _ := c.Paused(); p {
		t.Fatal("one model out is not a pause")
	}
	if c.ActiveModel() != "gemini-2.5-flash-lite" {
		t.Fatalf("active model = %s", c.ActiveModel())
	}

	// Later calls the same day go straight to the fallback.
	if _, err := c.Decide("p"); err != nil {
		t.Fatal(err)
	}
	if n := len(*paths); n != 3 || strings.Contains((*paths)[2], DefaultModel+":") {
		t.Fatalf("primary must not be called again today: %v", *paths)
	}

	// Next LA day: back to the primary.
	clock = time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	if c.ActiveModel() != DefaultModel {
		t.Errorf("active model after reset = %s", c.ActiveModel())
	}
}

func TestPauseOnlyWhenBothModelsAreExhausted(t *testing.T) {
	body := quotaDetailsBody(dayID, dayMetric, "20", "34s")
	logs := captureLog(t)
	c, paths, _ := newTestClient(t, "gemini-2.5-flash-lite", []step{{429, body}})
	clock := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }

	_, err := c.Decide("p")
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("want ErrQuotaExhausted, got %v", err)
	}
	if len(*paths) != 2 {
		t.Fatalf("want one call per model, got %v", *paths)
	}
	if p, why := c.Paused(); !p || !strings.Contains(why, "quota") {
		t.Fatalf("both models out must pause: %v %q", p, why)
	}
	if _, err := c.Decide("p"); !errors.Is(err, ErrQuotaExhausted) || len(*paths) != 2 {
		t.Fatalf("paused client must not call out: %v %v", err, *paths)
	}
	if !strings.Contains(logs.String(), "switching to fallback model gemini-2.5-flash-lite") || !strings.Contains(logs.String(), "no model left") {
		t.Errorf("expected switch and pause log lines:\n%s", logs)
	}
	clock = time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	if p, _ := c.Paused(); p {
		t.Error("pause should clear after LA midnight")
	}
}

func TestBudgetPriorityTiersAndBudgetIsNotQuota(t *testing.T) {
	c, _, _ := newTestClient(t, "", []step{{200, okBody}})
	clock := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	c.SetLimits(1000, 10) // candidates stop at 5, core at 8, review at 9, held at 10

	call := func(p Priority) error { _, err := c.DecideAt("p", p); return err }
	for i := 0; i < 5; i++ {
		if err := call(PrioCandidate); err != nil {
			t.Fatalf("candidate call %d: %v", i+1, err)
		}
	}
	if err := call(PrioCandidate); !errors.Is(err, ErrBudgetExhausted) || errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("6th candidate call must hit the budget, not the quota: %v", err)
	}
	for i := 5; i < 8; i++ {
		if err := call(PrioCore); err != nil {
			t.Fatalf("core call %d: %v", i+1, err)
		}
	}
	if err := call(PrioCore); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("core stops at 8 calls: %v", err)
	}
	if err := call(PrioReview); err != nil { // 9th
		t.Fatalf("review still has room: %v", err)
	}
	if err := call(PrioReview); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("review stops at 9 calls: %v", err)
	}
	if err := call(PrioHeld); err != nil { // 10th
		t.Fatalf("held positions use the last call: %v", err)
	}
	if err := call(PrioHeld); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("budget fully spent: %v", err)
	}
	if p, why := c.Paused(); !p || !strings.Contains(why, "budget") || strings.Contains(why, "quota") {
		t.Errorf("paused = %v %q, want a budget reason", p, why)
	}
	if used, budget := c.Usage(); used != 10 || budget != 10 {
		t.Errorf("usage %d/%d", used, budget)
	}
}

func TestDefaultBudgetIs18(t *testing.T) {
	if DefaultDailyBudget != 18 {
		t.Errorf("DefaultDailyBudget = %d, want 18", DefaultDailyBudget)
	}
	if got := NewClient("k", "", "").dailyBudget; got != 18 {
		t.Errorf("new client budget = %d", got)
	}
}
