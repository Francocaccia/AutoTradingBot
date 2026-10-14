package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/gemini"
)

// reviewSkipLog runs maybeReview and returns the [Review] log lines.
func reviewSkipLog(t *testing.T, s *Scheduler, cs *cycleState, day, hhmm string) []string {
	t.Helper()
	logs := captureSchedLog(t)
	s.maybeReview(cs, session(day, hhmm))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(l, "[Review]") {
			out = append(out, l)
		}
	}
	return out
}

// wantOneSkipLine checks that exactly one "not run" line was logged, with the
// reason and the cash position against its band.
func wantOneSkipLine(t *testing.T, lines []string, reason string) {
	t.Helper()
	if len(lines) != 1 {
		t.Fatalf("want exactly one [Review] line, got %d: %v", len(lines), lines)
	}
	for _, want := range []string{"[Review] not run: ", reason, "cash ", "of equity, band 5-30%"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q missing %q", lines[0], want)
		}
	}
}

func balancedCycle(s *Scheduler) *cycleState {
	cs := newCycle()
	withRegime(cs, true)
	hold(cs, "SPY", 55_000)
	hold(cs, "IEF", 15_000)
	withAccount(s, cs, 100_000, 30_000)
	return cs
}

func TestReviewSkipNotDue(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	cs := balancedCycle(s)
	lines := reviewSkipLog(t, s, cs, "2026-10-13", "10:30") // a Tuesday
	wantOneSkipLine(t, lines, "not due: not Monday")
	if !strings.Contains(lines[0], "cash 30.0% of equity") {
		t.Errorf("cash sleeve not shown: %s", lines[0])
	}
}

func TestReviewSkipBeforeTenAM(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	lines := reviewSkipLog(t, s, rebalanceCycle(s), "2026-10-12", "09:45")
	wantOneSkipLine(t, lines, "before 10:00 ET")
	if !strings.Contains(lines[0], "cash 69.0% of equity") {
		t.Errorf("cash sleeve not shown: %s", lines[0])
	}
}

func TestReviewSkipAlreadyRanToday(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	s.lastReviewDay = "2026-10-12"
	wantOneSkipLine(t, reviewSkipLog(t, s, rebalanceCycle(s), "2026-10-12", "11:00"), "already ran today (weekly review)")

	s, _ = newTestScheduler(baseConfig())
	s.lastBandReviewDay = "2026-10-13"
	wantOneSkipLine(t, reviewSkipLog(t, s, rebalanceCycle(s), "2026-10-13", "11:00"), "already ran today (band-breach review)")
}

func TestReviewSkipGeminiPaused(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	s.llm = &fakeLLM{}
	s.geminiPaused = func() (bool, string) { return true, "daily call budget used" }
	lines := reviewSkipLog(t, s, rebalanceCycle(s), "2026-10-12", "11:00")
	wantOneSkipLine(t, lines, "Gemini is paused (daily call budget used)")
	if s.lastReviewDay != "" {
		t.Error("a skipped review must not consume the day")
	}
}

func TestReviewSkipGeminiStoppedEarlierThisCycle(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	cs := rebalanceCycle(s)
	cs.stopGemini = true
	wantOneSkipLine(t, reviewSkipLog(t, s, cs, "2026-10-12", "11:00"), "quota/budget ran out earlier this cycle")
}

func TestReviewSkipTradeCapReached(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	cs := rebalanceCycle(s)
	cs.tradesDay = 2 // MaxTradesPerDay in baseConfig
	lines := reviewSkipLog(t, s, cs, "2026-10-12", "11:00")
	wantOneSkipLine(t, lines, "trade cap is reached (today 2/2")
	if s.lastReviewDay != "" {
		t.Error("a skipped review must not consume the day")
	}

	// A circuit-breaker cycle is exempt from the caps, so the review is not held back by them.
	s, _ = newTestScheduler(baseConfig())
	cs = rebalanceCycle(s)
	cs.tradesDay, cs.breaker = 2, true
	for _, l := range reviewSkipLog(t, s, cs, "2026-10-12", "11:00") {
		if strings.Contains(l, "trade cap") {
			t.Errorf("breaker cycle skipped for the cap: %s", l)
		}
	}
}

func TestReviewSkipNoBudgetWhenTheCallIsRefused(t *testing.T) {
	// The client itself refuses the call (budget for the review priority is
	// spent) although it is not paused for every priority.
	s, _ := newTestScheduler(baseConfig())
	cs := rebalanceCycle(s)
	s.llm = &fakeLLM{reviewErr: fmt.Errorf("%w: m has 17/18 calls used today, review priority stops at 17", gemini.ErrBudgetExhausted)}
	s.geminiPaused = func() (bool, string) { return false, "" }
	lines := reviewSkipLog(t, s, cs, "2026-10-12", "11:00")
	wantOneSkipLine(t, lines, "no Gemini budget")
	if s.lastReviewDay != "" || !cs.stopGemini {
		t.Errorf("the day must stay free for a later cycle (lastReviewDay=%q) and the cycle must stop Gemini (%v)", s.lastReviewDay, cs.stopGemini)
	}
}

func TestReviewRunDoesNotLogANotRunLine(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	f := &fakeLLM{reviewErr: errors.New("gemini HTTP 503: overloaded")}
	s.llm = f
	s.geminiPaused = func() (bool, string) { return false, "" }
	cs := rebalanceCycle(s)
	logs := captureSchedLog(t)
	s.maybeReview(cs, session("2026-10-12", "11:00"))
	if f.reviews != 1 {
		t.Fatalf("the review should have been called once, got %d", f.reviews)
	}
	if strings.Contains(logs.String(), "not run") || !strings.Contains(logs.String(), "[Review] weekly review: Gemini error") {
		t.Errorf("a review that ran logs its own outcome, not a skip line:\n%s", logs)
	}
	if s.lastReviewDay != "2026-10-12" {
		t.Errorf("the weekly review should be marked as run, got %q", s.lastReviewDay)
	}
}

func TestReviewSkipLineWithoutAccountOmitsCash(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	s.acct = nil
	logs := captureSchedLog(t)
	s.logEarlySkip("market closed") // no account source configured
	if !strings.Contains(logs.String(), "[Review] not run: market closed") || strings.Contains(logs.String(), "cash") {
		t.Errorf("log = %q", logs.String())
	}
	// With an account the cash and band are shown.
	s, _ = newTestScheduler(baseConfig())
	logs = captureSchedLog(t)
	s.logEarlySkip("Gemini paused (daily quota exhausted)") // newTestScheduler's account: 100% cash
	if !strings.Contains(logs.String(), "cash 100.0% of equity, band 5-30%") {
		t.Errorf("log = %q", logs.String())
	}
}
