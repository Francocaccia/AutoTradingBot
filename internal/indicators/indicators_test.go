package indicators

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	barsJSON  = `{"AAA":{"price":10,"rsi":55,"avg_volume":90000,"source":"alpaca-iex"},"BBB":{"error":"not enough data for BBB (10 bars)"}}`
	fundsJSON = `{"AAA":{"beta":1.1,"pe_ratio":20,"avg_volume":5000000,"sector":"Tech","name":"AAA Inc","quote_type":"EQUITY"},"BBB":{"error":"Too Many Requests"}}`
)

type call struct{ args []string }

func newTestProvider(bars, funds string) (*Provider, *[]call, *time.Time) {
	var calls []call
	now := time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)
	p := NewProvider(func(args ...string) ([]byte, []byte, error) {
		calls = append(calls, call{args})
		if args[0] == "bars" {
			return []byte(bars), nil, nil
		}
		return []byte(funds), nil, nil
	})
	p.now = func() time.Time { return now }
	return p, &calls, &now
}

func TestPrefetchBatchesAndCaches(t *testing.T) {
	p, calls, now := newTestProvider(barsJSON, fundsJSON)
	p.Prefetch([]string{"AAA", "BBB", "AAA"})
	if len(*calls) != 2 || strings.Join((*calls)[0].args, " ") != "bars AAA BBB" {
		t.Fatalf("want one batched bars call and one fundamentals call, got %v", *calls)
	}

	res, err := p.Calculate("AAA")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sector != "Tech" || res.Beta != 1.1 || res.Price != 10 {
		t.Fatalf("bad merge: %+v", res)
	}
	if res.AvgVolume != 5_000_000 {
		t.Fatalf("IEX bars must take average volume from fundamentals, got %v", res.AvgVolume)
	}
	if _, err := p.Calculate("BBB"); err == nil || !strings.Contains(err.Error(), "not enough data") {
		t.Fatalf("want per-symbol error, got %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("Calculate within TTL must hit the cache, calls=%d", len(*calls))
	}

	// Technicals expire after 10 minutes; fundamentals live for 24 hours.
	*now = now.Add(11 * time.Minute)
	p.Prefetch([]string{"AAA"})
	if len(*calls) != 3 || (*calls)[2].args[0] != "bars" {
		t.Fatalf("only bars should refresh after 11m, calls=%v", *calls)
	}
	*now = now.Add(25 * time.Hour)
	p.Prefetch([]string{"AAA"})
	if len(*calls) != 5 {
		t.Fatalf("both should refresh after 25h, calls=%d", len(*calls))
	}
}

func TestFundamentalsFailureIsNonFatalButZeroesIEXVolume(t *testing.T) {
	p, _, _ := newTestProvider(barsJSON, `{"error":"Too Many Requests"}`)
	res, err := p.Calculate("AAA")
	if err != nil {
		t.Fatalf("technicals must survive a fundamentals failure: %v", err)
	}
	if res.AvgVolume != 0 || res.Sector != "" {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestYfinanceBarsKeepOwnAverageVolume(t *testing.T) {
	p, _, _ := newTestProvider(`{"AAA":{"price":10,"avg_volume":2000000,"source":"yfinance"}}`, fundsJSON)
	res, err := p.Calculate("AAA")
	if err != nil || res.AvgVolume != 2_000_000 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestParseFailureReportsRawOutput(t *testing.T) {
	p := NewProvider(func(args ...string) ([]byte, []byte, error) {
		return []byte("Traceback garbage"), []byte("yfinance warning"), nil
	})
	if _, err := p.Calculate("AAA"); err == nil || !strings.Contains(err.Error(), "failed to parse indicators") {
		t.Fatalf("got %v", err)
	}
}

func TestWorkerErrorObjectAndFailureCache(t *testing.T) {
	n := 0
	p := NewProvider(func(args ...string) ([]byte, []byte, error) {
		n++
		return []byte(`{"error":"RuntimeError: boom"}`), []byte("warn"), nil
	})
	if _, err := p.Calculate("AAA"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
	before := n
	p.Calculate("AAA")
	if n != before {
		t.Fatal("failure should be cached briefly")
	}
}

func TestRunnerErrorWithoutOutput(t *testing.T) {
	p := NewProvider(func(args ...string) ([]byte, []byte, error) { return nil, nil, errors.New("exit status 1") })
	if _, err := p.Calculate("AAA"); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("got %v", err)
	}
}

func TestSnippetTruncatesTo300(t *testing.T) {
	if got := snippet([]byte(strings.Repeat("x", 500))); len(got) != 300 {
		t.Fatalf("len=%d", len(got))
	}
}
