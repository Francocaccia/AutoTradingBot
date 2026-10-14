package scheduler

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/gemini"
	"github.com/francocaccia/quantengine/internal/news"
)

type fakeLLM struct {
	batchPrompts []string
	batchPrios   []gemini.Priority
	results      []gemini.BatchResult
	batchErr     error
	singleCalls  []string
	singleErr    error
	remaining    int
	reviews      int
	review       *gemini.Review
	reviewErr    error
}

func (f *fakeLLM) DecideBatch(prompt string, prio gemini.Priority) ([]gemini.BatchResult, error) {
	f.batchPrompts = append(f.batchPrompts, prompt)
	f.batchPrios = append(f.batchPrios, prio)
	return f.results, f.batchErr
}

func (f *fakeLLM) DecideAt(prompt string, prio gemini.Priority) (*gemini.Decision, error) {
	f.singleCalls = append(f.singleCalls, prompt)
	if f.singleErr != nil {
		return nil, f.singleErr
	}
	return &gemini.Decision{Action: "HOLD", Conviction: 5, Thesis: "single"}, nil
}

func (f *fakeLLM) Remaining() int { return f.remaining }

func (f *fakeLLM) Review(string) (*gemini.Review, error) {
	f.reviews++
	return f.review, f.reviewErr
}

func entry(sym string, prio gemini.Priority) *batchEntry {
	return &batchEntry{symbol: sym, prio: prio, ind: ind(), canBuy: true, in: evalInput{Symbol: sym}}
}

func symbols(es []*batchEntry) string {
	var out []string
	for _, e := range es {
		out = append(out, e.symbol)
	}
	return strings.Join(out, ",")
}

func TestSelectBatchPriorityOrderAndCap(t *testing.T) {
	due := []*batchEntry{
		entry("CAND1", gemini.PrioCandidate), entry("CORE1", gemini.PrioCore), entry("HELD1", gemini.PrioHeld),
		entry("CAND2", gemini.PrioCandidate), entry("HELD2", gemini.PrioHeld), entry("CORE2", gemini.PrioCore),
		entry("CAND3", gemini.PrioCandidate),
	}
	batch, deferred := selectBatch(due, 4)
	if got := symbols(batch); got != "HELD1,HELD2,CORE1,CORE2" {
		t.Errorf("batch = %s, want held, then core, in arrival order within a class", got)
	}
	if got := symbols(deferred); got != "CAND1,CAND2,CAND3" {
		t.Errorf("deferred = %s", got)
	}
	// Held positions are never pushed out by core symbols or candidates.
	batch, deferred = selectBatch(due, 1)
	if symbols(batch) != "HELD1" || len(deferred) != 6 {
		t.Errorf("cap 1: batch %s, %d deferred", symbols(batch), len(deferred))
	}
	// Below the cap nothing is deferred and the input is not reordered in place.
	batch, deferred = selectBatch(due, 12)
	if len(batch) != 7 || len(deferred) != 0 || due[0].symbol != "CAND1" {
		t.Errorf("under the cap: batch %d, deferred %d, input[0]=%s", len(batch), len(deferred), due[0].symbol)
	}
}

func TestRunBatchCapsDefersAndUsesTheHighestPriority(t *testing.T) {
	logs := captureSchedLog(t)
	s, _ := newTestScheduler(Config{BatchMaxSymbols: 2})
	f := &fakeLLM{}
	s.llm = f
	cs := newCycle()
	cs.pf = nil
	due := []*batchEntry{entry("CAND", gemini.PrioCandidate), entry("CORE", gemini.PrioCore), entry("OTHER", gemini.PrioCore)}
	for _, e := range due {
		e.articles = []news.Article{}
	}
	f.results = []gemini.BatchResult{
		{Symbol: "CORE", Decision: &gemini.Decision{Action: "HOLD", Conviction: 5}},
		{Symbol: "OTHER", Decision: &gemini.Decision{Action: "HOLD", Conviction: 5}},
	}
	s.runBatch(cs, due)
	if len(f.batchPrompts) != 1 {
		t.Fatalf("want exactly one Gemini call, got %d", len(f.batchPrompts))
	}
	if strings.Join(cs.batchSyms, ",") != "CORE,OTHER" || cs.deferred != 1 {
		t.Errorf("batch %v deferred %d", cs.batchSyms, cs.deferred)
	}
	if f.batchPrios[0] != gemini.PrioCore {
		t.Errorf("prio = %v, want the highest in the batch (core)", f.batchPrios[0])
	}
	if strings.Contains(f.batchPrompts[0], "# SYMBOL: CAND") {
		t.Error("deferred symbol leaked into the prompt")
	}
	if !strings.Contains(logs.String(), "deferred to the next cycle: CAND") {
		t.Errorf("deferral not logged:\n%s", logs)
	}
	// The deferred symbol was not recorded by the gate: it is still due next cycle.
	if _, seen := s.gate.last["CAND"]; seen {
		t.Error("a deferred symbol must not be marked as evaluated")
	}
	if _, seen := s.gate.last["CORE"]; !seen {
		t.Error("an answered symbol should be marked as evaluated")
	}
}

func TestApplyBatchOneInvalidItemDoesNotInvalidateTheOthers(t *testing.T) {
	s, _ := newTestScheduler(Config{})
	cs := newCycle()
	hold(cs, "OLD", 5_000)
	batch := []*batchEntry{entry("AAA", gemini.PrioCore), entry("BBB", gemini.PrioCore), entry("OLD", gemini.PrioHeld), entry("DDD", gemini.PrioCore)}
	batch[0].articles = []news.Article{{Headline: headline}}
	batch[2].position = cs.positions["OLD"]
	results := []gemini.BatchResult{
		{Symbol: "AAA", Decision: &gemini.Decision{Action: "BUY", Conviction: 9, Role: "growth", Catalyst: headline, Rationale: "r", Thesis: "t"}},
		{Symbol: "BBB", Err: errors.New("conviction is a string")},
		{Symbol: "OLD", Decision: &gemini.Decision{Action: "SELL", Conviction: 7, Role: "core", Rationale: "r"}},
		{Symbol: "DDD", Decision: &gemini.Decision{Action: "MAYBE", Conviction: 7}},
		{Symbol: "ZZZ", Decision: &gemini.Decision{Action: "BUY", Conviction: 9}},  // never asked about
		{Symbol: "AAA", Decision: &gemini.Decision{Action: "SELL", Conviction: 9}}, // duplicate
	}
	s.applyBatch(cs, batch, results)

	if len(cs.pending) != 2 || cs.pending[0].symbol != "AAA" || cs.pending[1].symbol != "OLD" {
		t.Fatalf("pending = %+v", cs.pending)
	}
	if cs.pending[0].decision.Action != "BUY" {
		t.Errorf("the duplicate must not replace the first AAA answer: %+v", cs.pending[0].decision)
	}
	if cs.blocked != 2 || cs.dropReasons["invalid-item"] != 1 || cs.dropReasons["invalid-action"] != 1 {
		t.Errorf("blocked %d reasons %v", cs.blocked, cs.dropReasons)
	}
	if cs.evaluated != 2 || cs.missing != 0 {
		t.Errorf("evaluated %d missing %d", cs.evaluated, cs.missing)
	}
	// The valid item still passes through the normal guards afterwards.
	if why, _ := s.buyEligibility(&cs.pending[0], cs); why != "" {
		t.Errorf("AAA's BUY should pass the guards: %s", why)
	}
}

func TestApplyBatchMissingSymbolsAreHeldAndLogged(t *testing.T) {
	logs := captureSchedLog(t)
	s, _ := newTestScheduler(Config{})
	cs := newCycle()
	batch := []*batchEntry{entry("AAA", gemini.PrioCore), entry("BBB", gemini.PrioCore), entry("CCC", gemini.PrioCore)}
	s.applyBatch(cs, batch, []gemini.BatchResult{
		{Symbol: "BBB", Decision: &gemini.Decision{Action: "HOLD", Conviction: 5, Thesis: "t"}},
	})
	if cs.missing != 2 || cs.evaluated != 1 || len(cs.pending) != 1 {
		t.Errorf("missing %d evaluated %d pending %d", cs.missing, cs.evaluated, len(cs.pending))
	}
	for _, sym := range []string{"AAA", "CCC"} {
		if !strings.Contains(logs.String(), fmt.Sprintf("[%s] missing from the batch response: treated as HOLD", sym)) {
			t.Errorf("no log line for %s:\n%s", sym, logs)
		}
		if _, ok := s.gate.last[sym]; !ok {
			t.Errorf("%s: a missing answer is a HOLD for this cycle; the gate should not ask again at once", sym)
		}
	}
	for _, p := range cs.pending {
		if p.decision.Action != "HOLD" {
			t.Errorf("nothing but HOLD may result: %+v", p.decision)
		}
	}
}

func TestBatchFailureFallsBackPerSymbolOnlyWithBudget(t *testing.T) {
	logs := captureSchedLog(t)
	for _, tc := range []struct {
		remaining  int
		wantSingle int
	}{{remaining: 3, wantSingle: 0}, {remaining: 2, wantSingle: 0}, {remaining: 4, wantSingle: 3}} {
		s, _ := newTestScheduler(Config{})
		f := &fakeLLM{batchErr: errors.New("gemini HTTP 503: overloaded"), remaining: tc.remaining}
		s.llm = f
		cs := newCycle()
		cs.pf = nil
		s.runBatch(cs, []*batchEntry{entry("AAA", gemini.PrioCore), entry("BBB", gemini.PrioCore), entry("CCC", gemini.PrioCore)})
		if len(f.singleCalls) != tc.wantSingle {
			t.Errorf("remaining %d: %d per-symbol calls, want %d", tc.remaining, len(f.singleCalls), tc.wantSingle)
		}
		if tc.wantSingle > 0 && cs.evaluated != 3 {
			t.Errorf("remaining %d: fallback answers were not used (evaluated %d)", tc.remaining, cs.evaluated)
		}
	}
	if !strings.Contains(logs.String(), "skipping this cycle") || !strings.Contains(logs.String(), "falling back to one call per symbol") {
		t.Errorf("expected both decisions logged:\n%s", logs)
	}
}

func TestBatchQuotaErrorsStopTheCycleWithoutFallback(t *testing.T) {
	for _, err := range []error{gemini.ErrQuotaExhausted, gemini.ErrBudgetExhausted, gemini.ErrNoModel, fmt.Errorf("%w: wrapped", gemini.ErrNoModel)} {
		s, _ := newTestScheduler(Config{})
		f := &fakeLLM{batchErr: err, remaining: 100}
		s.llm = f
		cs := newCycle()
		cs.pf = nil
		s.runBatch(cs, []*batchEntry{entry("AAA", gemini.PrioHeld)})
		if !cs.stopGemini || len(f.singleCalls) != 0 {
			t.Errorf("%v: stopGemini=%v singleCalls=%d", err, cs.stopGemini, len(f.singleCalls))
		}
	}
}

func captureSchedLog(t *testing.T) *strings.Builder {
	t.Helper()
	var sb strings.Builder
	log.SetOutput(&sb)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &sb
}
