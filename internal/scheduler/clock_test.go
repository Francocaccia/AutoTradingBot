package scheduler

import (
	"strings"
	"testing"
	"time"
)

func et(day, hhmm string) time.Time {
	tm, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, eastern)
	if err != nil {
		panic(err)
	}
	return tm
}

func TestSlotAtFollowsTheNewYorkWallClock(t *testing.T) {
	q := 15 * time.Minute
	for _, c := range []struct {
		name, day, hhmm string
		ok              bool
		key             string
		start           string
	}{
		{"before the open", "2026-10-15", "09:29", false, "", ""},
		{"at the open", "2026-10-15", "09:30", true, "2026-10-15#0", "09:30"},
		{"inside the first slot", "2026-10-15", "09:44", true, "2026-10-15#0", "09:30"},
		{"second slot", "2026-10-15", "09:45", true, "2026-10-15#1", "09:45"},
		{"mid morning", "2026-10-15", "10:07", true, "2026-10-15#2", "10:00"},
		{"last slot", "2026-10-15", "15:59", true, "2026-10-15#25", "15:45"},
		{"at the close", "2026-10-15", "16:00", false, "", ""},
		{"after hours", "2026-10-15", "20:00", false, "", ""},
		{"Saturday", "2026-10-17", "11:00", false, "", ""},
		{"Sunday", "2026-10-18", "11:00", false, "", ""},
	} {
		key, start, ok := slotAt(et(c.day, c.hhmm), q)
		if ok != c.ok || key != c.key || (ok && start.Format("15:04") != c.start) {
			t.Errorf("%s: slotAt = %q %v ok=%v, want %q %v ok=%v", c.name, key, start.Format("15:04"), ok, c.key, c.start, c.ok)
		}
	}

	// The same instant given in UTC is the same slot: only the ET wall clock matters.
	utc := et("2026-10-15", "10:07").UTC()
	if key, _, ok := slotAt(utc, q); !ok || key != "2026-10-15#2" {
		t.Errorf("UTC input: %q %v", key, ok)
	}
	// Daylight saving: the slot grid follows 09:30 ET on both sides of the change.
	for _, day := range []string{"2026-11-02", "2026-11-03"} { // clocks change on Sunday 2026-11-01
		if key, start, ok := slotAt(et(day, "09:31"), q); !ok || key != day+"#0" || start.Format("15:04") != "09:30" {
			t.Errorf("%s: %q %v %v", day, key, start, ok)
		}
	}
	// A different interval changes the grid.
	if key, _, _ := slotAt(et("2026-10-15", "10:07"), 5*time.Minute); key != "2026-10-15#7" {
		t.Errorf("5-minute slots: %q", key)
	}
	if key, _, ok := slotAt(et("2026-10-15", "09:31"), 0); !ok || key != "2026-10-15#1" {
		t.Errorf("a zero interval is treated as one minute: %q %v", key, ok)
	}
}

// newClockScheduler is a scheduler whose cycle and news hooks only count.
func newClockScheduler() (*Scheduler, *int, *int) {
	s, _ := newTestScheduler(Config{CycleInterval: 15 * time.Minute})
	cycles, news := 0, 0
	s.runCycle = func() { cycles++ }
	s.fetchNews = func() { news++ }
	return s, &cycles, &news
}

func TestAtMostOneCyclePerSlot(t *testing.T) {
	s, cycles, _ := newClockScheduler()
	// The poll fires every 20 seconds: a slot of 15 minutes sees 45 ticks.
	for tm := et("2026-10-15", "10:00"); tm.Before(et("2026-10-15", "10:15")); tm = tm.Add(pollEvery) {
		s.tick(tm)
	}
	if *cycles != 1 {
		t.Fatalf("cycles in one slot = %d, want 1", *cycles)
	}
	s.tick(et("2026-10-15", "10:15"))
	s.tick(et("2026-10-15", "10:15").Add(pollEvery))
	if *cycles != 2 {
		t.Errorf("the next slot runs once: %d", *cycles)
	}
}

func TestAWholeTradingDayRunsOneCyclePerSlot(t *testing.T) {
	s, cycles, _ := newClockScheduler()
	for tm := et("2026-10-15", "06:00"); tm.Before(et("2026-10-15", "20:00")); tm = tm.Add(pollEvery) {
		s.tick(tm)
	}
	if *cycles != 26 { // 09:30 .. 15:45
		t.Errorf("cycles = %d, want 26", *cycles)
	}
}

// A Render free instance sleeps and wakes with a fresh process: on its first tick
// inside a slot it runs ONE catch-up cycle, not one per slot it slept through.
func TestWakeUpRunsASingleCatchUpCycle(t *testing.T) {
	s, cycles, news := newClockScheduler()
	s.tick(et("2026-10-15", "10:01")) // a normal cycle
	if *cycles != 1 {
		t.Fatal("setup")
	}
	logs := captureSchedLog(t)
	s.tick(et("2026-10-15", "13:07")) // woke up after almost three hours
	s.tick(et("2026-10-15", "13:07").Add(pollEvery))
	s.tick(et("2026-10-15", "13:09"))
	if *cycles != 2 {
		t.Errorf("cycles after the wake-up = %d, want exactly one catch-up", *cycles)
	}
	if got := logs.String(); !containsAll(got, "13:00 EDT", "running one cycle now", "missed slots are not replayed") {
		t.Errorf("the catch-up is not explained:\n%s", got)
	}
	if *news == 0 {
		t.Error("stale news must be refreshed on wake-up")
	}
}

func TestFirstTickOfAFreshProcessCatchesUpOnlyWhileTheMarketIsOpen(t *testing.T) {
	for _, c := range []struct {
		name, day, hhmm string
		want            int
	}{
		{"inside a slot", "2026-10-15", "11:42", 1},
		{"before the open", "2026-10-15", "08:00", 0},
		{"after the close", "2026-10-15", "16:30", 0},
		{"weekend", "2026-10-17", "11:42", 0},
	} {
		s, cycles, _ := newClockScheduler()
		s.tick(et(c.day, c.hhmm))
		if *cycles != c.want {
			t.Errorf("%s: cycles = %d, want %d", c.name, *cycles, c.want)
		}
	}
}

func TestNoNetworkWorkOutsideTheTradingDay(t *testing.T) {
	s, cycles, news := newClockScheduler()
	s.newsAt = et("2026-10-15", "08:00")
	for _, tm := range []time.Time{et("2026-10-16", "03:00"), et("2026-10-17", "12:00"), et("2026-10-18", "12:00"), et("2026-10-16", "16:05")} {
		s.tick(tm)
	}
	if *cycles != 0 || *news != 0 {
		t.Errorf("cycles=%d news=%d outside the trading day", *cycles, *news)
	}
}

func TestAClosedDayIsRememberedAndCostsNothing(t *testing.T) {
	s, cycles, _ := newClockScheduler()
	s.runCycle = func() { *cycles++; s.skipDay = "2026-10-15" } // what runMarketCycle does on a holiday
	s.tick(et("2026-10-15", "09:31"))
	s.tick(et("2026-10-15", "09:46"))
	s.tick(et("2026-10-15", "11:00"))
	if *cycles != 1 {
		t.Errorf("a day known to be closed ran %d cycles", *cycles)
	}
	s.tick(et("2026-10-16", "09:31"))
	if *cycles != 2 {
		t.Error("the next day must run again")
	}
}

func TestNewsRefreshesWhenStaleOnly(t *testing.T) {
	s, _, news := newClockScheduler()
	s.tick(et("2026-10-15", "09:31"))
	if *news != 1 {
		t.Fatalf("first cycle loads the news once, got %d", *news)
	}
	s.tick(et("2026-10-15", "09:46"))
	s.tick(et("2026-10-15", "10:01"))
	if *news != 1 {
		t.Errorf("fresh news refreshed again: %d", *news)
	}
	s.newsAt = et("2026-10-15", "10:01").Add(-newsMaxAge - time.Minute)
	s.tick(et("2026-10-15", "10:16"))
	if *news != 2 {
		t.Errorf("stale news not refreshed: %d", *news)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
