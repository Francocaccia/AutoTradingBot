package scheduler

import (
	"testing"
	"time"
)

// 14:00 UTC on 2026-10-12 is 10:00 EDT, the first eligible moment for entries.
var t0 = time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)

func in(sym string, held bool, price, rsi float64, mins int, headlines ...string) evalInput {
	return evalInput{Symbol: sym, Held: held, Price: price, RSI: rsi, Headlines: headlines, Now: t0.Add(time.Duration(mins) * time.Minute)}
}

func TestHeldCooldownBlocksUntilElapsed(t *testing.T) {
	g := newEvalGate(120 * time.Minute)
	if ok, _, why := g.Check(in("AAPL", true, 100, 50, 0)); !ok || why != "first evaluation" {
		t.Fatalf("first look must be evaluated, got ok=%v why=%q", ok, why)
	}
	g.Record(in("AAPL", true, 100, 50, 0))

	if ok, skip, _ := g.Check(in("AAPL", true, 100.5, 52, 45)); ok || skip != "cooldown" {
		t.Fatalf("within cooldown: ok=%v skip=%q", ok, skip)
	}
	if ok, skip, _ := g.Check(in("AAPL", true, 100.5, 52, 119)); ok || skip != "cooldown" {
		t.Fatalf("1 minute before the 2h cooldown: ok=%v skip=%q", ok, skip)
	}
	if ok, _, why := g.Check(in("AAPL", true, 100.5, 52, 120)); !ok || why != "cooldown elapsed" {
		t.Fatalf("after cooldown: ok=%v why=%q", ok, why)
	}
}

func TestHeldCooldownExceptions(t *testing.T) {
	g := newEvalGate(120 * time.Minute)
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

func TestHeldPositionsAreNeverPrefiltered(t *testing.T) {
	g := newEvalGate(120 * time.Minute)
	if ok, _, _ := g.Check(in("HDV", true, 100, 50, 0)); !ok {
		t.Error("a held position with a quiet RSI must still get its review")
	}
	// Held symbols are reviewed at any hour, before 10:00 included.
	if ok, _, _ := g.Check(in("HDV", true, 100, 50, -20)); !ok {
		t.Error("held review is not subject to the 10:00 rule")
	}
}

func TestNonHeldNotEvaluatedBefore1000ET(t *testing.T) {
	g := newEvalGate(120 * time.Minute)
	if ok, skip, _ := g.Check(in("NEWC", false, 20, 35, -1)); ok || skip != "before 10:00" {
		t.Fatalf("09:59 ET: ok=%v skip=%q", ok, skip)
	}
	if ok, _, _ := g.Check(in("NEWC", false, 20, 35, 0)); !ok {
		t.Error("10:00 ET with a setup must be evaluated")
	}
}

func TestNonHeldEvaluatedOncePerTradingDay(t *testing.T) {
	g := newEvalGate(120 * time.Minute)
	g.Record(in("NEWC", false, 20, 35, 0, "NEWC wins contract"))

	// Neither the cooldown elapsing nor a new headline, a 5% move or an RSI
	// cross may re-trigger a symbol we do not hold: those are for held risk.
	for name, ev := range map[string]evalInput{
		"same day, hours later":  in("NEWC", false, 20, 35, 300),
		"new headline":           in("NEWC", false, 20, 35, 30, "NEWC wins contract", "fresh headline"),
		"price moved 5%":         in("NEWC", false, 21, 35, 30),
		"RSI crossed 30":         in("NEWC", false, 20, 25, 30),
		"RSI crossed 70":         in("NEWC", false, 20, 75, 30),
		"cooldown long elapsed":  in("NEWC", false, 20, 35, 240),
		"still ET same day, 3pm": in("NEWC", false, 20, 35, 290),
	} {
		if ok, skip, _ := g.Check(ev); ok || skip != "daily" {
			t.Errorf("%s: ok=%v skip=%q, want skip \"daily\"", name, ok, skip)
		}
	}

	// The next trading day it is evaluated again, after 10:00.
	next := in("NEWC", false, 20, 35, 24*60)
	if ok, _, _ := g.Check(next); !ok {
		t.Error("next day at 10:00 ET must be evaluated")
	}
	early := in("NEWC", false, 20, 35, 24*60-30)
	if ok, skip, _ := g.Check(early); ok || skip != "before 10:00" {
		t.Errorf("next day 09:30 ET: ok=%v skip=%q", ok, skip)
	}
}

func TestPrefilterSkipsQuietCandidatesWithoutUsingTheDay(t *testing.T) {
	g := newEvalGate(120 * time.Minute)
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
	// A "no setup" skip records nothing, so a later cycle that finds a setup may
	// still use the day's one evaluation.
	if ok, _, _ := g.Check(in("NEWC", false, 20, 38, 60)); !ok {
		t.Error("a skipped quiet candidate keeps its daily evaluation")
	}
}

func TestZeroCooldownAlwaysEvaluatesHeld(t *testing.T) {
	g := newEvalGate(0)
	g.Record(in("AAPL", true, 100, 50, 0))
	if ok, _, _ := g.Check(in("AAPL", true, 100, 50, 0)); !ok {
		t.Error("cooldown 0 disables the gate")
	}
}
