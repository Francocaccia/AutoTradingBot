package market

import (
	"testing"
	"time"
)

func TestEntryWindowOpen(t *testing.T) {
	open, closeT := time.Date(2026, 10, 12, 9, 30, 0, 0, eastern), time.Date(2026, 10, 12, 16, 0, 0, 0, eastern)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 12, h, m, 0, 0, eastern) }
	buf := 10 * time.Minute
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"at the open", at(9, 30), false},
		{"9:39", at(9, 39), false},
		{"9:40", at(9, 40), true},
		{"midday", at(12, 0), true},
		{"15:49", at(15, 49), true},
		{"15:50", at(15, 50), false},
		{"at the close", at(16, 0), false},
	}
	for _, c := range cases {
		if got := EntryWindowOpen(c.now, open, closeT, buf); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
