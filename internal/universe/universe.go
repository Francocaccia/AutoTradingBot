// Package universe builds the per-cycle list of tradable symbols: the core
// watchlist plus a few screened candidates. Every symbol must pass hard
// filters, enforced here in code, before it is shown to Gemini or traded.
package universe

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/francocaccia/quantengine/internal/broker"
	"github.com/francocaccia/quantengine/internal/indicators"
)

const (
	MinPrice     = 5.0
	MinAvgVolume = 1_000_000.0

	rejectTTL       = time.Hour
	maxPending      = 20
	checksPerSlot   = 4 // validation attempts allowed per candidate slot
	screenerPerList = 10
)

var allowedExchanges = map[string]bool{"NYSE": true, "NASDAQ": true, "ARCA": true}

var symbolRe = regexp.MustCompile(`^[A-Z]{1,5}$`)

// Asset is the subset of Alpaca's /v2/assets/{symbol} the filters use.
type Asset struct {
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Class    string `json:"class"`
	Exchange string `json:"exchange"`
	Status   string `json:"status"`
	Tradable bool   `json:"tradable"`
}

// Assets looks up an Alpaca asset. A missing asset must return broker.ErrNotFound.
type Assets interface {
	GetAsset(symbol string) (*Asset, error)
}

// Screener supplies candidate symbols from market-wide screens.
type Screener interface {
	MostActives(top int) ([]string, error)
	Movers(top int) (gainers, losers []string, err error)
}

// Fetcher returns market data/indicators for a symbol (yfinance in production).
type Fetcher func(symbol string) (*indicators.Result, error)

// BrokerAssets adapts the Alpaca client to Assets.
type BrokerAssets struct{ Broker *broker.AlpacaClient }

func (b BrokerAssets) GetAsset(symbol string) (*Asset, error) {
	data, err := b.Broker.GetAsset(symbol)
	if err != nil {
		return nil, err
	}
	var a Asset
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// Item is a validated symbol with its freshly fetched indicators.
type Item struct {
	Symbol    string
	Ind       *indicators.Result
	Core      bool
	Validated bool // always true for items returned by Build
}

type Universe struct {
	Core          []string
	MaxCandidates int
	Assets        Assets
	Screener      Screener // optional
	Fetch         Fetcher
	// Prefetch optionally warms Fetch's cache for several symbols at once
	// (one batched market-data call per cycle). May be nil.
	Prefetch func(symbols []string)
	// Exempt lists symbols (the curated bond ETFs) that skip the price and
	// average-volume filters. The leveraged/inverse check still applies. May be nil.
	// Core watchlist symbols skip the average-volume filter too (price still applies).
	Exempt func(symbol string) bool
	// Held lists symbols currently held; a candidate whose other share class
	// is held is skipped. Set by the scheduler each cycle.
	Held []string

	now func() time.Time

	mu       sync.Mutex
	pending  []string             // Gemini suggestions for the next cycle
	rejected map[string]time.Time // symbol -> when the rejection expires
}

func New(core []string, maxCandidates int, assets Assets, screener Screener, fetch Fetcher) *Universe {
	return &Universe{
		Core: core, MaxCandidates: maxCandidates, Assets: assets, Screener: screener, Fetch: fetch,
		now: time.Now, rejected: map[string]time.Time{},
	}
}

// Suggest queues Gemini-suggested tickers for the NEXT cycle's candidate
// pool. They get no special treatment: they pass the same filters.
func (u *Universe) Suggest(symbols []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, s := range symbols {
		s = strings.ToUpper(strings.TrimSpace(s))
		if !symbolRe.MatchString(s) || contains(u.pending, s) || len(u.pending) >= maxPending {
			continue
		}
		u.pending = append(u.pending, s)
	}
}

// Build validates the core watchlist and then up to MaxCandidates extra
// symbols drawn from Gemini suggestions, the screener and news. Symbols that
// fail validation are logged and omitted.
func (u *Universe) Build(newsSymbols []string) []Item {
	var items []Item
	seen := map[string]bool{}
	represented := map[string]bool{} // symbols in the core list, accepted, or held
	for _, sym := range u.Core {
		represented[sym] = true
	}
	for _, sym := range u.Held {
		represented[sym] = true
	}
	for _, sym := range u.Core {
		seen[sym] = true
		if ind, err := u.Validate(sym); err != nil {
			log.Printf("[Universe] core %s skipped: %v", sym, err)
		} else {
			items = append(items, Item{Symbol: sym, Ind: ind, Core: true, Validated: true})
		}
	}

	if u.MaxCandidates <= 0 {
		return items
	}

	// Cheap Alpaca-side checks first, so only plausible candidates reach the
	// (batched) market-data fetch.
	assets := map[string]*Asset{}
	var plausible []string
	checks := 0
	for _, sym := range u.candidatePool(newsSymbols) {
		if seen[sym] {
			continue
		}
		seen[sym] = true
		if u.isRejected(sym) {
			continue
		}
		if duplicateClass(sym, represented) {
			log.Printf("[Universe] candidate %s skipped: another share class is already represented", sym)
			continue
		}
		if checks >= u.MaxCandidates*checksPerSlot {
			break
		}
		checks++
		if _, err := u.checkedAsset(sym, assets); err != nil {
			log.Printf("[Universe] candidate %s rejected: %v", sym, err)
			u.reject(sym)
			continue
		}
		plausible = append(plausible, sym)
	}
	if u.Prefetch != nil && len(plausible) > 0 {
		u.Prefetch(plausible)
	}

	added := 0
	for _, sym := range plausible {
		if added >= u.MaxCandidates {
			break
		}
		if duplicateClass(sym, represented) { // a sibling was accepted earlier in this loop
			continue
		}
		ind, err := u.validateMarket(sym, assets[sym])
		if err != nil {
			log.Printf("[Universe] candidate %s rejected: %v", sym, err)
			u.reject(sym)
			continue
		}
		log.Printf("[Universe] candidate %s accepted (price $%.2f, avg vol %.1fM)", sym, ind.Price, ind.AvgVolume/1e6)
		items = append(items, Item{Symbol: sym, Ind: ind, Validated: true})
		represented[sym] = true
		added++
	}
	return items
}

// shareClasses lists tickers that are different share classes of one issuer.
// Only one per group is added as a candidate.
var shareClasses = [][]string{
	{"GOOG", "GOOGL"}, {"FOX", "FOXA"}, {"NWS", "NWSA"}, {"UA", "UAA"}, {"Z", "ZG"}, {"LBTYA", "LBTYB", "LBTYK"},
}

func shareClassGroup(sym string) []string {
	for _, g := range shareClasses {
		if contains(g, sym) {
			return g
		}
	}
	return nil
}

// duplicateClass reports whether another share class of sym is already in held.
func duplicateClass(sym string, held map[string]bool) bool {
	for _, other := range shareClassGroup(sym) {
		if other != sym && held[other] {
			return true
		}
	}
	return false
}

// IsDerivativeSymbol flags 5-letter tickers ending in W, WS, R or U, the
// conventions for warrants, rights and units of SPACs.
func IsDerivativeSymbol(sym string) bool {
	if len(sym) != 5 {
		return false
	}
	return strings.HasSuffix(sym, "WS") || strings.HasSuffix(sym, "W") || strings.HasSuffix(sym, "R") || strings.HasSuffix(sym, "U")
}

var derivativeNameRe = regexp.MustCompile(`(?i)\b(warrants?|rights?|units?)\b`)

// Validate runs every hard filter and returns the symbol's indicators. The
// cheap Alpaca asset checks run before the (slow) market-data fetch. A data
// fetch failure is an error, so the symbol is skipped.
func (u *Universe) Validate(symbol string) (*indicators.Result, error) {
	asset, err := u.checkedAsset(symbol, nil)
	if err != nil {
		return nil, err
	}
	return u.validateMarket(symbol, asset)
}

// checkedAsset runs the symbol and Alpaca-side filters. cache (optional)
// remembers lookups so a symbol is only fetched from Alpaca once per Build.
func (u *Universe) checkedAsset(symbol string, cache map[string]*Asset) (*Asset, error) {
	if !symbolRe.MatchString(symbol) {
		return nil, fmt.Errorf("invalid symbol %q", symbol)
	}
	if IsDerivativeSymbol(symbol) {
		return nil, fmt.Errorf("%s looks like a warrant, right or unit", symbol)
	}
	asset, ok := cache[symbol]
	if !ok {
		var err error
		asset, err = u.Assets.GetAsset(symbol)
		if err != nil {
			if errors.Is(err, broker.ErrNotFound) {
				return nil, errors.New("asset does not exist on Alpaca")
			}
			return nil, fmt.Errorf("asset lookup failed: %w", err)
		}
		if cache != nil {
			cache[symbol] = asset
		}
	}
	if err := CheckAsset(asset); err != nil {
		return nil, err
	}
	return asset, nil
}

func (u *Universe) validateMarket(symbol string, asset *Asset) (*indicators.Result, error) {
	ind, err := u.Fetch(symbol)
	if err != nil {
		return nil, fmt.Errorf("market data fetch failed: %w", err)
	}
	// Screener candidates fail CLOSED on every filter, including unknown volume.
	// The curated bond ETFs and the core watchlist are known, liquid names: an
	// unavailable volume (Yahoo failing from a cloud IP) must not drop them.
	exempt := u.Exempt != nil && u.Exempt(symbol)
	check := CheckMarket
	switch {
	case exempt:
		check = CheckMarketExempt
	case u.isCore(symbol):
		check = CheckMarketCore
	}
	if err := check(asset, ind); err != nil {
		return nil, err
	}
	if ind.AvgVolume <= 0 && (exempt || u.isCore(symbol)) {
		log.Printf("[Universe] WARN %s: avg daily volume unavailable, keeping it (core watchlist / bond ETF sleeve are exempt from the liquidity filter)", symbol)
	}
	return ind, nil
}

func (u *Universe) isCore(symbol string) bool { return contains(u.Core, symbol) }

// CheckAsset applies the Alpaca-side filters: exists (non-nil), active,
// tradable, US equity class, NYSE/NASDAQ/ARCA, and no leveraged/inverse name.
func CheckAsset(a *Asset) error {
	if a == nil {
		return errors.New("asset does not exist")
	}
	if a.Status != "active" {
		return fmt.Errorf("asset status %q is not active", a.Status)
	}
	if !a.Tradable {
		return errors.New("asset is not tradable")
	}
	if a.Class != "" && a.Class != "us_equity" {
		return fmt.Errorf("asset class %q is not us_equity", a.Class)
	}
	if !allowedExchanges[strings.ToUpper(a.Exchange)] {
		return fmt.Errorf("exchange %q not in NYSE/NASDAQ/ARCA", a.Exchange)
	}
	if IsLeveragedOrInverse(a.Name, false) {
		return fmt.Errorf("leveraged/inverse product %q", a.Name)
	}
	if derivativeNameRe.MatchString(a.Name) {
		return fmt.Errorf("warrant, right or unit %q", a.Name)
	}
	return nil
}

// CheckMarket applies the price, liquidity and ETF-name filters to fetched data.
func CheckMarket(a *Asset, ind *indicators.Result) error {
	if ind == nil {
		return errors.New("no market data")
	}
	if ind.Price < MinPrice {
		return fmt.Errorf("price $%.2f below $%.0f", ind.Price, MinPrice)
	}
	if ind.AvgVolume <= 0 {
		return errors.New("avg daily volume unavailable")
	}
	if ind.AvgVolume < MinAvgVolume {
		return fmt.Errorf("avg daily volume %.0f below %.0f", ind.AvgVolume, MinAvgVolume)
	}
	isETF := ind.QuoteType != "" && !strings.EqualFold(ind.QuoteType, "EQUITY")
	for _, name := range []string{ind.Name, nameOf(a)} {
		if IsLeveragedOrInverse(name, isETF) {
			return fmt.Errorf("leveraged/inverse product %q", name)
		}
	}
	return nil
}

// CheckMarketCore is CheckMarket without the average-volume filter, for the
// core watchlist: a missing or low volume reading never drops a core symbol.
// The price and leveraged/inverse filters still apply.
func CheckMarketCore(a *Asset, ind *indicators.Result) error {
	if ind == nil {
		return errors.New("no market data")
	}
	if ind.Price < MinPrice {
		return fmt.Errorf("price $%.2f below $%.0f", ind.Price, MinPrice)
	}
	isETF := ind.QuoteType != "" && !strings.EqualFold(ind.QuoteType, "EQUITY")
	for _, name := range []string{ind.Name, nameOf(a)} {
		if IsLeveragedOrInverse(name, isETF) {
			return fmt.Errorf("leveraged/inverse product %q", name)
		}
	}
	return nil
}

// CheckMarketExempt is CheckMarket without the price and liquidity filters,
// for the curated bond ETFs (T-bill ETFs trade near $100 and some trade
// lightly). Market data must exist and leveraged/inverse products still fail.
func CheckMarketExempt(a *Asset, ind *indicators.Result) error {
	if ind == nil {
		return errors.New("no market data")
	}
	if ind.Price <= 0 {
		return errors.New("no price")
	}
	isETF := ind.QuoteType != "" && !strings.EqualFold(ind.QuoteType, "EQUITY")
	for _, name := range []string{ind.Name, nameOf(a)} {
		if IsLeveragedOrInverse(name, isETF) {
			return fmt.Errorf("leveraged/inverse product %q", name)
		}
	}
	return nil
}

func nameOf(a *Asset) string {
	if a == nil {
		return ""
	}
	return a.Name
}

var (
	// leveragedStrong patterns are specific enough to apply to any name.
	leveragedStrong = regexp.MustCompile(`(?i)(^|[^\w])(-1x|-?[23]x)([^\w]|$)|ultra\s*(pro|short)|\binverse\b|\bleverag|\bdaily\s+(bull|bear)\b|\b(bull|bear)\s+[1-3]x\b|\bproshares\s+ultra\b`)
	// leveragedWeak patterns are common words, so they only apply to ETFs/ETNs.
	leveragedWeak = regexp.MustCompile(`(?i)\b(ultra|short|bear|bull|direxion)\b`)
)

// IsLeveragedOrInverse reports whether name looks like a leveraged or
// inverse fund. The looser word patterns only apply when isETF is true, so
// operating companies such as "Ultra Clean Holdings" are not caught.
func IsLeveragedOrInverse(name string, isETF bool) bool {
	if leveragedStrong.MatchString(name) {
		return true
	}
	return isETF && leveragedWeak.MatchString(name)
}

// candidatePool merges Gemini suggestions (first), then an interleave of the
// screener lists and news tickers, de-duplicated.
func (u *Universe) candidatePool(newsSymbols []string) []string {
	u.mu.Lock()
	suggested := u.pending
	u.pending = nil
	u.mu.Unlock()

	var lists [][]string
	if u.Screener != nil {
		if actives, err := u.Screener.MostActives(screenerPerList); err != nil {
			log.Printf("[Universe] most-actives screener failed: %v", err)
		} else {
			lists = append(lists, actives)
		}
		if gainers, losers, err := u.Screener.Movers(screenerPerList); err != nil {
			log.Printf("[Universe] movers screener failed: %v", err)
		} else {
			lists = append(lists, gainers, losers)
		}
	}
	lists = append(lists, newsSymbols)

	pool := append([]string{}, suggested...)
	for i := 0; ; i++ {
		more := false
		for _, l := range lists {
			if i < len(l) {
				pool = append(pool, l[i])
				more = true
			}
		}
		if !more {
			break
		}
	}

	var out []string
	seen := map[string]bool{}
	for _, s := range pool {
		s = strings.ToUpper(strings.TrimSpace(s))
		if symbolRe.MatchString(s) && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (u *Universe) isRejected(sym string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	exp, ok := u.rejected[sym]
	if ok && u.now().After(exp) {
		delete(u.rejected, sym)
		return false
	}
	return ok
}

func (u *Universe) reject(sym string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rejected[sym] = u.now().Add(rejectTTL)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
