package universe

import (
	"errors"
	"strings"
	"testing"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/indicators"
)

func goodAsset() *Asset {
	return &Asset{Symbol: "XYZ", Name: "XYZ Corp", Class: "us_equity", Exchange: "NASDAQ", Status: "active", Tradable: true}
}

func TestCheckAsset(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*Asset)
		want string // substring of the error; "" = accepted
	}{
		{"ok NASDAQ", func(a *Asset) {}, ""},
		{"ok NYSE", func(a *Asset) { a.Exchange = "NYSE" }, ""},
		{"ok ARCA", func(a *Asset) { a.Exchange = "ARCA" }, ""},
		{"OTC", func(a *Asset) { a.Exchange = "OTC" }, "exchange"},
		{"AMEX not allowed", func(a *Asset) { a.Exchange = "AMEX" }, "exchange"},
		{"inactive", func(a *Asset) { a.Status = "inactive" }, "not active"},
		{"not tradable", func(a *Asset) { a.Tradable = false }, "not tradable"},
		{"crypto class", func(a *Asset) { a.Class = "crypto" }, "class"},
		{"leveraged ETF", func(a *Asset) { a.Name = "ProShares UltraPro QQQ" }, "leveraged"},
		{"inverse ETF", func(a *Asset) { a.Name = "ProShares Short S&P500 Inverse ETF" }, "leveraged"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := goodAsset()
			tc.mod(a)
			err := CheckAsset(a)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	if CheckAsset(nil) == nil {
		t.Error("nil asset must be rejected")
	}
}

func TestCheckMarket(t *testing.T) {
	ok := func() *indicators.Result {
		return &indicators.Result{Price: 50, AvgVolume: 2_000_000, Name: "XYZ Corp", QuoteType: "EQUITY"}
	}
	tests := []struct {
		name string
		mod  func(*indicators.Result)
		want string
	}{
		{"ok", func(r *indicators.Result) {}, ""},
		{"price exactly $5", func(r *indicators.Result) { r.Price = 5 }, ""},
		{"price below $5", func(r *indicators.Result) { r.Price = 4.99 }, "price"},
		{"volume exactly 1M", func(r *indicators.Result) { r.AvgVolume = 1_000_000 }, ""},
		{"volume below 1M", func(r *indicators.Result) { r.AvgVolume = 999_999 }, "volume"},
		{"missing volume", func(r *indicators.Result) { r.AvgVolume = 0 }, "volume"},
		{"3x ETF", func(r *indicators.Result) { r.QuoteType = "ETF"; r.Name = "Direxion Daily S&P 500 Bull 3X Shares" }, "leveraged"},
		{"weak word on ETF", func(r *indicators.Result) { r.QuoteType = "ETF"; r.Name = "ProShares Short QQQ" }, "leveraged"},
		{"plain ETF ok", func(r *indicators.Result) { r.QuoteType = "ETF"; r.Name = "SPDR S&P 500 ETF Trust" }, ""},
		{"weak word on equity ok", func(r *indicators.Result) { r.Name = "Ultra Clean Holdings, Inc." }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := ok()
			tc.mod(r)
			err := CheckMarket(goodAsset(), r)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	if CheckMarket(goodAsset(), nil) == nil {
		t.Error("nil data must be rejected")
	}
}

func TestIsLeveragedOrInverse(t *testing.T) {
	yes := []string{"ProShares UltraShort QQQ", "ProShares Ultra QQQ", "GraniteShares 2x Long NVDA Daily ETF",
		"Direxion Daily TSLA Bear 1X Shares", "ProShares UltraPro QQQ", "Defiance Daily Target -2X Short ETF", "Inverse VIX Fund", "Leveraged Fund"}
	for _, n := range yes {
		if !IsLeveragedOrInverse(n, false) && !IsLeveragedOrInverse(n, true) {
			t.Errorf("%q should be flagged", n)
		}
	}
	no := []string{"Apple Inc.", "Invesco QQQ Trust", "Vanguard S&P 500 ETF", "Bullion Holdings", "Form 12x Industries"}
	for _, n := range no {
		if IsLeveragedOrInverse(n, true) {
			t.Errorf("%q should not be flagged", n)
		}
	}
}

// --- Build / Validate with fakes ---

type fakeAssets map[string]*Asset

func (f fakeAssets) GetAsset(s string) (*Asset, error) {
	if a, ok := f[s]; ok {
		return a, nil
	}
	return nil, broker.ErrNotFound
}

type fakeScreener struct {
	actives, gainers, losers []string
	err                      error
}

func (f fakeScreener) MostActives(int) ([]string, error) { return f.actives, f.err }
func (f fakeScreener) Movers(int) ([]string, []string, error) {
	return f.gainers, f.losers, f.err
}

func newTestUniverse(max int, assets fakeAssets, data map[string]*indicators.Result, sc Screener) (*Universe, *[]string) {
	var fetched []string
	fetch := func(s string) (*indicators.Result, error) {
		fetched = append(fetched, s)
		if r, ok := data[s]; ok {
			return r, nil
		}
		return nil, errors.New("no data")
	}
	return New([]string{"SPY"}, max, assets, sc, fetch), &fetched
}

func liquid(price float64) *indicators.Result {
	return &indicators.Result{Price: price, AvgVolume: 5_000_000, QuoteType: "EQUITY"}
}

func asset(sym, exch string) *Asset {
	return &Asset{Symbol: sym, Name: sym + " Inc", Class: "us_equity", Exchange: exch, Status: "active", Tradable: true}
}

func symbols(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Symbol)
	}
	return out
}

func TestBuildAppliesFiltersAndCap(t *testing.T) {
	assets := fakeAssets{
		"SPY": asset("SPY", "ARCA"), "AAA": asset("AAA", "NASDAQ"), "BBB": asset("BBB", "NYSE"),
		"OTCX": asset("OTCX", "OTC"), "PNNY": asset("PNNY", "NASDAQ"), "THIN": asset("THIN", "NYSE"),
		"NODA": asset("NODA", "NYSE"), "CCC": asset("CCC", "NASDAQ"),
	}
	data := map[string]*indicators.Result{
		"SPY": liquid(500), "AAA": liquid(20), "BBB": liquid(30), "PNNY": liquid(2),
		"THIN": {Price: 20, AvgVolume: 10_000, QuoteType: "EQUITY"}, "CCC": liquid(40),
	}
	sc := fakeScreener{actives: []string{"OTCX", "PNNY", "THIN", "GHOST", "NODA", "AAA"}, gainers: []string{"BBB"}}
	u, fetched := newTestUniverse(2, assets, data, sc)

	items := u.Build([]string{"CCC", "spy", "bad.sym"})
	got := symbols(items)
	if len(got) != 3 || got[0] != "SPY" || !items[0].Core {
		t.Fatalf("got %v, want core SPY plus 2 candidates", got)
	}
	for _, it := range items {
		if !it.Validated || it.Ind == nil {
			t.Errorf("%s returned without validation/data", it.Symbol)
		}
	}
	for _, bad := range []string{"OTCX", "PNNY", "THIN", "GHOST", "NODA"} {
		for _, g := range got {
			if g == bad {
				t.Errorf("%s must have been filtered out", bad)
			}
		}
	}
	// OTC and nonexistent assets must be rejected before any data fetch.
	for _, f := range *fetched {
		if f == "OTCX" || f == "GHOST" {
			t.Errorf("fetched data for %s that failed the asset check", f)
		}
	}
}

func TestBuildSkipsOnDataFailureAndCachesRejects(t *testing.T) {
	assets := fakeAssets{"SPY": asset("SPY", "ARCA"), "AAA": asset("AAA", "NASDAQ"), "BBB": asset("BBB", "NASDAQ")}
	data := map[string]*indicators.Result{"SPY": liquid(500), "BBB": liquid(30)} // AAA has no data
	u, fetched := newTestUniverse(5, assets, data, fakeScreener{actives: []string{"AAA", "BBB"}})

	got := symbols(u.Build(nil))
	if strings.Join(got, ",") != "SPY,BBB" {
		t.Fatalf("got %v, want [SPY BBB]", got)
	}
	n := len(*fetched)
	u.Build(nil)
	count := 0
	for _, f := range (*fetched)[n:] {
		if f == "AAA" {
			count++
		}
	}
	if count != 0 {
		t.Errorf("rejected AAA was re-fetched on the next cycle")
	}
}

func TestSuggestedTickersPassSameFilters(t *testing.T) {
	assets := fakeAssets{"SPY": asset("SPY", "ARCA"), "GOOD": asset("GOOD", "NYSE"), "SQQQ": {Symbol: "SQQQ", Name: "ProShares UltraPro Short QQQ", Class: "us_equity", Exchange: "NASDAQ", Status: "active", Tradable: true}}
	data := map[string]*indicators.Result{"SPY": liquid(500), "GOOD": liquid(60), "SQQQ": liquid(10)}
	u, _ := newTestUniverse(5, assets, data, nil)

	u.Suggest([]string{"good", "SQQQ", "NOPE", "not a ticker", "GOOD"})
	got := symbols(u.Build(nil))
	if strings.Join(got, ",") != "SPY,GOOD" {
		t.Fatalf("got %v, want [SPY GOOD]", got)
	}
	// Suggestions are one-shot: consumed by the cycle that used them.
	if got = symbols(u.Build(nil)); strings.Join(got, ",") != "SPY" {
		t.Fatalf("suggestion reused: %v", got)
	}
}

func TestCoreFailureIsSkippedNotTraded(t *testing.T) {
	assets := fakeAssets{"SPY": asset("SPY", "ARCA")}
	u, _ := newTestUniverse(0, assets, map[string]*indicators.Result{}, nil) // data fetch fails
	if items := u.Build(nil); len(items) != 0 {
		t.Fatalf("symbol without data must not be returned: %v", symbols(items))
	}
}

func TestScreenerFailureFallsBackToNews(t *testing.T) {
	assets := fakeAssets{"SPY": asset("SPY", "ARCA"), "NEWS": asset("NEWS", "NYSE")}
	data := map[string]*indicators.Result{"SPY": liquid(500), "NEWS": liquid(25)}
	u, _ := newTestUniverse(5, assets, data, fakeScreener{err: errors.New("boom")})
	if got := symbols(u.Build([]string{"NEWS"})); strings.Join(got, ",") != "SPY,NEWS" {
		t.Fatalf("got %v", got)
	}
}
