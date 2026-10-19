package broker

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAlpaca serves GET /v2/orders from `existing` and records POSTed orders.
type fakeAlpaca struct {
	mu       sync.Mutex
	existing string // JSON array returned for GET /v2/orders
	listFail bool
	posted   []map[string]any
	gets     []string
}

func (f *fakeAlpaca) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		f.gets = append(f.gets, r.URL.RawQuery)
		if f.listFail {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		if f.existing == "" {
			w.Write([]byte(`[]`))
			return
		}
		w.Write([]byte(f.existing))
	case http.MethodPost:
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		f.posted = append(f.posted, body)
		w.Write([]byte(`{}`))
	}
}

func (f *fakeAlpaca) ids() []string {
	var out []string
	for _, p := range f.posted {
		out = append(out, p["client_order_id"].(string))
	}
	return out
}

func newTestClient(f *fakeAlpaca) (*AlpacaClient, func()) {
	srv := httptest.NewServer(f)
	c := NewAlpacaClient("k", "s", srv.URL)
	// Friday 2026-10-16, 10:15 ET.
	c.now = func() time.Time { return time.Date(2026, 10, 16, 10, 15, 0, 0, etLoc) }
	return c, srv.Close
}

// Every order type the bot submits carries a deterministic "atb-" client_order_id.
func TestOrdersCarryDeterministicClientOrderID(t *testing.T) {
	f := &fakeAlpaca{}
	c, done := newTestClient(f)
	defer done()
	if err := c.PlaceMarketOrder("SGOV", 1, Buy, KindEntry); err != nil {
		t.Fatal(err)
	}
	if err := c.PlaceBracketOrder("SPY", 1, Buy, 90, 120, KindEntry); err != nil {
		t.Fatal(err)
	}
	if err := c.PlaceOTOOrder("IEF", 1, Buy, 90, KindRebalance); err != nil {
		t.Fatal(err)
	}
	if err := c.PlaceMarketOrder("SPY", 1, Sell, KindExit); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"atb-20261016-SGOV-buy-entry", "atb-20261016-SPY-buy-entry",
		"atb-20261016-IEF-buy-rebalance", "atb-20261016-SPY-sell-exit",
	}
	got := f.ids()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for _, id := range got {
		if !strings.HasPrefix(id, ClientOrderPrefix) || len(id) > 128 {
			t.Errorf("bad client_order_id %q", id)
		}
	}
	// The ids are parseable back into their kind (the trade counters rely on it).
	for id, kind := range map[string]string{got[0]: KindEntry, got[2]: KindRebalance, got[3]: KindExit, got[2] + "-r2": KindRebalance} {
		if k, ok := ParseClientOrderID(id); !ok || k != kind {
			t.Errorf("ParseClientOrderID(%q) = %q, %v; want %q", id, k, ok, kind)
		}
	}
	for _, id := range []string{"atb-4ca8e7134cc2c90f233e61d1", "be0e9b06-df9a-4023-b1b8-b3709433ac01", "", "atb-2026-x-y-z-w", "atb-20261016-SPY-buy-bogus"} {
		if _, ok := ParseClientOrderID(id); ok {
			t.Errorf("ParseClientOrderID(%q) should not parse", id)
		}
	}
}

// The same decision always has the same id, and a second attempt is refused
// before anything is sent: a restart cannot place the same order twice.
func TestRepeatedOrderIsRefusedAsDuplicate(t *testing.T) {
	f := &fakeAlpaca{existing: `[{"client_order_id":"atb-20261016-NVDA-buy-entry","side":"buy","status":"filled","filled_qty":"32"}]`}
	c, done := newTestClient(f)
	defer done()
	err := c.PlaceBracketOrder("NVDA", 5, Buy, 90, 120, KindEntry)
	if !errors.Is(err, ErrDuplicateOrder) {
		t.Fatalf("err = %v, want ErrDuplicateOrder", err)
	}
	if len(f.posted) != 0 {
		t.Errorf("a duplicate must not be posted: %v", f.posted)
	}
	// A different kind of buy for the same symbol the same day is the same exposure: refused too.
	if err := c.PlaceOTOOrder("NVDA", 5, Buy, 90, KindRebalance); !errors.Is(err, ErrDuplicateOrder) {
		t.Errorf("rebalance after entry: %v", err)
	}
	// The opposite side, another symbol and another day are independent.
	if err := c.PlaceMarketOrder("NVDA", 32, Sell, KindExit); err != nil {
		t.Errorf("sell after buy: %v", err)
	}
	if err := c.PlaceMarketOrder("MSFT", 1, Buy, KindEntry); err != nil {
		t.Errorf("other symbol: %v", err)
	}
	if got := f.ids(); strings.Join(got, ",") != "atb-20261016-NVDA-sell-exit,atb-20261016-MSFT-buy-entry" {
		t.Errorf("posted = %v", got)
	}
	old := &fakeAlpaca{existing: `[{"client_order_id":"atb-20261015-NVDA-buy-entry","side":"buy","status":"filled","filled_qty":"32"}]`}
	c2, done2 := newTestClient(old)
	defer done2()
	if err := c2.PlaceMarketOrder("NVDA", 1, Buy, KindEntry); err != nil {
		t.Errorf("yesterday's order must not block today's: %v", err)
	}
}

func TestLiveButUnfilledOrdersAlsoCount(t *testing.T) {
	for _, status := range []string{"new", "accepted", "pending_new", "partially_filled", "held"} {
		f := &fakeAlpaca{existing: `[{"client_order_id":"atb-20261016-SPY-buy-entry","side":"buy","status":"` + status + `","filled_qty":"0"}]`}
		c, done := newTestClient(f)
		if err := c.PlaceMarketOrder("SPY", 1, Buy, KindEntry); !errors.Is(err, ErrDuplicateOrder) {
			t.Errorf("status %s: err = %v", status, err)
		}
		done()
	}
}

// A canceled or rejected order can be retried with a new id, up to a limit.
func TestFailedOrdersAreRetriedWithANewIDUpToALimit(t *testing.T) {
	dead := func(id string) string {
		return `{"client_order_id":"` + id + `","side":"buy","status":"canceled","filled_qty":"0"}`
	}
	f := &fakeAlpaca{existing: "[" + dead("atb-20261016-SPY-buy-entry") + "]"}
	c, done := newTestClient(f)
	defer done()
	if err := c.PlaceMarketOrder("SPY", 1, Buy, KindEntry); err != nil {
		t.Fatal(err)
	}
	if got := f.ids(); len(got) != 1 || got[0] != "atb-20261016-SPY-buy-entry-r2" {
		t.Errorf("retry id = %v", got)
	}

	f2 := &fakeAlpaca{existing: "[" + strings.Join([]string{dead("atb-20261016-SPY-buy-entry"), dead("atb-20261016-SPY-buy-entry-r2"), dead("atb-20261016-SPY-buy-entry-r3")}, ",") + "]"}
	c2, done2 := newTestClient(f2)
	defer done2()
	if err := c2.PlaceMarketOrder("SPY", 1, Buy, KindEntry); !errors.Is(err, ErrDuplicateOrder) || len(f2.posted) != 0 {
		t.Errorf("after %d failures: err = %v, posted = %v", maxOrderAttempts, err, f2.posted)
	}
}

// A canceled order that partly filled is a real trade, not a failed attempt.
func TestPartlyFilledCanceledOrderCounts(t *testing.T) {
	f := &fakeAlpaca{existing: `[{"client_order_id":"atb-20261016-SPY-buy-entry","side":"buy","status":"canceled","filled_qty":"3"}]`}
	c, done := newTestClient(f)
	defer done()
	if err := c.PlaceMarketOrder("SPY", 1, Buy, KindEntry); !errors.Is(err, ErrDuplicateOrder) {
		t.Errorf("err = %v", err)
	}
}

// Not knowing whether an order exists is a reason not to send one.
func TestOrderLookupFailureBlocksTheOrder(t *testing.T) {
	f := &fakeAlpaca{listFail: true}
	c, done := newTestClient(f)
	defer done()
	err := c.PlaceMarketOrder("SPY", 1, Buy, KindEntry)
	if err == nil || errors.Is(err, ErrDuplicateOrder) || len(f.posted) != 0 {
		t.Errorf("err = %v, posted = %v", err, f.posted)
	}
	if err := c.CheckOrder("SPY", Buy, KindEntry); err == nil {
		t.Error("CheckOrder must report the lookup failure")
	}
}

func TestCheckOrderSendsNothingAndLooksAtTodayOnly(t *testing.T) {
	f := &fakeAlpaca{}
	c, done := newTestClient(f)
	defer done()
	if err := c.CheckOrder("SPY", Sell, KindExit); err != nil {
		t.Fatal(err)
	}
	if len(f.posted) != 0 || len(f.gets) != 1 {
		t.Fatalf("posted %v, gets %v", f.posted, f.gets)
	}
	// 2026-10-16 00:00 EDT is 04:00 UTC.
	if !strings.Contains(f.gets[0], "after=2026-10-16T04%3A00%3A00Z") || !strings.Contains(f.gets[0], "symbols=SPY") {
		t.Errorf("query = %s", f.gets[0])
	}
	if err := c.CheckOrder("spy; drop", Buy, KindEntry); err == nil {
		t.Error("an invalid symbol must be refused")
	}
}
