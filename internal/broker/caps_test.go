package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// brokerStub is a small Alpaca: it keeps the orders posted to it, so several
// clients (instances of the bot) see each other's orders, and serves the calendar.
type brokerStub struct {
	mu     sync.Mutex
	orders []map[string]any
	posts  int
	fail   bool // GET /v2/orders answers 500
	marks  []map[string]any
}

func (b *brokerStub) add(id, side, status, filled, at string) {
	b.orders = append(b.orders, map[string]any{
		"client_order_id": id, "symbol": strings.ToUpper(strings.Split(id+"----", "-")[2]), "side": side, "status": status,
		"filled_qty": filled, "submitted_at": at, "filled_at": nil,
	})
}

func (b *brokerStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case r.URL.Path == "/v2/calendar":
		// Monday 2026-10-05 to Friday 2026-10-16, weekdays only.
		var days []string
		for _, d := range []string{"2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-12", "2026-10-13", "2026-10-14", "2026-10-15", "2026-10-16"} {
			days = append(days, fmt.Sprintf(`{"date":"%s","open":"09:30","close":"16:00"}`, d))
		}
		w.Write([]byte("[" + strings.Join(days, ",") + "]"))
	case r.URL.Path == "/v2/orders" && r.Method == http.MethodGet:
		if b.fail {
			http.Error(w, `{"message":"down"}`, http.StatusInternalServerError)
			return
		}
		sym := r.URL.Query().Get("symbols")
		var out []map[string]any
		for _, o := range b.orders {
			if sym == "" || o["symbol"] == sym {
				out = append(out, o)
			}
		}
		json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/v2/orders" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		var o map[string]any
		json.Unmarshal(body, &o)
		o["status"], o["filled_qty"], o["submitted_at"] = "accepted", "0", "2026-10-16T14:15:00Z"
		b.orders = append(b.orders, o)
		b.posts++
		w.Write([]byte(`{}`))
	case r.URL.Path == "/v2/watchlists" && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode(b.marksOrEmpty())
	case r.URL.Path == "/v2/watchlists" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(body, &m)
		m["id"] = fmt.Sprintf("id-%d", len(b.marks)+1)
		b.marks = append(b.marks, m)
		json.NewEncoder(w).Encode(m)
	case strings.HasPrefix(r.URL.Path, "/v2/watchlists/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/v2/watchlists/")
		for i, m := range b.marks {
			if m["id"] == id {
				b.marks = append(b.marks[:i], b.marks[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (b *brokerStub) marksOrEmpty() []map[string]any {
	if b.marks == nil {
		return []map[string]any{}
	}
	return b.marks
}

// newClient is an instance of the bot at Friday 2026-10-16 10:15 ET with the caps 2 a day / 5 a week.
func (b *brokerStub) newClient(t *testing.T) *AlpacaClient {
	t.Helper()
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	c := NewAlpacaClient("k", "s", srv.URL)
	c.now = func() time.Time { return time.Date(2026, 10, 16, 10, 15, 0, 0, etLoc) }
	c.SetTradeCaps(2, 5, "2026-10-14")
	return c
}

func (b *brokerStub) post() int { b.mu.Lock(); defer b.mu.Unlock(); return b.posts }

// Five orders already counted by Alpaca: the sixth is refused, whatever any
// in-memory counter says (a fresh client has none).
func TestSixthOrderOfTheWeekIsRefusedFromAlpacasCount(t *testing.T) {
	b := &brokerStub{}
	// 2026-10-14 .. 10-15, none today. Five bot orders: filled, accepted, pending, new and partly filled.
	b.add("atb-20261014-AAA-buy-entry", "buy", "filled", "5", "2026-10-14T14:00:00Z")
	b.add("atb-20261014-BBB-buy-entry", "buy", "accepted", "0", "2026-10-14T14:05:00Z")
	b.add("atb-20261015-CCC-buy-entry", "buy", "pending_new", "0", "2026-10-15T14:00:00Z")
	b.add("atb-20261015-DDD-sell-exit", "sell", "new", "0", "2026-10-15T14:05:00Z")
	b.add("atb-20261015-EEE-buy-rebalance", "buy", "partially_filled", "2", "2026-10-15T14:10:00Z")
	c := b.newClient(t)

	err := c.PlaceBracketOrder("NVDA", 3, Buy, 90, 120, KindEntry)
	if !errors.Is(err, ErrCapReached) || !strings.Contains(err.Error(), "5 of 5") {
		t.Fatalf("err = %v, want ErrCapReached 5 of 5", err)
	}
	for _, place := range []func() error{
		func() error { return c.PlaceMarketOrder("NVDA", 3, Buy, KindEntry) },
		func() error { return c.PlaceOTOOrder("NVDA", 3, Buy, 90, KindRebalance) },
	} {
		if err := place(); !errors.Is(err, ErrCapReached) {
			t.Errorf("every kind of BUY goes through the same check: %v", err)
		}
	}
	if b.post() != 0 {
		t.Errorf("%d orders were sent", b.post())
	}
	// An exit is never held back by the cap.
	if err := c.PlaceMarketOrder("AAA", 5, Sell, KindExit); err != nil {
		t.Errorf("sell: %v", err)
	}
}

// One order below each cap goes through; the next one is the one refused.
func TestOrdersBelowTheCapsGoThroughAndTheNextIsRefused(t *testing.T) {
	b := &brokerStub{}
	b.add("atb-20261015-AAA-buy-entry", "buy", "filled", "5", "2026-10-15T14:00:00Z")
	c := b.newClient(t)
	if err := c.PlaceMarketOrder("NVDA", 1, Buy, KindEntry); err != nil {
		t.Fatalf("1 of 2 today: %v", err)
	}
	if err := c.PlaceMarketOrder("MSFT", 1, Buy, KindEntry); err != nil {
		t.Fatalf("2 of 2 today (week 3 of 5): %v", err)
	}
	err := c.PlaceMarketOrder("AMZN", 1, Buy, KindEntry)
	if !errors.Is(err, ErrCapReached) || !strings.Contains(err.Error(), "2 of 2 orders already today") {
		t.Errorf("the third order today: %v", err)
	}
}

// Two instances alive at once during a deploy: neither remembers the other's
// order, both read Alpaca's list, so together they cannot exceed the cap.
func TestOverlappingInstancesCannotExceedTheDailyCapTogether(t *testing.T) {
	b := &brokerStub{}
	old, fresh := b.newClient(t), b.newClient(t)
	if err := old.PlaceMarketOrder("AAA", 1, Buy, KindEntry); err != nil {
		t.Fatal(err)
	}
	if err := fresh.PlaceMarketOrder("BBB", 1, Buy, KindEntry); err != nil {
		t.Fatal(err)
	}
	// Each has placed one order and thinks one slot is used; Alpaca has two (both only accepted).
	if err := old.PlaceMarketOrder("CCC", 1, Buy, KindEntry); !errors.Is(err, ErrCapReached) {
		t.Errorf("old instance: %v", err)
	}
	if err := fresh.PlaceMarketOrder("DDD", 1, Buy, KindEntry); !errors.Is(err, ErrCapReached) {
		t.Errorf("fresh instance: %v", err)
	}
	if b.post() != 2 {
		t.Errorf("orders sent = %d, want 2", b.post())
	}
}

func TestCapCountIgnoresWhatDoesNotCount(t *testing.T) {
	b := &brokerStub{}
	b.add("atb-20261009-OLD-buy-entry", "buy", "filled", "5", "2026-10-09T14:00:00Z")            // outside the 5-day window
	b.add("atb-20261013-PRE-buy-entry", "buy", "filled", "5", "2026-10-13T14:00:00Z")            // before GUARDS_START_DATE
	b.add("atb-20261015-DEAD-buy-entry", "buy", "canceled", "0", "2026-10-15T14:00:00Z")         // never filled
	b.add("atb-20261015-REJ-buy-entry", "buy", "rejected", "0", "2026-10-15T14:00:00Z")          // never filled
	b.add("atb-20261015-EXP-buy-entry", "buy", "expired", "0", "2026-10-15T14:00:00Z")           // never filled
	b.add("1f2e3d4c-aaaa-bbbb-cccc-000000000001", "sell", "filled", "5", "2026-10-15T14:00:00Z") // a stop leg: not the bot's decision
	b.add("manual-1", "buy", "filled", "5", "2026-10-15T14:00:00Z")
	b.orders[1]["filled_at"] = "2026-10-13T14:00:01Z"
	c := b.newClient(t)
	for _, sym := range []string{"AAA", "BBB", "CCC", "DDD", "EEE"} {
		c.capDay = 0 // only the weekly cap matters here
		if err := c.PlaceMarketOrder(sym, 1, Buy, KindEntry); err != nil {
			t.Fatalf("%s: nothing counted yet should leave room: %v", sym, err)
		}
	}
	if err := c.PlaceMarketOrder("FFF", 1, Buy, KindEntry); !errors.Is(err, ErrCapReached) {
		t.Errorf("five new orders fill the week: %v", err)
	}
}

func TestPartlyFilledCanceledOrderCountsInTheCapsToo(t *testing.T) {
	b := &brokerStub{}
	b.add("atb-20261016-AAA-buy-entry", "buy", "canceled", "2", "2026-10-16T14:00:00Z")
	b.add("atb-20261016-BBB-buy-entry", "buy", "filled", "2", "2026-10-16T14:01:00Z")
	c := b.newClient(t)
	if err := c.PlaceMarketOrder("CCC", 1, Buy, KindEntry); !errors.Is(err, ErrCapReached) {
		t.Errorf("a partly filled order is a trade: %v", err)
	}
}

// Not being able to count is a refusal, not a pass.
func TestOrderIsNotSentWhenAlpacaCannotBeCounted(t *testing.T) {
	b := &brokerStub{}
	c := b.newClient(t)
	b.fail = true
	err := c.PlaceMarketOrder("NVDA", 1, Buy, KindEntry)
	if err == nil || b.post() != 0 {
		t.Errorf("err %v posts %d", err, b.post())
	}
}

func TestNoCapsMeansNoCheck(t *testing.T) {
	b := &brokerStub{}
	for i := 0; i < 8; i++ {
		b.add(fmt.Sprintf("atb-20261016-S%dX-buy-entry", i), "buy", "filled", "1", "2026-10-16T13:00:00Z")
	}
	c := b.newClient(t)
	c.SetTradeCaps(0, 0, "")
	if err := c.PlaceMarketOrder("NVDA", 1, Buy, KindEntry); err != nil {
		t.Errorf("caps off: %v", err)
	}
}

func TestMarkersAreWatchlistsThatSurviveAClient(t *testing.T) {
	b := &brokerStub{}
	c := b.newClient(t)
	if has, err := c.HasMarker("atb-weekly-review-2026-10-12"); err != nil || has {
		t.Fatalf("empty account: %v %v", has, err)
	}
	if err := c.SetMarker("atb-weekly-review-2026-10-12"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMarker("atb-weekly-review-2026-10-12"); err != nil { // idempotent
		t.Fatal(err)
	}
	if len(b.marks) != 1 {
		t.Fatalf("marks = %v", b.marks)
	}
	// A different client (a restarted process) sees it; another week's name does not match.
	other := b.newClient(t)
	if has, _ := other.HasMarker("atb-weekly-review-2026-10-12"); !has {
		t.Error("the marker must be visible to a new process")
	}
	if has, _ := other.HasMarker("atb-weekly-review-2026-10-19"); has {
		t.Error("another week is another marker")
	}
	if err := other.ClearMarker("atb-weekly-review-2026-10-12"); err != nil || len(b.marks) != 0 {
		t.Errorf("clear: %v %v", err, b.marks)
	}
	if err := other.ClearMarker("never-set"); err != nil {
		t.Errorf("clearing nothing: %v", err)
	}
}

func TestUnreadableMarkersAreAnErrorNotAbsence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"down"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewAlpacaClient("k", "s", srv.URL)
	if has, err := c.HasMarker("x"); err == nil || has {
		t.Errorf("has=%v err=%v: unknown must not read as absent", has, err)
	}
	if err := c.SetMarker("x"); err == nil {
		t.Error("an unwritable marker must be an error")
	}
}
