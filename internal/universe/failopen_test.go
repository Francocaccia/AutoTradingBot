package universe

import (
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/francocaccia/quantengine/internal/indicators"
)

type brokenAssets struct{}

func (brokenAssets) GetAsset(string) (*Asset, error) { return nil, errors.New("alpaca error 503") }

func spyData() *indicators.Result {
	return &indicators.Result{Price: 520, ATR: 6, Name: "SPDR S&P 500 ETF Trust", QuoteType: "ETF"}
}

// A data outage must not remove a core symbol from the universe.
func TestCoreSymbolFailsOpenWhenTheAssetLookupIsDown(t *testing.T) {
	logs := captureLog(t)
	u := New([]string{"SPY"}, 0, brokenAssets{}, nil, func(string) (*indicators.Result, error) { return spyData(), nil })
	items := u.Build(nil)
	if len(items) != 1 || items[0].Symbol != "SPY" || !items[0].Core {
		t.Fatalf("items = %+v", items)
	}
	if !strings.Contains(logs.String(), "WARN core SPY") {
		t.Errorf("failing open must warn:\n%s", logs)
	}
	// A screener candidate gets no such leniency.
	if _, err := u.Validate("CAND"); err == nil {
		t.Error("a non-core symbol still fails when the lookup is down")
	}
}

func TestCoreSymbolFailsOpenWhenVolumeIsUnknownAndDataIsStale(t *testing.T) {
	logs := captureLog(t)
	d := spyData()
	d.AvgVolume, d.StaleFor = 0, 12*time.Minute // Yahoo gone and the refresh failing
	u, _ := newTestUniverse(0, fakeAssets{"SPY": asset("SPY", "ARCA")}, map[string]*indicators.Result{"SPY": d}, nil)
	if items := u.Build(nil); len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	if !strings.Contains(logs.String(), "market data is 12m0s old") {
		t.Errorf("stale data must be flagged:\n%s", logs)
	}
}

// Real rejections still apply to core symbols; only outages are forgiven.
func TestCoreSymbolIsStillRejectedForRealReasons(t *testing.T) {
	captureLog(t)
	notTradable := asset("SPY", "ARCA")
	notTradable.Tradable = false
	u, _ := newTestUniverse(0, fakeAssets{"SPY": notTradable}, map[string]*indicators.Result{"SPY": spyData()}, nil)
	if items := u.Build(nil); len(items) != 0 {
		t.Errorf("an untradable core symbol was kept: %+v", items)
	}
	// No price data from any source: nothing to evaluate, so it is skipped, loudly.
	logs := captureLog(t)
	u, _ = newTestUniverse(0, fakeAssets{"SPY": asset("SPY", "ARCA")}, map[string]*indicators.Result{}, nil)
	if items := u.Build(nil); len(items) != 0 {
		t.Errorf("items = %+v", items)
	}
	if !strings.Contains(logs.String(), "core SPY skipped: market data fetch failed") {
		t.Errorf("logs:\n%s", logs)
	}
}

func captureLog(t *testing.T) *strings.Builder {
	t.Helper()
	var sb strings.Builder
	log.SetOutput(&sb)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &sb
}
