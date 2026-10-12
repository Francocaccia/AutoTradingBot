package risk

import (
	"strings"
	"testing"
)

func TestTargetPct(t *testing.T) {
	cases := map[int]float64{0: 0, 1: 0, 6: 0, 7: 5, 8: 7, 9: 10, 10: 10}
	for conv, want := range cases {
		if got := TargetPct(conv); got != want {
			t.Errorf("TargetPct(%d) = %v, want %v", conv, got, want)
		}
	}
}

func TestSize(t *testing.T) {
	base := SizeInput{Equity: 100_000, Spendable: 100_000, Price: 100, Conviction: 8, MaxPositionPct: 10}
	tests := []struct {
		name    string
		mod     func(*SizeInput)
		wantQty int
		wantMsg string
	}{
		{"conviction 7 = 5%", func(i *SizeInput) { i.Conviction = 7 }, 50, ""},
		{"conviction 8 = 7%", func(i *SizeInput) {}, 70, ""},
		{"conviction 9 = 10%", func(i *SizeInput) { i.Conviction = 9 }, 100, ""},
		{"conviction 10 = 10%", func(i *SizeInput) { i.Conviction = 10 }, 100, ""},
		{"below minimum conviction", func(i *SizeInput) { i.Conviction = 6 }, 0, "below minimum"},
		{"MAX_POSITION_PCT caps", func(i *SizeInput) { i.Conviction = 10; i.MaxPositionPct = 6 }, 60, ""},
		{"MAX_POSITION_USD caps", func(i *SizeInput) { i.MaxPositionUSD = 2_000 }, 20, ""},
		{"abs cap above pct is ignored", func(i *SizeInput) { i.MaxPositionUSD = 50_000 }, 70, ""},
		{"buying power caps", func(i *SizeInput) { i.Spendable = 1_550 }, 15, ""},
		{"no buying power", func(i *SizeInput) { i.Spendable = 0 }, 0, "insufficient"},
		{"existing position counts", func(i *SizeInput) { i.CurrentPositionValue = 5_000 }, 20, ""},
		{"already at target", func(i *SizeInput) { i.CurrentPositionValue = 7_000 }, 0, "already at target"},
		{"price above one share", func(i *SizeInput) { i.Price = 20_000 }, 0, "insufficient"},
		{"bad equity", func(i *SizeInput) { i.Equity = 0 }, 0, "invalid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mod(&in)
			qty, msg := Size(in)
			if qty != tc.wantQty {
				t.Errorf("qty = %d, want %d (%s)", qty, tc.wantQty, msg)
			}
			if tc.wantMsg != "" && !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("reason %q lacks %q", msg, tc.wantMsg)
			}
		})
	}
}

func TestBracket(t *testing.T) {
	l, err := Bracket(100, 2.5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if l.Stop != 95 || l.Take != 110 || l.RewardRisk < 2 {
		t.Errorf("default levels = %+v", l)
	}

	if l, err = Bracket(100, 2.5, 112); err != nil || l.Take != 112 {
		t.Errorf("good proposal rejected: %+v %v", l, err)
	}
	if _, err = Bracket(100, 2.5, 107); err == nil || !strings.Contains(err.Error(), "reward:risk") {
		t.Errorf("R:R < 2 must be rejected, got %v", err)
	}
	if _, err = Bracket(100, 2.5, 99); err == nil {
		t.Error("take-profit below price must be rejected")
	}
	if _, err = Bracket(100, 0, 0); err == nil {
		t.Error("missing ATR must be rejected")
	}
	if _, err = Bracket(10, 6, 0); err == nil {
		t.Error("non-positive stop must be rejected")
	}
	// Odd prices must not produce an R:R of 1.999 from rounding.
	if l, err = Bracket(37.33, 0.917, 0); err != nil || l.RewardRisk < 2 {
		t.Errorf("rounding broke 2R: %+v %v", l, err)
	}
}

func TestBreakers(t *testing.T) {
	day, dd, dayPct, ddPct := Breakers(97_000, 100_000, 100_000)
	if !day || dd || dayPct != -3 || ddPct != 3 {
		t.Errorf("-3%% day: day=%v dd=%v %.2f %.2f", day, dd, dayPct, ddPct)
	}
	if day, _, _, _ = Breakers(97_100, 100_000, 100_000); day {
		t.Error("-2.9% must not trip the daily breaker")
	}
	if _, dd, _, _ = Breakers(90_000, 90_000, 100_000); !dd {
		t.Error("10% drawdown from peak must halt")
	}
	if _, dd, _, _ = Breakers(90_500, 90_500, 100_000); dd {
		t.Error("9.5% drawdown must not halt")
	}
	if day, dd, _, _ = Breakers(50, 0, 0); day || dd {
		t.Error("missing baselines must not trip breakers")
	}
}

func TestSectorRoom(t *testing.T) {
	held := map[string]float64{"Technology": 25_000}
	room, ok := SectorRoom("Technology", held, 100_000)
	if !ok || room != 5_000 {
		t.Errorf("room = %v ok=%v, want 5000", room, ok)
	}
	if room, _ = SectorRoom("Technology", map[string]float64{"Technology": 40_000}, 100_000); room != 0 {
		t.Errorf("over-limit sector room = %v, want 0", room)
	}
	if _, ok = SectorRoom("", held, 100_000); ok {
		t.Error("unknown sector must skip the rule")
	}
}
