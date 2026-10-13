package trades

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
)

func et(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04", s, eastern)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// Thursday 2026-10-15 is "now". Columbus Day (Mon 10-12) is NOT a market
// holiday, so the calendar below is simply Mon-Fri.
var (
	days = []string{"2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-12", "2026-10-13", "2026-10-14", "2026-10-15"}
)

func snap(t *testing.T, fills ...Fill) *Snapshot {
	t.Helper()
	return NewSnapshot(fills, days, et(t, "2026-10-15 11:00"))
}

func TestPositionAge(t *testing.T) {
	buy := func(sym, at string, qty float64) Fill {
		return Fill{Symbol: sym, Side: "buy", Type: "market", At: et(t, at), Qty: qty}
	}
	s := snap(t,
		buy("AAA", "2026-10-13 10:30", 100), // 2 trading days ago
		buy("BBB", "2026-10-15 10:30", 50),  // today
		buy("CCC", "2026-10-09 15:00", 10),  // opened Friday; Mon, Tue, Wed, Thu = 4
	)
	for sym, want := range map[string]int{"AAA": 2, "BBB": 0, "CCC": 4} {
		if got, ok := s.PositionAge(sym); !ok || got != want {
			t.Errorf("%s age = %d ok=%v, want %d", sym, got, ok, want)
		}
	}
}

func TestPositionAgeUnknownForLegacyPositions(t *testing.T) {
	s := snap(t)
	if _, ok := s.PositionAge("OLD"); ok {
		t.Error("no order in the window means unknown age")
	}
	// Opened before the window, partly sold inside it: still unknown.
	s = snap(t, Fill{Symbol: "OLD", Side: "sell", Type: "limit", At: et(t, "2026-10-13 12:00"), Qty: 5})
	if _, ok := s.PositionAge("OLD"); ok {
		t.Error("a lone sell must not be mistaken for an opening")
	}
}

func TestPositionAgeUsesTheCurrentRoundTrip(t *testing.T) {
	s := snap(t,
		Fill{Symbol: "AAA", Side: "buy", Type: "market", At: et(t, "2026-10-06 10:30"), Qty: 100},
		Fill{Symbol: "AAA", Side: "sell", Type: "stop", At: et(t, "2026-10-08 11:00"), Qty: 100},
		Fill{Symbol: "AAA", Side: "buy", Type: "market", At: et(t, "2026-10-14 10:30"), Qty: 40},
		Fill{Symbol: "AAA", Side: "buy", Type: "market", At: et(t, "2026-10-15 10:30"), Qty: 20}, // an add does not reset the age
	)
	if got, ok := s.PositionAge("AAA"); !ok || got != 1 {
		t.Errorf("age = %d ok=%v, want 1 (since the 10-14 re-entry)", got, ok)
	}
	// Fully closed: no open position, so no age.
	s = snap(t,
		Fill{Symbol: "AAA", Side: "buy", Type: "market", At: et(t, "2026-10-13 10:30"), Qty: 10},
		Fill{Symbol: "AAA", Side: "sell", Type: "market", At: et(t, "2026-10-14 10:30"), Qty: 10},
	)
	if _, ok := s.PositionAge("AAA"); ok {
		t.Error("closed position has no age")
	}
}

func TestSinceLastSellCountsEverySellKind(t *testing.T) {
	s := snap(t,
		Fill{Symbol: "AAA", Side: "sell", Type: "market", At: et(t, "2026-10-13 10:30"), Qty: 10},
		Fill{Symbol: "BBB", Side: "sell", Type: "stop", At: et(t, "2026-10-14 10:30"), Qty: 10},
		Fill{Symbol: "CCC", Side: "buy", Type: "market", At: et(t, "2026-10-14 10:30"), Qty: 10},
	)
	if got, ok := s.SinceLastSell("AAA"); !ok || got != 2 {
		t.Errorf("AAA = %d ok=%v, want 2", got, ok)
	}
	if got, ok := s.SinceLastSell("BBB"); !ok || got != 1 {
		t.Errorf("BBB (stopped out) = %d ok=%v, want 1", got, ok)
	}
	if _, ok := s.SinceLastSell("CCC"); ok {
		t.Error("a buy is not a sell")
	}
}

func TestDecisionCountsExcludeBracketLegs(t *testing.T) {
	fills := []Fill{
		{Symbol: "A", Side: "buy", Type: "market", At: et(t, "2026-10-15 10:05"), Qty: 1},  // today, decision
		{Symbol: "A", Side: "sell", Type: "stop", At: et(t, "2026-10-15 10:40"), Qty: 1},   // leg
		{Symbol: "B", Side: "sell", Type: "limit", At: et(t, "2026-10-15 10:41"), Qty: 1},  // leg
		{Symbol: "C", Side: "sell", Type: "market", At: et(t, "2026-10-14 10:41"), Qty: 1}, // yesterday, decision
		{Symbol: "D", Side: "buy", Type: "market", At: et(t, "2026-10-09 10:41"), Qty: 1},  // 5th trading day back: in the window
		{Symbol: "E", Side: "buy", Type: "market", At: et(t, "2026-10-08 10:41"), Qty: 1},  // 6th: out
	}
	today, week := snap(t, fills...).DecisionCounts()
	// Window = last 5 trading days = 10-09 .. 10-15 is 5 days only if 10-09,12,13,14,15.
	if today != 1 || week != 3 {
		t.Errorf("today=%d week=%d, want 1 and 3", today, week)
	}
}

func TestWeekdaysFallback(t *testing.T) {
	got := Weekdays(et(t, "2026-10-08 12:00"), et(t, "2026-10-13 12:00"))
	want := []string{"2026-10-08", "2026-10-09", "2026-10-12", "2026-10-13"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

type fakeSource struct {
	fills   []Fill
	days    []string
	daysErr error
}

func (f fakeSource) Fills(time.Time) ([]Fill, error) { return f.fills, nil }
func (f fakeSource) TradingDays(time.Time, time.Time) ([]string, error) {
	return f.days, f.daysErr
}

func TestLoadFallsBackToWeekdaysWhenCalendarFails(t *testing.T) {
	now := et(t, "2026-10-15 11:00")
	fills := []Fill{{Symbol: "AAA", Side: "buy", Type: "market", At: et(t, "2026-10-13 10:30"), Qty: 1}}
	s, err := Load(fakeSource{fills: fills, daysErr: http.ErrHandlerTimeout}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.PositionAge("AAA"); !ok || got != 2 {
		t.Errorf("age = %d ok=%v, want 2 via weekdays", got, ok)
	}
}

func TestAlpacaSourceParsesFilledOrders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/orders":
			w.Write([]byte(`[
			 {"id":"1","symbol":"AAA","side":"buy","type":"market","status":"filled","filled_qty":"10","filled_at":"2026-10-13T14:31:00.123Z","submitted_at":"2026-10-13T14:30:59Z"},
			 {"id":"2","symbol":"AAA","side":"sell","type":"stop","status":"filled","filled_qty":"10","filled_at":"2026-10-14T15:00:00Z","submitted_at":"2026-10-13T14:30:59Z"},
			 {"id":"3","symbol":"BBB","side":"sell","type":"limit","status":"canceled","filled_qty":"0","filled_at":null,"submitted_at":"2026-10-13T14:30:59Z"},
			 {"id":"1","symbol":"AAA","side":"buy","type":"market","status":"filled","filled_qty":"10","filled_at":"2026-10-13T14:31:00.123Z","submitted_at":"2026-10-13T14:30:59Z"}
			]`))
		case "/v2/calendar":
			if r.URL.Query().Get("start") == "" || r.URL.Query().Get("end") == "" {
				t.Errorf("calendar query missing range: %s", r.URL.RawQuery)
			}
			w.Write([]byte(`[{"date":"2026-10-14"},{"date":"2026-10-13"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := AlpacaSource{Broker: broker.NewAlpacaClient("k", "s", srv.URL)}
	fills, err := src.Fills(et(t, "2026-10-01 00:00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 2 {
		t.Fatalf("want 2 filled orders (cancelled and duplicate dropped), got %+v", fills)
	}
	if !fills[0].IsDecision() || fills[1].IsDecision() {
		t.Errorf("buy is a decision, stop leg is not: %+v", fills)
	}
	d, err := src.TradingDays(et(t, "2026-10-01 00:00"), et(t, "2026-10-15 00:00"))
	if err != nil || len(d) != 2 || d[0] != "2026-10-13" {
		t.Errorf("trading days = %v, %v", d, err)
	}
}
