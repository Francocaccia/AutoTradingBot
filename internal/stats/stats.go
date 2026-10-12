// Package stats persists the bot's daily performance against the SPY
// benchmark and the equity high-water mark used by the drawdown breaker.
package stats

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Record is one end-of-day performance line.
type Record struct {
	Date          string  `json:"date"`
	Equity        float64 `json:"equity"`
	DayPnLPct     float64 `json:"day_pnl_pct"`
	SPYDayPct     float64 `json:"spy_day_pct"`
	CumPnLPct     float64 `json:"cum_pnl_pct"`
	SPYCumPct     float64 `json:"spy_cum_pct"`
	OpenPositions int     `json:"open_positions"`
}

type state struct {
	StartDate   string  `json:"start_date"`
	StartEquity float64 `json:"start_equity"`
	StartSPY    float64 `json:"start_spy"`
	PeakEquity  float64 `json:"peak_equity"`
	LastDate    string  `json:"last_recorded_date"`
	Latest      *Record `json:"latest"`
}

// Tracker is safe for concurrent use (the /stats handler runs on its own goroutine).
type Tracker struct {
	dir string
	mu  sync.Mutex
	st  state
}

// New loads (or initialises) the tracker state under dir, e.g. ./data.
func New(dir string) (*Tracker, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	t := &Tracker{dir: dir}
	data, err := os.ReadFile(t.statePath())
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &t.st); err != nil {
			return nil, fmt.Errorf("corrupt %s: %w", t.statePath(), err)
		}
	case !os.IsNotExist(err):
		return nil, err
	}
	return t, nil
}

func (t *Tracker) statePath() string { return filepath.Join(t.dir, "state.json") }
func (t *Tracker) csvPath() string   { return filepath.Join(t.dir, "performance.csv") }

// ObservePeak raises the stored high-water mark if equity exceeds it and
// returns the current peak.
func (t *Tracker) ObservePeak(equity float64) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if equity > t.st.PeakEquity {
		t.st.PeakEquity = equity
		if err := t.saveLocked(); err != nil {
			log.Printf("[Stats] could not persist peak equity: %v", err)
		}
	}
	return t.st.PeakEquity
}

// Recorded reports whether the given date (YYYY-MM-DD) already has a record.
func (t *Tracker) Recorded(date string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st.LastDate == date
}

// RecordDay computes, appends and persists the end-of-day line.
// dayStartEquity is the previous close's equity; spyClose/spyPrevClose are
// SPY's close today and on the prior session. The first call fixes the
// baselines for the cumulative figures.
func (t *Tracker) RecordDay(date string, equity, dayStartEquity, spyClose, spyPrevClose float64, openPositions int) (Record, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.st.LastDate == date {
		return Record{}, fmt.Errorf("date %s already recorded", date)
	}
	if dayStartEquity <= 0 {
		dayStartEquity = equity
	}
	if t.st.StartEquity <= 0 {
		t.st.StartDate, t.st.StartEquity, t.st.StartSPY = date, dayStartEquity, spyPrevClose
	}

	r := Record{
		Date:          date,
		Equity:        equity,
		DayPnLPct:     pct(equity, dayStartEquity),
		SPYDayPct:     pct(spyClose, spyPrevClose),
		CumPnLPct:     pct(equity, t.st.StartEquity),
		SPYCumPct:     pct(spyClose, t.st.StartSPY),
		OpenPositions: openPositions,
	}
	if equity > t.st.PeakEquity {
		t.st.PeakEquity = equity
	}
	if err := t.appendCSVLocked(r); err != nil {
		return Record{}, err
	}
	t.st.LastDate, t.st.Latest = date, &r
	if err := t.saveLocked(); err != nil {
		return r, err
	}
	return r, nil
}

func pct(now, base float64) float64 {
	if base <= 0 {
		return 0
	}
	return (now - base) / base * 100
}

func (t *Tracker) appendCSVLocked(r Record) error {
	_, statErr := os.Stat(t.csvPath())
	f, err := os.OpenFile(t.csvPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if os.IsNotExist(statErr) {
		w.Write([]string{"date", "equity", "day_pnl_pct", "spy_day_pct", "cum_pnl_pct", "spy_cum_pct", "open_positions"})
	}
	f2 := func(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
	w.Write([]string{r.Date, f2(r.Equity), f2(r.DayPnLPct), f2(r.SPYDayPct), f2(r.CumPnLPct), f2(r.SPYCumPct), strconv.Itoa(r.OpenPositions)})
	w.Flush()
	return w.Error()
}

// saveLocked writes state atomically (temp file + rename).
func (t *Tracker) saveLocked() error {
	data, err := json.MarshalIndent(t.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := t.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, t.statePath())
}

// FormatLine renders the one-line daily log message.
func (r Record) FormatLine() string {
	return fmt.Sprintf("[Stats] %s equity=$%.2f day=%+.2f%% (SPY %+.2f%%) cum=%+.2f%% (SPY %+.2f%%) positions=%d",
		r.Date, r.Equity, r.DayPnLPct, r.SPYDayPct, r.CumPnLPct, r.SPYCumPct, r.OpenPositions)
}

// Handler serves GET /stats with the latest daily record and baselines.
func (t *Tracker) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.mu.Lock()
		out := struct {
			Latest      *Record `json:"latest"`
			StartDate   string  `json:"start_date,omitempty"`
			StartEquity float64 `json:"start_equity"`
			PeakEquity  float64 `json:"peak_equity"`
			ServedAt    string  `json:"served_at"`
		}{t.st.Latest, t.st.StartDate, t.st.StartEquity, t.st.PeakEquity, time.Now().UTC().Format(time.RFC3339)}
		t.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
}
