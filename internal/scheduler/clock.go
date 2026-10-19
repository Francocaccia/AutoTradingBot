package scheduler

import (
	"log"
	"strconv"
	"time"
)

// The nominal US equity session in America/New_York. The real open, close,
// holidays and half days come from Alpaca's clock and calendar when a cycle
// runs (market.GetSession); these bounds only decide when to ask.
const (
	slotOpenMinute  = 9*60 + 30
	slotCloseMinute = 16 * 60
)

// slotAt returns the cycle slot containing t: its key (ET date and index), the
// time it began, and ok=false outside the nominal trading day (weekends, before
// 09:30 or from 16:00 ET). Slots are counted from 09:30 ET in steps of
// interval, so they depend on the wall clock only, never on when the process
// started or how long it slept.
func slotAt(t time.Time, interval time.Duration) (key string, start time.Time, ok bool) {
	et := t.In(eastern)
	if wd := et.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return "", time.Time{}, false
	}
	minute := et.Hour()*60 + et.Minute()
	if minute < slotOpenMinute || minute >= slotCloseMinute {
		return "", time.Time{}, false
	}
	step := int(interval / time.Minute)
	if step < 1 {
		step = 1
	}
	idx := (minute - slotOpenMinute) / step
	start = time.Date(et.Year(), et.Month(), et.Day(), 9, 30+idx*step, 0, 0, eastern)
	return et.Format("2006-01-02") + "#" + strconv.Itoa(idx), start, true
}

// tick looks at the wall clock and runs a decision cycle if now falls in a slot
// that has not had one yet. The rules:
//
//   - at most one cycle per slot, however often tick is called;
//   - a process that was asleep (Render free tier) or has just started runs one
//     catch-up cycle on its first tick inside a slot, never one per missed slot;
//   - outside the nominal trading day nothing happens and no network call is made;
//   - a day Alpaca's calendar says is closed (holiday, or after an early close)
//     is remembered, so its remaining slots cost nothing.
func (s *Scheduler) tick(now time.Time) {
	key, start, ok := slotAt(now, s.cfg.CycleInterval)
	if !ok || key == s.lastSlot {
		return
	}
	if s.skipDay == now.In(eastern).Format("2006-01-02") {
		return
	}
	if late := now.Sub(start); s.lastCycle.IsZero() || late > 2*pollEvery {
		why := "late"
		if s.lastCycle.IsZero() {
			why = "first since start or wake-up"
		}
		log.Printf("[Scheduler] slot %s began %s ago (%s): running one cycle now, missed slots are not replayed", start.Format("15:04 MST"), late.Round(time.Second), why)
	}
	s.lastSlot, s.lastCycle = key, now
	s.slotsRun++
	s.refreshNewsIfStale(now)
	s.runCycle()
}

// refreshNews reloads the news cache and notes when.
func (s *Scheduler) refreshNews() {
	s.newsAt = s.clock()
	s.fetchNews()
}

// refreshNewsIfStale reloads the news when it is older than newsMaxAge (or was
// never loaded), which also covers a wake-up after a long sleep.
func (s *Scheduler) refreshNewsIfStale(now time.Time) {
	if s.newsAt.IsZero() || now.Sub(s.newsAt) >= newsMaxAge {
		s.refreshNews()
	}
}

// clock is the current time (injectable in tests).
func (s *Scheduler) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
