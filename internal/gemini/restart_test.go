package gemini

import (
	"strings"
	"testing"
	"time"
)

// The counters live in memory, so right after a start (Render wakes the service
// fresh every morning) the day's earlier calls are unknown, not zero, until the
// first successful call or the first daily-quota 429.
func TestUsageIsUnknownUntilTheFirstCallOfTheDay(t *testing.T) {
	c := NewClient("k", "primary", "fallback")
	if c.UsageKnown() {
		t.Fatal("a fresh client must not claim to know today's usage")
	}
	if n, exact := c.RemainingEstimate(); exact || n != 2*DefaultDailyBudget {
		t.Errorf("remaining = %d exact=%v: an upper bound, never exact", n, exact)
	}
	if got := c.UsageSummary(); !strings.Contains(got, "unknown") || !strings.Contains(got, "429") {
		t.Errorf("summary must say the count is partial: %q", got)
	}

	c.countCall("primary") // the first successful call of the day
	if !c.UsageKnown() {
		t.Fatal("after the first successful call usage is treated as known")
	}
	if n, exact := c.RemainingEstimate(); !exact || n != 2*DefaultDailyBudget-1 {
		t.Errorf("remaining = %d exact=%v", n, exact)
	}
	if strings.Contains(c.UsageSummary(), "unknown") {
		t.Errorf("summary = %q", c.UsageSummary())
	}
}

func TestADailyQuota429AlsoCalibratesUsage(t *testing.T) {
	c := NewClient("k", "primary", "fallback")
	c.markModelOut("primary", quotaInfo{Limit: "20", Daily: true})
	if !c.UsageKnown() {
		t.Error("a daily-quota 429 is Google's own count: usage is known from then on")
	}
}

func TestUsageIsKnownFromTheNextQuotaDay(t *testing.T) {
	c := NewClient("k", "primary", "fallback")
	c.now = func() time.Time { return time.Now().Add(36 * time.Hour) } // two quota days later in every zone
	if !c.UsageKnown() {
		t.Fatal("after the quota day rolls over the counters cover the whole day")
	}
}

// After a restart the real limit is whatever the API says: a daily-quota 429
// takes the model out and the call moves to the fallback.
func TestRestartRelyOn429AndFallback(t *testing.T) {
	quota := `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"generativelanguage.googleapis.com/generate_content_free_tier_requests","quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier","quotaValue":"20"}]}]}}`
	c, ms := newModelServer(t, map[string][]step{
		"primary":  {{429, quota}},
		"fallback": {{200, okBody}},
	})
	c.startDay = c.now().In(quotaLoc()).Format("2006-01-02") // the process started on the fake clock's day
	if c.UsageKnown() {
		t.Fatal("setup: usage should be unknown")
	}
	if _, err := c.Decide("p"); err != nil {
		t.Fatalf("the fallback should serve the call: %v", err)
	}
	if strings.Join(ms.calls, ",") != "primary,fallback" {
		t.Errorf("calls = %v", ms.calls)
	}
	byModel := map[string]ModelUsage{}
	for _, u := range c.UsageByModel() {
		byModel[u.Model] = u
	}
	if !byModel["primary"].Out || byModel["primary"].Limit != 20 {
		t.Errorf("primary = %+v, want out with the 429's limit", byModel["primary"])
	}
}
