package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/gemini"
)

// memStore is the broker account's markers: it outlives any scheduler.
type memStore struct {
	marks    map[string]bool
	readErr  error
	writeErr error
	sets     int
	clears   int
}

func newMemStore() *memStore { return &memStore{marks: map[string]bool{}} }

func (m *memStore) HasMarker(name string) (bool, error) {
	if m.readErr != nil {
		return false, m.readErr
	}
	return m.marks[name], nil
}
func (m *memStore) SetMarker(name string) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	m.marks[name] = true
	m.sets++
	return nil
}
func (m *memStore) ClearMarker(name string) error { delete(m.marks, name); m.clears++; return nil }

// reviewProcess is a freshly started scheduler: empty memory, the shared store.
func reviewProcess(store *memStore) (*Scheduler, *fakeLLM) {
	s, _ := newTestScheduler(baseConfig())
	f := &fakeLLM{review: &gemini.Review{Summary: "nothing to do"}}
	s.llm, s.marks = f, store
	s.geminiPaused = func() (bool, string) { return false, "" }
	return s, f
}

func mondayCycle(s *Scheduler) *cycleState {
	cs := newCycle()
	withRegime(cs, true)
	withAccount(s, cs, 100_000, 30_000)
	hold(cs, "SPY", 55_000)
	hold(cs, "IEF", 15_000)
	return cs
}

func TestReviewMarkerNamesTheWeekByItsMonday(t *testing.T) {
	for day, want := range map[string]string{
		"2026-10-12 10:30": "atb-weekly-review-2026-10-12", // Monday
		"2026-10-14 15:00": "atb-weekly-review-2026-10-12", // Wednesday
		"2026-10-18 11:00": "atb-weekly-review-2026-10-12", // Sunday
		"2026-10-19 10:00": "atb-weekly-review-2026-10-19", // next Monday
	} {
		parts := strings.Fields(day)
		if got := reviewMarker(session(parts[0], parts[1]).Now); got != want {
			t.Errorf("%s: %s, want %s", day, got, want)
		}
	}
}

// A restart (a new process, memory wiped) after the weekly review ran must not
// run it again: the fact lives in the account, not in the process.
func TestRestartAfterTheWeeklyReviewDoesNotRepeatIt(t *testing.T) {
	store := newMemStore()

	first, llm1 := reviewProcess(store)
	first.maybeReview(mondayCycle(first), session("2026-10-12", "10:30"))
	if llm1.reviews != 1 || first.lastReviewDay != "2026-10-12" || !store.marks["atb-weekly-review-2026-10-12"] {
		t.Fatalf("setup: reviews %d day %q marks %v", llm1.reviews, first.lastReviewDay, store.marks)
	}

	// The service restarts: nothing in memory, the same account.
	second, llm2 := reviewProcess(store)
	if second.lastReviewDay != "" {
		t.Fatal("a fresh process has no memory of the review")
	}
	cs := mondayCycle(second)
	if r, why := second.reviewDecision(cs, session("2026-10-12", "10:45")); r != "" || !strings.Contains(why, "already ran this week") {
		t.Errorf("decision after the restart: %q (%q)", r, why)
	}
	second.maybeReview(cs, session("2026-10-12", "10:45"))
	if llm2.reviews != 0 {
		t.Errorf("the review ran again after a restart (%d call)", llm2.reviews)
	}
	if store.sets != 1 {
		t.Errorf("the marker is written once: %d", store.sets)
	}

	// The next Monday is a new week.
	if r, _ := second.reviewDecision(cs, session("2026-10-19", "10:30")); r != "weekly review" {
		t.Errorf("next Monday: %q", r)
	}
}

// Even a review that proposed nothing, with no order to show for it, is remembered.
func TestMarkerExistsEvenWhenTheReviewQueuedNoOrders(t *testing.T) {
	store := newMemStore()
	s, llm := reviewProcess(store)
	cs := mondayCycle(s)
	s.maybeReview(cs, session("2026-10-12", "10:30"))
	if llm.reviews != 1 || len(cs.pending) != 0 || !store.marks["atb-weekly-review-2026-10-12"] {
		t.Errorf("reviews %d pending %v marks %v", llm.reviews, cs.pending, store.marks)
	}
}

// If the marker cannot be read the review is not guessed to be undone.
func TestUnreadableMarkerSkipsTheReviewThisCycle(t *testing.T) {
	store := newMemStore()
	store.readErr = errors.New("alpaca down")
	s, llm := reviewProcess(store)
	cs := mondayCycle(s)
	logs := captureSchedLog(t)
	s.maybeReview(cs, session("2026-10-12", "10:30"))
	if llm.reviews != 0 || s.lastReviewDay != "" {
		t.Errorf("reviews %d day %q", llm.reviews, s.lastReviewDay)
	}
	_, why := s.reviewDecision(cs, session("2026-10-12", "10:30"))
	if !strings.Contains(why, "marker unavailable") {
		t.Errorf("reason: %q", why)
	}
	_ = logs
	// Back up the next cycle: it runs.
	store.readErr = nil
	s.maybeReview(cs, session("2026-10-12", "10:45"))
	if llm.reviews != 1 {
		t.Errorf("the review should run once Alpaca answers: %d", llm.reviews)
	}
}

// Without a durable record the review would be repeated by the next restart, so it does not run.
func TestReviewDoesNotRunIfItsMarkerCannotBeWritten(t *testing.T) {
	store := newMemStore()
	store.writeErr = errors.New("alpaca down")
	s, llm := reviewProcess(store)
	cs := mondayCycle(s)
	s.maybeReview(cs, session("2026-10-12", "10:30"))
	if llm.reviews != 0 || s.lastReviewDay != "" {
		t.Errorf("reviews %d day %q", llm.reviews, s.lastReviewDay)
	}
	if len(cs.whyNot) != 1 || !strings.Contains(cs.whyNot[0], "cannot record") {
		t.Errorf("reason: %v", cs.whyNot)
	}
}

// A review that never got its Gemini call (no budget) leaves no marker, so a later
// cycle that day can still run it.
func TestNoBudgetClearsTheMarkerSoTheReviewCanRetry(t *testing.T) {
	store := newMemStore()
	s, llm := reviewProcess(store)
	llm.reviewErr = fmt.Errorf("%w: 17/18 calls used", gemini.ErrBudgetExhausted)
	cs := mondayCycle(s)
	s.maybeReview(cs, session("2026-10-12", "10:30"))
	if store.marks["atb-weekly-review-2026-10-12"] || store.clears != 1 || s.lastReviewDay != "" {
		t.Fatalf("marks %v clears %d day %q", store.marks, store.clears, s.lastReviewDay)
	}
	llm.reviewErr = nil
	s.maybeReview(mondayCycle(s), session("2026-10-12", "11:00"))
	if llm.reviews != 2 || !store.marks["atb-weekly-review-2026-10-12"] {
		t.Errorf("retry: reviews %d marks %v", llm.reviews, store.marks)
	}
}

// A different failure (a bad reply) keeps the marker: it is not retried every cycle, nor after a restart.
func TestAFailedCallKeepsTheMarker(t *testing.T) {
	store := newMemStore()
	s, llm := reviewProcess(store)
	llm.reviewErr = errors.New("gemini HTTP 503: overloaded")
	s.maybeReview(mondayCycle(s), session("2026-10-12", "10:30"))
	if !store.marks["atb-weekly-review-2026-10-12"] || store.clears != 0 {
		t.Errorf("marks %v clears %d", store.marks, store.clears)
	}
}

// The broker layer counts from Alpaca; when it says the cap is reached a BUY is
// dropped even though this cycle's own counters have room.
func TestBrokerCapRefusalDropsTheBuyWithoutCountingIt(t *testing.T) {
	s, o := newTestScheduler(baseConfig())
	o.capErr = fmt.Errorf("%w: 5 of 5 orders already in the last 5 trading days", broker.ErrCapReached)
	cs := newCycle() // memory says 0/2 and 0/5
	withAccount(s, cs, 100_000, 100_000)
	cs.pending = []pendingDecision{buy("MSFT", 9, headline)}
	s.executeDecisions(cs)
	if len(o.placed) != 0 || cs.dropReasons["cap"] != 1 || cs.tradesDay != 0 || cs.tradesWeek != 0 || len(cs.did) != 0 {
		t.Errorf("placed %+v drops %v counters %d/%d did %v", o.placed, cs.dropReasons, cs.tradesDay, cs.tradesWeek, cs.did)
	}

	// The same for a policy tranche.
	s, o = policyScheduler("gradual")
	o.capErr = fmt.Errorf("%w: 2 of 2 orders already today", broker.ErrCapReached)
	cs = policyCycle(s, 72_000, map[string]float64{"AAPL": 28_000})
	runPolicy(s, cs)
	if len(o.placed) != 0 || len(cs.did) != 0 || cs.tradesDay != 0 || !strings.Contains(strings.Join(cs.whyNot, ";"), "trade cap reached") {
		t.Errorf("placed %+v did %v why %v", o.placed, cs.did, cs.whyNot)
	}
}
