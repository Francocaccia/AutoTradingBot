package indicators

import (
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	barsJSON  = `{"AAA":{"price":10,"rsi":55,"avg_volume":90000,"source":"alpaca-iex"},"BBB":{"error":"not enough data for BBB (10 bars)"}}`
	fundsJSON = `{"AAA":{"beta":1.1,"pe_ratio":20,"avg_volume":5000000,"sector":"Tech","name":"AAA Inc","quote_type":"EQUITY"},"BBB":{"beta":1,"avg_volume":1000}}`
)

type call struct{ args []string }

func newTestProvider(bars, funds string) (*Provider, *[]call, *time.Time) {
	var calls []call
	now := time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)
	p := newFastProvider(func(args ...string) ([]byte, []byte, error) {
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

	// Technicals expire after 10 minutes; fundamentals live for 12 hours.
	*now = now.Add(11 * time.Minute)
	p.Prefetch([]string{"AAA"})
	if len(*calls) != 3 || (*calls)[2].args[0] != "bars" {
		t.Fatalf("only bars should refresh after 11m, calls=%v", *calls)
	}
	*now = now.Add(13 * time.Hour)
	p.Prefetch([]string{"AAA"})
	if len(*calls) != 5 {
		t.Fatalf("both should refresh after 13h, calls=%d", len(*calls))
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
	p := newFastProvider(func(args ...string) ([]byte, []byte, error) {
		return []byte("Traceback garbage"), []byte("yfinance warning"), nil
	})
	if _, err := p.Calculate("AAA"); err == nil || !strings.Contains(err.Error(), "failed to parse indicators") {
		t.Fatalf("got %v", err)
	}
}

func TestWorkerErrorObjectAndFailureCache(t *testing.T) {
	n := 0
	p := newFastProvider(func(args ...string) ([]byte, []byte, error) {
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
	p := newFastProvider(func(args ...string) ([]byte, []byte, error) { return nil, nil, errors.New("exit status 1") })
	if _, err := p.Calculate("AAA"); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("got %v", err)
	}
}

func TestSnippetTruncatesTo300(t *testing.T) {
	if got := snippet([]byte(strings.Repeat("x", 500))); len(got) != 300 {
		t.Fatalf("len=%d", len(got))
	}
}

// newFastProvider is NewProvider without the retry backoff sleeps.
func newFastProvider(run Runner) *Provider {
	p := NewProvider(run)
	p.sleep = func(time.Duration) {}
	return p
}

func TestFundamentalsRetryWithBackoffThenSucceed(t *testing.T) {
	var fundCalls int
	var sleeps []time.Duration
	p := NewProvider(func(args ...string) ([]byte, []byte, error) {
		if args[0] == "bars" {
			return []byte(barsJSON), nil, nil
		}
		fundCalls++
		if fundCalls < 3 {
			return []byte(`{"error":"Too Many Requests"}`), nil, nil
		}
		return []byte(fundsJSON), nil, nil
	})
	p.sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	res, err := p.Calculate("AAA")
	if err != nil || res.AvgVolume != 5_000_000 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// 2s and 5s backoff, each plus up to 50% jitter.
	if fundCalls != 3 || len(sleeps) != 2 ||
		sleeps[0] < 2*time.Second || sleeps[0] > 3*time.Second ||
		sleeps[1] < 5*time.Second || sleeps[1] > 7500*time.Millisecond {
		t.Errorf("fundamentals calls=%d sleeps=%v, want 3 attempts with 2s/5s backoff plus up to 50%% jitter", fundCalls, sleeps)
	}
}

func TestFundamentalsGiveUpAfterThreeAttempts(t *testing.T) {
	var fundCalls int
	p := newFastProvider(func(args ...string) ([]byte, []byte, error) {
		if args[0] == "bars" {
			return []byte(barsJSON), nil, nil
		}
		fundCalls++
		return []byte(`{"error":"Too Many Requests"}`), nil, nil
	})
	if _, err := p.Calculate("AAA"); err != nil {
		t.Fatal(err)
	}
	if fundCalls != 3 {
		t.Errorf("fundamentals attempts = %d, want 3", fundCalls)
	}
}

// When Yahoo fails on refresh, the last good average volume (< 24h old) is used.
func TestLastGoodAvgVolumeIsFallbackFor24h(t *testing.T) {
	failing := false
	now := time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)
	p := newFastProvider(func(args ...string) ([]byte, []byte, error) {
		if args[0] == "bars" {
			return []byte(barsJSON), nil, nil
		}
		if failing {
			return []byte(`{"error":"Too Many Requests"}`), nil, nil
		}
		return []byte(fundsJSON), nil, nil
	})
	p.now = func() time.Time { return now }
	if res, _ := p.Calculate("AAA"); res.AvgVolume != 5_000_000 || res.VolumeSource != "yahoo" {
		t.Fatalf("first fetch: %+v", res)
	}

	failing = true
	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	now = now.Add(13 * time.Hour) // fundamentals expired and the refresh fails
	res, err := p.Calculate("AAA")
	if err != nil || res.AvgVolume != 5_000_000 {
		t.Fatalf("a failed refresh must fall back to the last good volume: %+v %v", res, err)
	}
	if res.VolumeSource != "cache" || !strings.Contains(logs.String(), "using the cached value") {
		t.Errorf("a cached volume must be labelled and logged: source=%q logs=%q", res.VolumeSource, logs.String())
	}
	if res.Sector != "" {
		t.Errorf("only the volume falls back, got %+v", res)
	}

	now = now.Add(12 * time.Hour) // 25h since the last good value
	if res, _ := p.Calculate("AAA"); res.AvgVolume != 0 {
		t.Errorf("a fallback older than 24h must not be used: %+v", res)
	}
}

func TestCandidateMetricsAndMarketCapAreParsed(t *testing.T) {
	bars := `{"AAA":{"price":12.5,"atr":1,"avg_dollar_volume":42000000,"day_change_pct":-51.9,"range_pct":18.5,"source":"alpaca-iex"}}`
	funds := `{"AAA":{"avg_volume":5000000,"market_cap":3000000000,"quote_type":"EQUITY"}}`
	p, _, _ := newTestProvider(bars, funds)
	res, err := p.Calculate("AAA")
	if err != nil {
		t.Fatal(err)
	}
	if res.AvgDollarVolume != 42_000_000 || res.DayChangePct != -51.9 || res.RangePct != 18.5 || res.MarketCap != 3e9 {
		t.Errorf("metrics = %+v", res)
	}
}
