package scheduler

import (
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/portfolio"
)

// field returns the " | "-separated field of a [Review] line that starts with prefix.
func field(t *testing.T, line, prefix string) string {
	t.Helper()
	for _, f := range strings.Split(line, " | ") {
		if strings.HasPrefix(f, prefix) {
			return f
		}
	}
	t.Fatalf("line has no %q field: %s", prefix, line)
	return ""
}

func balancedCycle(s *Scheduler) *cycleState {
	cs := newCycle()
	withRegime(cs, true)
	hold(cs, "SPY", 55_000)
	hold(cs, "IEF", 15_000)
	withAccount(s, cs, 100_000, 30_000)
	return cs
}

func TestReviewLineCarriesEveryField(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	cs := rebalanceCycle(s) // equities 31%, bonds 0%, cash 69%
	withRegime(cs, true)
	cs.pf.regime.Label = "risk-off, inverted curve"
	cs.why("rebalance: one trade per day already made (today 1)")

	line := s.reviewLine(cs, session("2026-10-16", "10:15"))
	if !strings.HasPrefix(line, "[Review] ts=2026-10-16 10:15 ") {
		t.Errorf("timestamp: %s", line)
	}
	for _, want := range []string{
		"regime=risk-off, inverted curve",
		"equities 31.0% (band 40-75%, below min)",
		"bonds 0.0% (band 15-40%, below min)",
		"cash 69.0% (band 5-30%, above max)",
		"mode=gradual",
		"action=none",
		"skip=rebalance: one trade per day already made (today 1)",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Error("the review line is one line")
	}
}

func TestReviewLineInBandAndNothingToSay(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	cs := balancedCycle(s)
	line := s.reviewLine(cs, session("2026-10-13", "10:30"))
	for _, want := range []string{"equities 55.0% (band 40-75%, in band)", "bonds 15.0% (band 15-40%, in band)", "cash 30.0% (band 5-30%, in band)", "action=none"} {
		if !strings.Contains(line, want) {
			t.Errorf("line lacks %q:\n%s", want, line)
		}
	}
	// Never silent: with no reason on record the line still says something.
	if got := field(t, line, "skip="); got == "skip=" || got == "skip=-" {
		t.Errorf("a cycle that did nothing needs a skip reason, got %q", got)
	}
}

func TestReviewLineActionsAreOrderedAndHaveNoSkip(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	cs := balancedCycle(s)
	cs.took("sell")
	cs.took("rebalance")
	cs.took("buy")
	cs.took("buy")
	cs.why("this reason is irrelevant once something happened")
	line := s.reviewLine(cs, session("2026-10-13", "10:30"))
	if field(t, line, "action=") != "action=rebalance+buy+sell" || field(t, line, "skip=") != "skip=-" {
		t.Errorf("line: %s", line)
	}
}

func TestReviewLineRegimeUnavailable(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	cs := newCycle()
	withAccount(s, cs, 100_000, 100_000)
	cs.pf = nil
	if line := s.reviewLine(cs, session("2026-10-13", "10:30")); !strings.Contains(line, "regime=unavailable") {
		t.Errorf("line: %s", line)
	}
	// No account at all (a cycle that ended early): still a line.
	line := s.reviewLine(&cycleState{whyNot: []string{"market closed"}}, nil)
	if !strings.Contains(line, "allocation unavailable") || !strings.Contains(line, "skip=market closed") || !strings.HasPrefix(line, "[Review] ts=") {
		t.Errorf("line: %s", line)
	}
}

func TestReviewLineShowsTheConfiguredMode(t *testing.T) {
	for _, mode := range []string{"gradual", "advisory", "off"} {
		cfg := baseConfig()
		cfg.IPSBandsMode = mode
		s, _ := newTestScheduler(cfg)
		if line := s.reviewLine(newCycle(), session("2026-10-13", "10:30")); !strings.Contains(line, "mode="+mode) {
			t.Errorf("mode %s: %s", mode, line)
		}
	}
	// Unset or unknown values read as the default.
	s, _ := newTestScheduler(baseConfig())
	if s.bandsMode() != bandsGradual {
		t.Errorf("default mode = %q", s.bandsMode())
	}
	s.cfg.IPSBandsMode = "nonsense"
	if s.bandsMode() != bandsGradual {
		t.Errorf("unknown mode = %q", s.bandsMode())
	}
}

// logEarlyReview is the line of a cycle that ended before the account was read.
type fakePositions struct {
	list []portfolio.Position
	err  error
}

func (f fakePositions) GetAllPositions() ([]portfolio.Position, error) { return f.list, f.err }

func TestEarlyCycleLogsExactlyOneLineWithTheAllocation(t *testing.T) {
	s, _ := newTestScheduler(baseConfig())
	s.acct = fakeAcct{portfolio.Account{Equity: 100_000, Cash: 69_000, BuyingPower: 69_000}}
	s.pos = fakePositions{list: []portfolio.Position{{Symbol: "AAPL", Qty: 100, MarketVal: 31_000}}}
	logs := captureSchedLog(t)
	s.logEarlyReview(session("2026-10-16", "12:00"), "scheduler paused")
	lines := reviewLines(logs.String())
	if len(lines) != 1 {
		t.Fatalf("want exactly one [Review] line, got %d: %v", len(lines), lines)
	}
	for _, want := range []string{"equities 31.0% (band 40-75%, below min)", "cash 69.0% (band 5-30%, above max)", "action=none", "skip=scheduler paused"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line lacks %q:\n%s", want, lines[0])
		}
	}

	// Positions that cannot be read: the allocation is left out rather than guessed.
	s.pos = fakePositions{err: errTest}
	logs = captureSchedLog(t)
	s.logEarlyReview(session("2026-10-16", "12:00"), "market closed")
	lines = reviewLines(logs.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "allocation unavailable") || !strings.Contains(lines[0], "skip=market closed") {
		t.Errorf("lines = %v", lines)
	}
}

// reviewLines returns the [Review] lines of a log.
func reviewLines(log string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		if strings.Contains(l, "[Review]") {
			out = append(out, l)
		}
	}
	return out
}
