package scheduler

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)

func in(sym string, held bool, price, rsi float64, mins int, headlines ...string) evalInput {
	return evalInput{Symbol: sym, Held: held, Price: price, RSI: rsi, Headlines: headlines, Now: t0.Add(time.Duration(mins) * time.Minute)}
}

func TestCooldownBlocksUntilElapsed(t *testing.T) {
	g := newEvalGate(45 * time.Minute)
	if ok, _, why := g.Check(in("AAPL", true, 100, 50, 0)); !ok || why != "first evaluation" {
		t.Fatalf("first look must be evaluated, got ok=%v why=%q", ok, why)
	}
	g.Record(in("AAPL", true, 100, 50, 0))

	if ok, skip, _ := g.Check(in("AAPL", true, 100.5, 52, 15)); ok || skip != "cooldown" {
		t.Fatalf("within cooldown: ok=%v skip=%q", ok, skip)
	}
	if ok, _, why := g.Check(in("AAPL", true, 100.5, 52, 45)); !ok || why != "cooldown elapsed" {
		t.Fatalf("after cooldown: ok=%v why=%q", ok, why)
	}
}

func TestCooldownExceptions(t *testing.T) {
	g := newEvalGate(45 * time.Minute)
	g.Record(in("AAPL", true, 100, 50, 0, "old headline"))

	cases := []struct {
		name string
		in   evalInput
		want bool
	}{
		{"price +2.5%", in("AAPL", true, 102.5, 52, 10, "old headline"), true},
		{"price -2.5%", in("AAPL", true, 97.5, 52, 10, "old headline"), true},
		{"price +1.5%", in("AAPL", true, 101.5, 52, 10, "old headline"), false},
		{"RSI crosses 70", in("AAPL", true, 100, 71, 10, "old headline"), true},
		{"new headline", in("AAPL", true, 100, 52, 10, "old headline", "fresh headline"), true},
		{"same headlines", in("AAPL", true, 100, 52, 10, "old headline"), false},
	}
	for _, c := range cases {
		if ok, _, _ := g.Check(c.in); ok != c.want {
			t.Errorf("%s: ok=%v, want %v", c.name, ok, c.want)
		}
	}

	g.Record(in("MSFT", true, 100, 25, 0))
	if ok, _, _ := g.Check(in("MSFT", true, 100, 31, 5)); !ok {
		t.Error("RSI crossing 30 upward should trigger")
	}
	if ok, _, _ := g.Check(in("MSFT", true, 100, 20, 5)); ok {
		t.Error("RSI staying below 30 is not a crossing")
	}
}

func TestPrefilterSkipsQuietCandidatesOnly(t *testing.T) {
	g := newEvalGate(45 * time.Minute)
	if ok, skip, _ := g.Check(in("NEWC", false, 20, 50, 0)); ok || skip != "no setup" {
		t.Fatalf("quiet candidate: ok=%v skip=%q", ok, skip)
	}
	if ok, _, _ := g.Check(in("NEWC", false, 20, 50, 0, "NEWC wins contract")); !ok {
		t.Error("news is a setup")
	}
	if ok, _, _ := g.Check(in("NEWC", false, 20, 35, 0)); !ok {
		t.Error("RSI 35 is a setup")
	}
	if ok, _, _ := g.Check(in("NEWC", false, 20, 65, 0)); !ok {
		t.Error("RSI 65 is a setup")
	}
	// Held positions are reviewed regardless of the prefilter.
	if ok, _, _ := g.Check(in("HDV", true, 100, 50, 0)); !ok {
		t.Error("held position must not be prefiltered")
	}
	// A price move is a setup even with a mid-range RSI.
	g.Record(in("NEWC", false, 20, 50, 0))
	if ok, _, _ := g.Check(in("NEWC", false, 21, 50, 5)); !ok {
		t.Error("5% move should bypass the prefilter")
	}
	// After the cooldown a quiet candidate is skipped again by the prefilter.
	if ok, skip, _ := g.Check(in("NEWC", false, 20, 50, 60)); ok || skip != "no setup" {
		t.Errorf("quiet candidate after cooldown: ok=%v skip=%q", ok, skip)
	}
}

func TestZeroCooldownAlwaysEvaluatesHeld(t *testing.T) {
	g := newEvalGate(0)
	g.Record(in("AAPL", true, 100, 50, 0))
	if ok, _, _ := g.Check(in("AAPL", true, 100, 50, 0)); !ok {
		t.Error("cooldown 0 disables the gate")
	}
}
