package portfolio

import (
	"math"
	"strings"
	"testing"
)

// book builds a 100k account with the given holdings; cash is whatever is left.
func book(holdings ...Holding) Book {
	b := Book{Equity: 100_000, Holdings: holdings}
	var held float64
	for _, h := range holdings {
		held += h.Value
	}
	b.Cash = 100_000 - held
	return b
}

func TestDefaultPolicyIsValid(t *testing.T) {
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := DefaultPolicy()
	bad.EquityMin, bad.EquityMax = 80, 70
	if bad.Validate() == nil {
		t.Error("min > max must be invalid")
	}
	bad = DefaultPolicy()
	bad.EquityMax, bad.BondMax, bad.CashMax = 40, 30, 20
	if bad.Validate() == nil {
		t.Error("maxes that cannot reach 100% must be invalid")
	}
	bad = DefaultPolicy()
	bad.TargetEquity = 90
	if bad.Validate() == nil {
		t.Error("target outside band must be invalid")
	}
}

func TestSleeveClassification(t *testing.T) {
	for sym, want := range map[string]string{
		"AAPL": SleeveEquities, "SPY": SleeveEquities, "SGOV": SleeveCash, "BIL": SleeveCash,
		"SHY": SleeveBonds, "TLT": SleeveBonds, "HYG": SleeveBonds, "LQD": SleeveBonds,
	} {
		if got := Sleeve(sym); got != want {
			t.Errorf("Sleeve(%s) = %s, want %s", sym, got, want)
		}
	}
}

func TestRoomRejections(t *testing.T) {
	p := DefaultPolicy()
	tests := []struct {
		name     string
		book     Book
		symbol   string
		sector   string
		wantZero bool
		wantBind string
	}{
		{"empty account: single stock limit binds at 10%", book(), "AAPL", "Technology", false, "single position limit"},
		{"equity band full", book(
			Holding{"A", 10_000, "Tech"}, Holding{"B", 10_000, "Health"}, Holding{"C", 10_000, "Energy"}, Holding{"D", 10_000, "Utilities"},
			Holding{"E", 10_000, "Financials"}, Holding{"F", 10_000, "Industrials"}, Holding{"G", 10_000, "Materials"}, Holding{"SPY", 5_000, ""}),
			"NEWCO", "Retail", true, "equity band maximum"},
		{"bond ETF already at 15%", book(Holding{"IEF", 15_000, ""}), "IEF", "", true, "single bond ETF limit"},
		{"credit-risk combined at 10%", book(Holding{"HYG", 6_000, ""}, Holding{"EMB", 4_000, ""}), "HYG", "", true, "credit-risk"},
		{"credit cap is shared across HYG and EMB", book(Holding{"HYG", 8_000, ""}), "EMB", "", false, "credit-risk"},
		{"sector at 30%", book(Holding{"A", 10_000, "Technology"}, Holding{"B", 10_000, "Technology"}, Holding{"C", 10_000, "Technology"}),
			"D", "Technology", true, "Technology sector limit"},
		{"unknown sector is one shared bucket", book(Holding{"A", 10_000, ""}, Holding{"B", 10_000, ""}, Holding{"C", 10_000, ""}),
			"D", "", true, "Unknown sector limit"},
		{"broad ETF is exempt from the sector cap", book(Holding{"A", 10_000, "Tech"}, Holding{"B", 10_000, "Tech"}, Holding{"C", 10_000, "Tech"}),
			"SPY", "", false, "single index ETF limit"},
		{"real cash floor", Book{Equity: 100_000, Cash: 2_000, Holdings: []Holding{
			{"A", 20_000, "Tech"}, {"B", 20_000, "Health"}, {"C", 20_000, "Energy"}, {"IEF", 15_000, ""}, {"AGG", 15_000, ""}, {"BND", 8_000, ""}}},
			"SGOV", "", true, "real cash floor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			room, bind := p.Room(tc.book, tc.symbol, tc.sector)
			if tc.wantZero && room > 0 {
				t.Errorf("room = %.0f (%s), want 0", room, bind)
			}
			if !tc.wantZero && room <= 0 {
				t.Errorf("room = %.0f (%s), want > 0", room, bind)
			}
			if !strings.Contains(bind, tc.wantBind) {
				t.Errorf("binding = %q, want it to contain %q", bind, tc.wantBind)
			}
		})
	}
}

func TestRoomValues(t *testing.T) {
	p := DefaultPolicy()
	// 5k of a 10k limit already held: 5k of room.
	if room, _ := p.Room(book(Holding{"AAPL", 5_000, "Technology"}), "AAPL", "Technology"); math.Abs(room-5_000) > 1e-6 {
		t.Errorf("room = %v, want 5000", room)
	}
	// Cash sleeve band minimum (5%): 8k cash, 3k of it must stay.
	b := Book{Equity: 100_000, Cash: 8_000, Holdings: []Holding{
		{"A", 20_000, "Tech"}, {"B", 20_000, "Health"}, {"C", 20_000, "Energy"}, {"IEF", 15_000, ""}, {"AGG", 15_000, ""}, {"BND", 2_000, ""}}}
	p.MinRealCash = 0
	if room, bind := p.Room(b, "NEW", "Health"); math.Abs(room-3_000) > 1e-6 || !strings.Contains(bind, "cash sleeve band minimum") {
		t.Errorf("room = %v (%s), want 3000 by the cash band", room, bind)
	}
}

func TestDurationCap(t *testing.T) {
	p := DefaultPolicy()
	p.MaxBondETFPct = 100 // isolate the duration cap
	p.BondMax = 100
	// 10k SHY (1.9y) held: adding TLT (16.5y) is bounded by the 8y portfolio cap.
	b := book(Holding{"SHY", 10_000, ""})
	room, bind := p.Room(b, "TLT", "")
	if !strings.Contains(bind, "duration") {
		t.Fatalf("binding = %q, want duration", bind)
	}
	// (8*10000 - 1.9*10000) / (16.5 - 8) = 7176.47
	if math.Abs(room-7176.4705882) > 0.01 {
		t.Errorf("room = %v, want ~7176.47", room)
	}
	// After buying exactly that much, the weighted duration is the cap.
	after := book(Holding{"SHY", 10_000, ""}, Holding{"TLT", room, ""})
	if d, _ := after.BondDuration(); math.Abs(d-8) > 1e-6 {
		t.Errorf("duration after = %v, want 8", d)
	}
	// A portfolio already over the cap cannot add more long duration, but may add short.
	over := book(Holding{"TLT", 10_000, ""})
	if room, _ := p.Room(over, "TLT", ""); room != 0 {
		t.Errorf("over the cap: room = %v, want 0", room)
	}
	if room, _ := p.Room(over, "SHY", ""); room <= 0 {
		t.Errorf("a short bond lowers duration and must be allowed, room = %v", room)
	}
	// T-bill ETFs carry no duration constraint.
	if _, bind := p.Room(over, "SGOV", ""); strings.Contains(bind, "duration") {
		t.Errorf("T-bill ETF bound by duration: %q", bind)
	}
}

func TestBandBreachAndClip(t *testing.T) {
	p := DefaultPolicy()
	// 20% cash... within band; equities 35% is 5 points under the 40% minimum.
	b := Book{Equity: 100_000, Cash: 20_000, Holdings: []Holding{{"AAPL", 10_000, "Tech"}, {"SPY", 25_000, ""}, {"IEF", 45_000, ""}}}
	pts, desc := p.BandBreach(b)
	if math.Abs(pts-5) > 1e-9 && math.Abs(pts-5) > 1e-6 {
		// bonds 45% is 5 over the 40% max, equities 35% is 5 under 40%
		t.Errorf("breach = %v (%s), want 5", pts, desc)
	}
	if pts, _ := p.BandBreach(book(Holding{"SPY", 60_000, ""}, Holding{"IEF", 30_000, ""})); pts != 0 {
		t.Errorf("in-band book reported breach %v", pts)
	}

	// Clipping: asks outside the bands are pulled in; if the result cannot sum to 100 it is rejected.
	if got, ok := p.ClipTargets(80, 10, 10); !ok || got.TargetEquity != 75 || got.TargetBond != 15 || got.TargetCash != 10 {
		t.Errorf("clip = %+v ok=%v", got, ok)
	}
	if _, ok := p.ClipTargets(80, 40, 30); ok {
		t.Error("80/40/30 clips to 75/40/30 = 145 and must be rejected")
	}
	if got, ok := p.ClipTargets(50, 30, 20); !ok || got.TargetEquity != 50 {
		t.Errorf("in-band targets must pass unchanged: %+v ok=%v", got, ok)
	}
}

func TestBondUniverse(t *testing.T) {
	all := NewBondUniverse(nil)
	if !all.Allowed("HYG") || !all.Allowed("sgov") || all.Allowed("AAPL") {
		t.Error("default universe wrong")
	}
	sub := NewBondUniverse([]string{"SHY", " IEF ", "NOPE"})
	if got := strings.Join(sub.Symbols(), ","); got != "SHY,IEF" {
		t.Errorf("subset = %s", got)
	}
	if DisabledBondUniverse().Allowed("SHY") {
		t.Error("disabled universe must allow nothing")
	}
	// A held ETF outside the allowed set is still a bond.
	if Sleeve("HYG") != SleeveBonds {
		t.Error("classification must not depend on the allowed set")
	}
	if info, _ := BondInfoFor("HYG"); !info.CreditRisk() {
		t.Error("HYG is credit-risk")
	}
	if info, _ := BondInfoFor("LQD"); info.CreditRisk() {
		t.Error("LQD investment-grade is not in the credit-risk bucket")
	}
}
