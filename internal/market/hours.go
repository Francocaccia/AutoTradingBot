package market

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
)

var eastern = mustLoadET()

func mustLoadET() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		// Should not happen: main imports time/tzdata.
		return time.FixedZone("ET", -5*3600)
	}
	return loc
}

type clockResponse struct {
	Timestamp time.Time `json:"timestamp"`
	IsOpen    bool      `json:"is_open"`
}

type calendarDay struct {
	Date  string `json:"date"`
	Open  string `json:"open"`
	Close string `json:"close"`
}

// Session describes today's trading session as reported by Alpaca, so
// holidays and half days are respected.
type Session struct {
	IsOpen     bool
	TradingDay bool      // false on weekends/holidays
	Now        time.Time // Alpaca's clock timestamp
	Date       string    // YYYY-MM-DD in ET
	Open       time.Time // zero unless TradingDay
	Close      time.Time // zero unless TradingDay
}

// IsMarketOpen queries Alpaca's /v2/clock endpoint, which accounts for
// exchange holidays and half days in addition to regular trading hours.
func IsMarketOpen(b *broker.AlpacaClient) (bool, error) {
	data, err := b.GetClock()
	if err != nil {
		return false, fmt.Errorf("fetching market clock: %w", err)
	}
	var clock clockResponse
	if err := json.Unmarshal(data, &clock); err != nil {
		return false, fmt.Errorf("parsing market clock: %w", err)
	}
	return clock.IsOpen, nil
}

// GetSession combines /v2/clock and /v2/calendar into today's session.
func GetSession(b *broker.AlpacaClient) (*Session, error) {
	data, err := b.GetClock()
	if err != nil {
		return nil, fmt.Errorf("fetching market clock: %w", err)
	}
	var clock clockResponse
	if err := json.Unmarshal(data, &clock); err != nil {
		return nil, fmt.Errorf("parsing market clock: %w", err)
	}
	now := clock.Timestamp.In(eastern)
	s := &Session{IsOpen: clock.IsOpen, Now: now, Date: now.Format("2006-01-02")}

	cal, err := b.GetCalendar(s.Date)
	if err != nil {
		return nil, fmt.Errorf("fetching market calendar: %w", err)
	}
	var days []calendarDay
	if err := json.Unmarshal(cal, &days); err != nil {
		return nil, fmt.Errorf("parsing market calendar: %w", err)
	}
	if len(days) == 0 || days[0].Date != s.Date {
		return s, nil
	}
	open, err1 := parseClock(s.Date, days[0].Open)
	closeT, err2 := parseClock(s.Date, days[0].Close)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("parsing calendar times %q/%q", days[0].Open, days[0].Close)
	}
	s.TradingDay, s.Open, s.Close = true, open, closeT
	return s, nil
}

func parseClock(date, hhmm string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02 15:04", date+" "+hhmm, eastern)
}

// EntryWindowOpen reports whether new positions may be opened: not within
// `buffer` after the open or before the close.
func EntryWindowOpen(now, open, closeT time.Time, buffer time.Duration) bool {
	return EntryWindowOpenAsym(now, open, closeT, buffer, buffer)
}

// EntryWindowOpenAsym is EntryWindowOpen with separate blackouts after the open
// and before the close.
func EntryWindowOpenAsym(now, open, closeT time.Time, afterOpen, beforeClose time.Duration) bool {
	return !now.Before(open.Add(afterOpen)) && now.Before(closeT.Add(-beforeClose))
}
