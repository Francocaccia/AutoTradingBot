package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

const okBody = `{"candidates":[{"content":{"parts":[{"text":"{\"action\":\"buy\",\"conviction\":8,\"thesis\":\"t\",\"reasoning\":\"r\",\"suggested_tickers\":[\"AMD\"]}"}]}}]}`

type step struct {
	status int
	body   string
}

// newTestClient serves the scripted steps in order (repeating the last one)
// and records every request path and every backoff sleep.
func newTestClient(t *testing.T, fallback string, steps []step) (c *Client, paths *[]string, sleeps *[]time.Duration) {
	t.Helper()
	var mu sync.Mutex
	var p []string
	var sl []time.Duration
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p = append(p, r.URL.Path)
		s := steps[len(steps)-1]
		if i < len(steps) {
			s = steps[i]
		}
		i++
		w.WriteHeader(s.status)
		w.Write([]byte(s.body))
	}))
	t.Cleanup(srv.Close)
	c = NewClient("key", fallback)
	c.baseURL = srv.URL
	c.sleep = func(d time.Duration) { sl = append(sl, d) }
	return c, &p, &sl
}

func TestDecideRetriesThenSucceeds(t *testing.T) {
	c, paths, sleeps := newTestClient(t, "", []step{{429, `{}`}, {503, `{}`}, {200, okBody}})
	d, err := c.Decide("p")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != "BUY" || d.Conviction != 8 || len(d.SuggestedTickers) != 1 {
		t.Fatalf("unexpected decision %+v", d)
	}
	if len(*paths) != 3 {
		t.Fatalf("want 3 requests, got %d", len(*paths))
	}
	if want := []time.Duration{2 * time.Second, 5 * time.Second}; !equalDurations(*sleeps, want) {
		t.Fatalf("sleeps %v, want %v", *sleeps, want)
	}
}

func TestDecideRetriesEmptyResponse(t *testing.T) {
	c, paths, _ := newTestClient(t, "", []step{{200, `{"candidates":[]}`}, {200, okBody}})
	if _, err := c.Decide("p"); err != nil {
		t.Fatal(err)
	}
	if len(*paths) != 2 {
		t.Fatalf("want 2 requests, got %d", len(*paths))
	}
}

func TestDecideDoesNotRetryOther4xx(t *testing.T) {
	c, paths, sleeps := newTestClient(t, "fallback-model", []step{{400, `{"error":"bad"}`}})
	if _, err := c.Decide("p"); err == nil {
		t.Fatal("expected error")
	}
	if len(*paths) != 1 || len(*sleeps) != 0 {
		t.Fatalf("400 must not retry or fall back: %d requests, sleeps %v", len(*paths), *sleeps)
	}
}

func TestDecideExhaustsRetriesWithoutFallback(t *testing.T) {
	c, paths, sleeps := newTestClient(t, "", []step{{503, `{}`}})
	if _, err := c.Decide("p"); err == nil {
		t.Fatal("expected error")
	}
	if len(*paths) != 4 { // 1 attempt + 3 retries
		t.Fatalf("want 4 requests, got %d", len(*paths))
	}
	if want := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}; !equalDurations(*sleeps, want) {
		t.Fatalf("sleeps %v, want %v", *sleeps, want)
	}
}

func TestDecideFallsBackOnce(t *testing.T) {
	steps := []step{{503, `{}`}, {503, `{}`}, {503, `{}`}, {503, `{}`}, {200, okBody}}
	c, paths, _ := newTestClient(t, "gemini-lite", steps)
	d, err := c.Decide("p")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != "BUY" {
		t.Fatalf("unexpected decision %+v", d)
	}
	n := len(*paths)
	if n != 5 || !strings.Contains((*paths)[n-1], "gemini-lite") || !strings.Contains((*paths)[0], primaryModel) {
		t.Fatalf("unexpected request paths %v", *paths)
	}

	// A failing fallback is not retried.
	c, paths, _ = newTestClient(t, "gemini-lite", []step{{503, `{}`}})
	if _, err := c.Decide("p"); err == nil {
		t.Fatal("expected error")
	}
	if len(*paths) != 5 {
		t.Fatalf("want 4 primary + 1 fallback requests, got %d", len(*paths))
	}
}

func TestBuildPromptLongOnlyAndPosition(t *testing.T) {
	ind := &indicators.Result{Price: 100, ATR: 2}
	flat := BuildPrompt(PromptInput{Symbol: "SPY", Indicators: ind, StopLoss: 96})
	for _, want := range []string{"LONG-ONLY", "Current position: none", "conviction", "suggested_tickers", "stop-loss will be $96.00", ">= $108.00"} {
		if !strings.Contains(flat, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(flat, "If uncertain: HOLD") {
		t.Error("old HOLD bias still present")
	}
	held := BuildPrompt(PromptInput{Symbol: "AAPL", Indicators: ind,
		Position: &portfolio.Position{Qty: 10, AvgPrice: 95.5, PLPercent: 0.047}})
	if !strings.Contains(held, "LONG 10 shares | avg entry $95.50 | unrealized P&L +4.70%") {
		t.Errorf("position line wrong:\n%s", held)
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
