package portfolio

import (
	"fmt"
	"math"
	"strings"
)

// Policy is the investment policy statement: allocation bands per sleeve and
// concentration limits, all as % of account equity. It is enforced in code.
type Policy struct {
	EquityMin, EquityMax float64 // single stocks + equity ETFs
	BondMin, BondMax     float64 // bond ETFs (non-T-bill)
	CashMin, CashMax     float64 // cash + T-bill ETFs

	MaxStockPct    float64 // single stock or sector / thematic equity ETF
	MaxBroadETFPct float64 // single diversified index ETF (SPY, QQQ...): it is not a single-name bet
	MaxBondETFPct  float64 // single bond ETF (T-bill ETFs included)
	SectorMaxPct   float64 // one sector of the equity sleeve
	CreditMaxPct   float64 // all credit-risk bond ETFs combined
	MaxDuration    float64 // portfolio weighted bond duration, years
	MinRealCash    float64 // always keep this much actual cash, % of equity

	// Target weights per sleeve, used to size bond purchases. The weekly
	// review may move them inside the bands.
	TargetEquity, TargetBond, TargetCash float64
}

// DefaultPolicy returns the default IPS: 40-75% equities, 15-40% bonds,
// 5-30% cash / T-bills, with a 60/30/10 target.
func DefaultPolicy() Policy {
	return Policy{
		EquityMin: 40, EquityMax: 75,
		BondMin: 15, BondMax: 40,
		CashMin: 5, CashMax: 30,
		MaxStockPct: 10, MaxBroadETFPct: 20, MaxBondETFPct: 15, SectorMaxPct: 30, CreditMaxPct: 10, MaxDuration: 8, MinRealCash: 2,
		TargetEquity: 60, TargetBond: 30, TargetCash: 10,
	}
}

// Validate checks the bands are coherent: min <= max, the minimums fit in 100%
// and the maximums can reach it, and the targets lie inside the bands.
func (p Policy) Validate() error {
	for _, b := range []struct {
		name   string
		lo, hi float64
		target float64
		hasTgt bool
	}{
		{"equity", p.EquityMin, p.EquityMax, p.TargetEquity, true},
		{"bond", p.BondMin, p.BondMax, p.TargetBond, true},
		{"cash", p.CashMin, p.CashMax, p.TargetCash, true},
	} {
		if b.lo < 0 || b.hi > 100 || b.lo > b.hi {
			return fmt.Errorf("%s band %.0f-%.0f is invalid", b.name, b.lo, b.hi)
		}
		if b.hasTgt && (b.target < b.lo-1e-9 || b.target > b.hi+1e-9) {
			return fmt.Errorf("%s target %.1f outside band %.0f-%.0f", b.name, b.target, b.lo, b.hi)
		}
	}
	if p.EquityMin+p.BondMin+p.CashMin > 100 || p.EquityMax+p.BondMax+p.CashMax < 100 {
		return fmt.Errorf("bands cannot sum to 100%% (mins %.0f, maxes %.0f)", p.EquityMin+p.BondMin+p.CashMin, p.EquityMax+p.BondMax+p.CashMax)
	}
	if math.Abs(p.TargetEquity+p.TargetBond+p.TargetCash-100) > 1 {
		return fmt.Errorf("sleeve targets sum to %.1f, want 100", p.TargetEquity+p.TargetBond+p.TargetCash)
	}
	return nil
}

// Band returns the min/max of a sleeve.
func (p Policy) Band(sleeve string) (lo, hi float64) {
	switch sleeve {
	case SleeveBonds:
		return p.BondMin, p.BondMax
	case SleeveCash:
		return p.CashMin, p.CashMax
	}
	return p.EquityMin, p.EquityMax
}

// Target returns the target weight of a sleeve.
func (p Policy) Target(sleeve string) float64 {
	switch sleeve {
	case SleeveBonds:
		return p.TargetBond
	case SleeveCash:
		return p.TargetCash
	}
	return p.TargetEquity
}

// Holding is one open position.
type Holding struct {
	Symbol string
	Value  float64 // market value, dollars
	Sector string  // equities only; "" = unknown
}

// Book is the account as the policy sees it.
type Book struct {
	Equity   float64 // total account equity, dollars
	Cash     float64 // actual cash, dollars
	Holdings []Holding
}

// SleeveValues returns dollars per sleeve (cash includes T-bill ETFs).
func (b Book) SleeveValues() map[string]float64 {
	v := map[string]float64{SleeveEquities: 0, SleeveBonds: 0, SleeveCash: math.Max(b.Cash, 0)}
	for _, h := range b.Holdings {
		v[Sleeve(h.Symbol)] += h.Value
	}
	return v
}

// SleeveWeights returns each sleeve as % of equity.
func (b Book) SleeveWeights() map[string]float64 {
	out := map[string]float64{}
	if b.Equity <= 0 {
		return out
	}
	for k, v := range b.SleeveValues() {
		out[k] = v / b.Equity * 100
	}
	return out
}

// SectorKey is the bucket a holding counts toward: unknown sectors share one
// "Unknown" bucket, so missing data never looks like free diversification.
func SectorKey(sym, sector string) string {
	if IsBroadETF(sym) {
		return BroadSector
	}
	if strings.TrimSpace(sector) == "" {
		return "Unknown"
	}
	return sector
}

// SectorValues sums equity-sleeve holdings by sector bucket.
func (b Book) SectorValues() map[string]float64 {
	out := map[string]float64{}
	for _, h := range b.Holdings {
		if Sleeve(h.Symbol) == SleeveEquities {
			out[SectorKey(h.Symbol, h.Sector)] += h.Value
		}
	}
	return out
}

// BondDuration returns the value-weighted duration of the non-T-bill bond ETFs
// held, and their total value.
func (b Book) BondDuration() (years, value float64) {
	var weighted float64
	for _, h := range b.Holdings {
		if info, ok := BondInfoFor(h.Symbol); ok && !info.CashLike {
			weighted += h.Value * info.Duration
			value += h.Value
		}
	}
	if value > 0 {
		years = weighted / value
	}
	return years, value
}

func (b Book) holdingValue(sym string) float64 {
	var v float64
	for _, h := range b.Holdings {
		if h.Symbol == sym {
			v += h.Value
		}
	}
	return v
}

func (b Book) creditRiskValue() float64 {
	var v float64
	for _, h := range b.Holdings {
		if info, ok := BondInfoFor(h.Symbol); ok && info.CreditRisk() {
			v += h.Value
		}
	}
	return v
}

// Room returns the most dollars that may be added to symbol without breaching
// an IPS band or limit, and the name of the binding constraint. A room below
// one share's price means the BUY is rejected with that reason.
//
// sector is the symbol's sector for the equity sleeve ("" = unknown).
func (p Policy) Room(b Book, symbol, sector string) (room float64, binding string) {
	if b.Equity <= 0 {
		return 0, "account equity unknown"
	}
	pct := func(x float64) float64 { return b.Equity * x / 100 }
	room, binding = math.Inf(1), ""
	limit := func(v float64, name string) {
		if v < room {
			room, binding = v, name
		}
	}

	sleeve := Sleeve(symbol)
	sv := b.SleeveValues()
	info, isBond := BondInfoFor(symbol)

	// Real cash: buying always spends it, and a floor is kept.
	limit(b.Cash-pct(p.MinRealCash), fmt.Sprintf("real cash floor %.0f%% of equity", p.MinRealCash))

	// Cash sleeve floor: every purchase except a T-bill ETF (cash to cash) lowers it.
	if sleeve != SleeveCash {
		limit(sv[SleeveCash]-pct(p.CashMin), fmt.Sprintf("cash sleeve band minimum %.0f%%", p.CashMin))
	}

	// Own sleeve ceiling.
	switch sleeve {
	case SleeveEquities:
		limit(pct(p.EquityMax)-sv[SleeveEquities], fmt.Sprintf("equity band maximum %.0f%%", p.EquityMax))
	case SleeveBonds:
		limit(pct(p.BondMax)-sv[SleeveBonds], fmt.Sprintf("bond band maximum %.0f%%", p.BondMax))
	}

	// Single-name limits.
	cur := b.holdingValue(symbol)
	if isBond {
		limit(pct(p.MaxBondETFPct)-cur, fmt.Sprintf("single bond ETF limit %.0f%%", p.MaxBondETFPct))
	} else if IsBroadETF(symbol) {
		limit(pct(p.MaxBroadETFPct)-cur, fmt.Sprintf("single index ETF limit %.0f%%", p.MaxBroadETFPct))
	} else {
		limit(pct(p.MaxStockPct)-cur, fmt.Sprintf("single position limit %.0f%%", p.MaxStockPct))
	}

	// Sector cap for the equity sleeve (broad index ETFs exempt).
	if sleeve == SleeveEquities {
		if key := SectorKey(symbol, sector); key != BroadSector {
			limit(pct(p.SectorMaxPct)-b.SectorValues()[key], fmt.Sprintf("%s sector limit %.0f%%", key, p.SectorMaxPct))
		}
	}

	// Combined credit-risk limit.
	if isBond && info.CreditRisk() {
		limit(pct(p.CreditMaxPct)-b.creditRiskValue(), fmt.Sprintf("credit-risk bonds limit %.0f%%", p.CreditMaxPct))
	}

	// Portfolio duration cap, applied to the bond ETFs excluding T-bills.
	if isBond && !info.CashLike && info.Duration > p.MaxDuration {
		var weighted, value float64
		for _, h := range b.Holdings {
			if hi, ok := BondInfoFor(h.Symbol); ok && !hi.CashLike {
				weighted += h.Value * hi.Duration
				value += h.Value
			}
		}
		// (weighted + x*d) / (value + x) <= D  =>  x <= (D*value - weighted) / (d - D)
		limit((p.MaxDuration*value-weighted)/(info.Duration-p.MaxDuration),
			fmt.Sprintf("portfolio duration cap %.0f years", p.MaxDuration))
	}

	if room < 0 {
		room = 0
	}
	return room, binding
}

// Breach is the sleeve furthest outside its band.
type Breach struct {
	Sleeve string
	Weight float64 // current % of equity
	Limit  float64 // the band edge it crossed
	Above  bool    // true: over the max; false: under the min
	Pts    float64 // distance outside the band, percentage points; 0 = all inside
}

// Catalyst is the deterministic, code-built reason for a rebalancing trade.
func (b Breach) Catalyst() string {
	if b.Above {
		return fmt.Sprintf("band breach: %s %.1f%% > %.0f%% max", b.Sleeve, b.Weight, b.Limit)
	}
	return fmt.Sprintf("band breach: %s %.1f%% < %.0f%% min", b.Sleeve, b.Weight, b.Limit)
}

// WorstBreach returns the sleeve furthest outside its band (zero Pts if none).
func (p Policy) WorstBreach(b Book) Breach {
	w := b.SleeveWeights()
	var worst Breach
	for _, s := range []string{SleeveEquities, SleeveBonds, SleeveCash} {
		lo, hi := p.Band(s)
		var d, limit float64
		var above bool
		switch {
		case w[s] > hi:
			d, limit, above = w[s]-hi, hi, true
		case w[s] < lo:
			d, limit = lo-w[s], lo
		}
		if d > worst.Pts {
			worst = Breach{Sleeve: s, Weight: w[s], Limit: limit, Above: above, Pts: d}
		}
	}
	return worst
}

// BandBreach reports the largest distance, in percentage points, by which a
// sleeve lies outside its band, with a description. 0 means all inside.
func (p Policy) BandBreach(b Book) (pts float64, desc string) {
	w := p.WorstBreach(b)
	if w.Pts == 0 {
		return 0, ""
	}
	lo, hi := p.Band(w.Sleeve)
	return w.Pts, fmt.Sprintf("%s %.1f%% vs band %.0f-%.0f%%", w.Sleeve, w.Weight, lo, hi)
}

// ClipTargets forces proposed sleeve targets (% of equity) into the bands. It
// returns the clipped targets and ok=false when they do not sum to 100 (+-1)
// even after clipping, in which case the proposal must be rejected.
func (p Policy) ClipTargets(eq, bond, cash float64) (Policy, bool) {
	clip := func(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }
	eq, bond, cash = clip(eq, p.EquityMin, p.EquityMax), clip(bond, p.BondMin, p.BondMax), clip(cash, p.CashMin, p.CashMax)
	if math.Abs(eq+bond+cash-100) > 1 {
		return p, false
	}
	p.TargetEquity, p.TargetBond, p.TargetCash = eq, bond, cash
	return p, true
}
