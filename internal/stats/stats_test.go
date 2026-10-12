package stats

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestRecordDayCumulativeAndPersistence(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Day 1 fixes the baselines: equity 100k, SPY 500.
	r1, err := tr.RecordDay("2026-10-12", 101_000, 100_000, 505, 500, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !near(r1.DayPnLPct, 1) || !near(r1.SPYDayPct, 1) || !near(r1.CumPnLPct, 1) || !near(r1.SPYCumPct, 1) {
		t.Errorf("day1 = %+v", r1)
	}

	// Day 2 accumulates against the same baselines.
	r2, err := tr.RecordDay("2026-10-13", 99_990, 101_000, 495, 505, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !near(r2.DayPnLPct, -1) || !near(r2.CumPnLPct, -0.01) || !near(r2.SPYCumPct, -1) || r2.OpenPositions != 4 {
		t.Errorf("day2 = %+v", r2)
	}

	if _, err := tr.RecordDay("2026-10-13", 1, 1, 1, 1, 0); err == nil {
		t.Error("duplicate date must be rejected")
	}
	if !tr.Recorded("2026-10-13") || tr.Recorded("2026-10-14") {
		t.Error("Recorded() wrong")
	}

	// CSV: header + 2 rows.
	data, _ := os.ReadFile(dir + "/performance.csv")
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n")), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "date,equity") || !strings.HasPrefix(lines[2], "2026-10-13,99990.0000") {
		t.Errorf("csv = %q", lines)
	}

	// State survives a restart, including baselines and the peak.
	tr2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !tr2.Recorded("2026-10-13") {
		t.Error("last date lost on reload")
	}
	r3, _ := tr2.RecordDay("2026-10-14", 102_010, 99_990, 500, 495, 2)
	if !near(r3.CumPnLPct, 2.01) || !near(r3.SPYCumPct, 0) {
		t.Errorf("day3 after reload = %+v", r3)
	}
	if got := tr2.ObservePeak(50); !near(got, 102_010) {
		t.Errorf("peak = %v", got)
	}
}

func TestObservePeakOnlyRises(t *testing.T) {
	tr, _ := New(t.TempDir())
	tr.ObservePeak(100)
	tr.ObservePeak(120)
	if got := tr.ObservePeak(110); got != 120 {
		t.Errorf("peak = %v, want 120", got)
	}
}

func TestHandler(t *testing.T) {
	tr, _ := New(t.TempDir())
	rec := httptest.NewRecorder()
	tr.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/stats", nil))
	var empty struct {
		Latest *Record `json:"latest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil || empty.Latest != nil {
		t.Fatalf("empty stats: %v %s", err, rec.Body)
	}

	tr.RecordDay("2026-10-12", 101_000, 100_000, 505, 500, 3)
	rec = httptest.NewRecorder()
	tr.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/stats", nil))
	var got struct {
		Latest     *Record `json:"latest"`
		PeakEquity float64 `json:"peak_equity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Latest == nil || got.Latest.Equity != 101_000 || got.PeakEquity != 101_000 {
		t.Fatalf("stats = %s", rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type %q", ct)
	}
}
