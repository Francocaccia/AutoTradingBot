package scheduler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/trades"
	"github.com/francocaccia/quantengine/internal/universe"
)

type fakeHistory struct{ fills []trades.Fill }

func (f fakeHistory) Fills(time.Time) ([]trades.Fill, error) { return f.fills, nil }
func (f fakeHistory) TradingDays(_, _ time.Time) ([]string, error) {
	return testDays, nil
}

// alpacaStub serves the endpoints runMarketCycle reads.
type alpacaStub struct {
	open        bool
	tradingDay  bool
	cash        float64
	positionsJS string
	failAccount bool
}

func (a *alpacaStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v2/clock":
		fmt.Fprintf(w, `{"timestamp":"2026-10-15T11:00:00-04:00","is_open":%v}`, a.open)
	case "/v2/calendar":
		if a.tradingDay {
			w.Write([]byte(`[{"date":"2026-10-15","open":"09:30","close":"16:00"}]`))
		} else {
			w.Write([]byte(`[]`))
		}
	case "/v2/account":
		if a.failAccount {
			http.Error(w, `{"message":"down"}`, http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"equity":"100000","last_equity":"100000","cash":"%.0f","buying_power":"%.0f"}`, a.cash, a.cash)
	case "/v2/positions":
		w.Write([]byte(a.positionsJS))
	default:
		http.NotFound(w, r)
	}
}

const aaplPositions = `[{"symbol":"AAPL","qty":"280","avg_entry_price":"100","market_value":"28000","unrealized_plpc":"0"}]`

// cycleScheduler builds a scheduler that runs runMarketCycle against the stub.
func cycleScheduler(t *testing.T, stub *alpacaStub, mode string, fills ...trades.Fill) (*Scheduler, *fakeOrders) {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	client := broker.NewAlpacaClient("k", "s", srv.URL)
	cfg := baseConfig()
	cfg.IPSBandsMode = mode
	cfg.Broker = client
	cfg.Portfolio = portfolio.NewPortfolio(client)
	cfg.History = fakeHistory{fills: fills}
	cfg.Universe = universe.New([]string{"SPY", "QQQ", "AAPL"}, 0, nil, nil, nil)
	cfg.Macro = func() (indicators.Macro, error) { return nil, errTest }
	s := New(cfg)
	o := &fakeOrders{}
	s.orders = o
	s.acct = cfg.Portfolio
	s.fetchBond = func(string) (*indicators.Result, error) { return ind(), nil }
	s.geminiPaused = func() (bool, string) { return true, "daily quota exhausted" }
	return s, o
}

// Every cycle ends with exactly one [Review] line, whatever happened in it.
func TestEveryCycleLogsExactlyOneReviewLine(t *testing.T) {
	cases := []struct {
		name string
		stub *alpacaStub
		want []string
	}{
		{"market closed", &alpacaStub{open: false, tradingDay: true, cash: 72_000, positionsJS: aaplPositions},
			[]string{"skip=market closed", "equities 28.0% (band 40-75%, below min)", "cash 72.0% (band 5-30%, above max)", "action=none"}},
		{"holiday", &alpacaStub{open: false, tradingDay: false, cash: 72_000, positionsJS: aaplPositions},
			[]string{"skip=market closed"}},
		{"account down", &alpacaStub{open: true, tradingDay: true, failAccount: true},
			[]string{"skip=account unavailable", "allocation unavailable"}},
		{"gemini paused, policy trade made", &alpacaStub{open: true, tradingDay: true, cash: 72_000, positionsJS: aaplPositions},
			[]string{"action=rebalance", "skip=-", "mode=gradual", "regime=unavailable"}},
		{"in band, nothing to do", &alpacaStub{open: true, tradingDay: true, cash: 30_000,
			positionsJS: `[{"symbol":"SPY","qty":"550","avg_entry_price":"100","market_value":"55000","unrealized_plpc":"0"},` +
				`{"symbol":"IEF","qty":"150","avg_entry_price":"100","market_value":"15000","unrealized_plpc":"0"}]`},
			[]string{"action=none", "Gemini paused (daily quota exhausted)", "equities 55.0% (band 40-75%, in band)"}},
	}
	for _, c := range cases {
		s, _ := cycleScheduler(t, c.stub, "")
		logs := captureSchedLog(t)
		s.runMarketCycle()
		lines := reviewLines(logs.String())
		if len(lines) != 1 {
			t.Errorf("%s: want exactly one [Review] line, got %d: %v\n%s", c.name, len(lines), lines, logs)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(lines[0], w) {
				t.Errorf("%s: line lacks %q:\n%s", c.name, w, lines[0])
			}
		}
		if !strings.Contains(lines[0], "ts=2026-10-15 11:00 EDT") {
			t.Errorf("%s: timestamp: %s", c.name, lines[0])
		}
	}
}

// Gemini being out of quota used to end the whole cycle; the policy trade does
// not need the model.
func TestPolicyTradeRunsEvenWhenGeminiIsPaused(t *testing.T) {
	s, o := cycleScheduler(t, &alpacaStub{open: true, tradingDay: true, cash: 72_000, positionsJS: aaplPositions}, "gradual")
	s.runMarketCycle()
	if len(o.placed) != 1 || o.placed[0].symbol != "SHY" && o.placed[0].symbol != "AGG" || o.placed[0].orderKind != broker.KindRebalance {
		t.Fatalf("orders = %+v", o.placed)
	}
	// The macro feed is down in this scenario, so only the bond ETFs that are safe to buy blind qualify.
	if o.placed[0].symbol != "SHY" {
		t.Errorf("without macro data only SHY may fill the bond sleeve, got %s", o.placed[0].symbol)
	}
}

func TestAdvisoryModePlacesNothingInAFullCycle(t *testing.T) {
	s, o := cycleScheduler(t, &alpacaStub{open: true, tradingDay: true, cash: 72_000, positionsJS: aaplPositions}, "advisory")
	logs := captureSchedLog(t)
	s.runMarketCycle()
	if len(o.placed) != 0 {
		t.Errorf("advisory placed %+v", o.placed)
	}
	lines := reviewLines(logs.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "would BUY") || !strings.Contains(lines[0], "mode=advisory") {
		t.Errorf("lines = %v", lines)
	}
}

func TestAHolidayStopsFurtherSlotsForTheDay(t *testing.T) {
	s, _ := cycleScheduler(t, &alpacaStub{open: false, tradingDay: false, cash: 72_000, positionsJS: aaplPositions}, "")
	s.runMarketCycle()
	if s.skipDay != "2026-10-15" {
		t.Errorf("skipDay = %q", s.skipDay)
	}
	// A normal closed-but-trading-day moment (before the open) does not skip the day.
	s2, _ := cycleScheduler(t, &alpacaStub{open: false, tradingDay: true, cash: 72_000, positionsJS: aaplPositions}, "")
	s2.runMarketCycle()
	if s2.skipDay != "" {
		t.Errorf("skipDay = %q for a trading day that is merely not open yet", s2.skipDay)
	}
}

// Cycles driven by tick: the catch-up after a sleep runs the real cycle once.
func TestTickRunsTheRealCycleOncePerSlot(t *testing.T) {
	s, o := cycleScheduler(t, &alpacaStub{open: true, tradingDay: true, cash: 72_000, positionsJS: aaplPositions}, "gradual")
	s.fetchNews = func() {}
	s.runCycle = s.runMarketCycle
	logs := captureSchedLog(t)
	s.tick(et("2026-10-15", "11:02"))
	s.tick(et("2026-10-15", "11:03"))
	s.tick(et("2026-10-15", "11:14"))
	if n := len(reviewLines(logs.String())); n != 1 {
		t.Errorf("review lines in one slot = %d, want 1", n)
	}
	if len(o.placed) != 1 {
		t.Errorf("orders = %+v", o.placed)
	}
}
