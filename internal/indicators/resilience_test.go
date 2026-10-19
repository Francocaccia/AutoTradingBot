package indicators

import (
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// A failed batch is retried with a backoff before the symbol is given up on.
func TestBarsRetryWholeRunFailuresWithBackoff(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	p := NewProvider(func(args ...string) ([]byte, []byte, error) {
		if args[0] == "bars" {
			calls++
			if calls < 3 {
				return nil, nil, errors.New("exit status 1") // the worker crashed or timed out
			}
			return []byte(barsJSON), nil, nil
		}
		return []byte(fundsJSON), nil, nil
	})
	p.sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	res, err := p.Calculate("AAA")
	if err != nil || res.Price != 10 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if calls != 3 || len(sleeps) != 2 || sleeps[0] < 2*time.Second || sleeps[1] < 5*time.Second {
		t.Errorf("calls=%d sleeps=%v, want 3 attempts with 2s and 5s backoff", calls, sleeps)
	}
}

// A symbol the worker reports an error for is a real answer: no retry.
func TestBarsPerSymbolErrorsAreNotRetried(t *testing.T) {
	calls := 0
	p := newFastProvider(func(args ...string) ([]byte, []byte, error) {
		if args[0] == "bars" {
			calls++
			return []byte(`{"AAA":{"error":"not enough data for AAA (10 bars)"}}`), nil, nil
		}
		return []byte(fundsJSON), nil, nil
	})
	if _, err := p.Calculate("AAA"); err == nil {
		t.Fatal("want an error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestStaleBarsAreServedBrieflyThenDropped(t *testing.T) {
	failing := false
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	p := newFastProvider(func(args ...string) ([]byte, []byte, error) {
		if args[0] == "bars" {
			if failing {
				return []byte(`{"error":"RuntimeError: yahoo and alpaca down"}`), nil, nil
			}
			return []byte(barsJSON), nil, nil
		}
		return []byte(fundsJSON), nil, nil
	})
	p.now = func() time.Time { return now }
	if res, err := p.Calculate("AAA"); err != nil || res.StaleFor != 0 {
		t.Fatalf("fresh: %+v %v", res, err)
	}

	failing = true
	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	now = now.Add(11 * time.Minute) // past the bars TTL: a refresh is attempted and fails
	res, err := p.Calculate("AAA")
	if err != nil || res.Price != 10 || res.StaleFor != 11*time.Minute {
		t.Fatalf("stale-while-error: %+v %v", res, err)
	}
	if !strings.Contains(logs.String(), "WARN AAA: refresh failed") {
		t.Errorf("serving stale data must warn:\n%s", logs.String())
	}

	now = now.Add(20 * time.Minute) // 31 minutes after the last good fetch
	if _, err := p.Calculate("AAA"); err == nil {
		t.Error("prices older than BarsStaleMax must not be served")
	}

	// Recovery: the next successful refresh clears the staleness.
	failing = false
	now = now.Add(3 * time.Minute)
	if res, err := p.Calculate("AAA"); err != nil || res.StaleFor != 0 {
		t.Errorf("recovered: %+v %v", res, err)
	}
}

func TestStaleMacroKeepsTheRegimeForUpToADay(t *testing.T) {
	failing := false
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	p := NewProvider(func(args ...string) ([]byte, []byte, error) {
		if failing {
			return []byte(`{"error":"Too Many Requests"}`), nil, nil
		}
		return []byte(macroJSON), nil, nil
	})
	p.now = func() time.Time { return now }
	p.sleep = func(time.Duration) {}
	if _, err := p.Macro(); err != nil {
		t.Fatal(err)
	}

	failing = true
	now = now.Add(2 * time.Hour) // expired and the refresh fails: the old series is served
	m, err := p.Macro()
	if err != nil {
		t.Fatalf("a failed refresh within a day must not lose the macro data: %v", err)
	}
	if v, ok := m.Value("^VIX"); !ok || v != 18.2 {
		t.Errorf("VIX = %v %v", v, ok)
	}

	now = now.Add(23 * time.Hour) // 25h after the last good series
	if _, err := p.Macro(); err == nil {
		t.Error("a series older than MacroStaleMax must not be served")
	}

	failing = false
	now = now.Add(11 * time.Minute)
	if _, err := p.Macro(); err != nil {
		t.Errorf("recovery: %v", err)
	}
}

func TestModeTimeouts(t *testing.T) {
	for mode, want := range map[string]time.Duration{"bars": barsTimeout, "macro": macroTimeout, "fundamentals": fundamentalsTimeout, "": fundamentalsTimeout} {
		if got := modeTimeout(mode); got != want || got <= 0 {
			t.Errorf("modeTimeout(%q) = %v, want %v", mode, got, want)
		}
	}
	if macroTimeout >= fundamentalsTimeout || barsTimeout > 2*time.Minute {
		t.Error("timeouts should stay short enough to fit a 15-minute cycle")
	}
}
