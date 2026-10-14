package indicators

import (
	"errors"
	"testing"
	"time"
)

const macroJSON = `{"^IRX":{"close":4.1},"^FVX":{"close":4.0},"^TNX":{"close":4.3},"^TYX":{"close":4.6},"^VIX":{"close":18.2},
 "SPY":{"close":520.5,"ma200":500.1},"TLT":{"close":90,"ma200":null}}`

func TestMacroParsesCachesAndExpires(t *testing.T) {
	var calls [][]string
	now := time.Date(2026, 10, 14, 14, 0, 0, 0, time.UTC)
	p := NewProvider(func(args ...string) ([]byte, []byte, error) {
		calls = append(calls, args)
		return []byte(macroJSON), nil, nil
	})
	p.now = func() time.Time { return now }

	m, err := p.Macro()
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := m.Value("^VIX"); !ok || v != 18.2 {
		t.Errorf("VIX = %v %v", v, ok)
	}
	if v, ok := m.MA200("SPY"); !ok || v != 500.1 {
		t.Errorf("SPY MA = %v %v", v, ok)
	}
	if _, ok := m.MA200("TLT"); ok {
		t.Error("a null moving average is unavailable")
	}
	if _, ok := m.Value("^NOPE"); ok {
		t.Error("unknown series")
	}
	if len(calls) != 1 || calls[0][0] != "macro" || len(calls[0]) != 1 {
		t.Fatalf("calls = %v, want one 'macro' invocation", calls)
	}

	p.Macro()
	now = now.Add(59 * time.Minute)
	p.Macro()
	if len(calls) != 1 {
		t.Errorf("macro must be cached for an hour, calls = %d", len(calls))
	}
	now = now.Add(2 * time.Minute)
	p.Macro()
	if len(calls) != 2 {
		t.Errorf("macro must refresh after an hour, calls = %d", len(calls))
	}
}

func TestMacroFailureDegradesAndRetriesLater(t *testing.T) {
	now := time.Date(2026, 10, 14, 14, 0, 0, 0, time.UTC)
	n := 0
	p := NewProvider(func(args ...string) ([]byte, []byte, error) {
		n++
		return []byte(`{"error":"Too Many Requests"}`), nil, errors.New("exit 0")
	})
	p.now = func() time.Time { return now }
	if _, err := p.Macro(); err == nil {
		t.Fatal("want an error")
	}
	p.Macro()
	if n != 1 {
		t.Errorf("failure must be cached briefly, calls = %d", n)
	}
	now = now.Add(11 * time.Minute)
	p.Macro()
	if n != 2 {
		t.Errorf("failure must be retried after the failure TTL, calls = %d", n)
	}
}
