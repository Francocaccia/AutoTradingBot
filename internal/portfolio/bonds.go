package portfolio

import "strings"

// Sleeves of the investment policy statement.
const (
	SleeveEquities = "equities"
	SleeveBonds    = "bonds"
	SleeveCash     = "cash"
)

// BroadSector is the sector label of diversified equity index ETFs. It is
// exempt from the single-sector cap.
const BroadSector = "Broad Market ETF"

// Credit classes of the curated bond ETFs.
const (
	CreditNone = "none" // Treasuries, T-bills
	CreditIG   = "investment-grade"
	CreditHY   = "high-yield"
	CreditEM   = "emerging-market"
)

// BondInfo is the static metadata of one curated bond / defensive ETF.
// Duration is the approximate effective duration in years.
type BondInfo struct {
	Symbol   string
	Name     string
	Kind     string
	Duration float64
	Credit   string
	CashLike bool // T-bill ETFs: sleeve "cash", no bracket order
}

// CreditRisk reports whether the ETF carries meaningful credit risk (high
// yield or emerging-market sovereign); these share one combined limit.
func (b BondInfo) CreditRisk() bool { return b.Credit == CreditHY || b.Credit == CreditEM }

// defaultBonds is the curated bond universe. Durations are approximate and
// static; they only feed the portfolio duration cap.
var defaultBonds = []BondInfo{
	{"SGOV", "iShares 0-3 Month Treasury Bond", "cash-like", 0.1, CreditNone, true},
	{"BIL", "SPDR 1-3 Month T-Bill", "cash-like", 0.2, CreditNone, true},
	{"SHY", "iShares 1-3 Year Treasury", "short Treasury", 1.9, CreditNone, false},
	{"IEI", "iShares 3-7 Year Treasury", "short Treasury", 4.4, CreditNone, false},
	{"IEF", "iShares 7-10 Year Treasury", "intermediate Treasury", 7.5, CreditNone, false},
	{"TLT", "iShares 20+ Year Treasury", "long Treasury", 16.5, CreditNone, false},
	{"TIP", "iShares TIPS Bond", "inflation-protected", 6.5, CreditNone, false},
	{"AGG", "iShares Core US Aggregate Bond", "broad aggregate", 6.0, CreditNone, false},
	{"BND", "Vanguard Total Bond Market", "broad aggregate", 6.0, CreditNone, false},
	{"LQD", "iShares Investment Grade Corporate", "investment-grade corporate", 8.5, CreditIG, false},
	{"VCSH", "Vanguard Short-Term Corporate", "investment-grade corporate", 2.6, CreditIG, false},
	{"HYG", "iShares High Yield Corporate", "high yield", 3.5, CreditHY, false},
	{"EMB", "iShares JPMorgan EM Bond", "emerging-market sovereign", 7.0, CreditEM, false},
}

// SafeWithoutMacro are the only bond ETFs the bot may buy when macro data is
// unavailable: no meaningful duration or credit exposure to justify blind.
var SafeWithoutMacro = map[string]bool{"SGOV": true, "BIL": true, "SHY": true}

// broadETFs are diversified equity index ETFs (sector data is not available
// for ETFs, and they are not a concentrated sector bet).
var broadETFs = map[string]bool{"SPY": true, "QQQ": true, "VOO": true, "IVV": true, "VTI": true, "DIA": true, "IWM": true}

// IsBroadETF reports whether sym is a diversified equity index ETF.
func IsBroadETF(sym string) bool { return broadETFs[strings.ToUpper(sym)] }

// BondInfoFor returns the metadata of a curated bond ETF, whether or not the
// bot is currently allowed to buy it.
func BondInfoFor(sym string) (BondInfo, bool) {
	sym = strings.ToUpper(sym)
	for _, b := range defaultBonds {
		if b.Symbol == sym {
			return b, true
		}
	}
	return BondInfo{}, false
}

// Sleeve classifies a symbol: T-bill ETFs are cash, other curated bond ETFs
// are bonds, everything else (single stocks, equity ETFs) is equities.
func Sleeve(sym string) string {
	if b, ok := BondInfoFor(sym); ok {
		if b.CashLike {
			return SleeveCash
		}
		return SleeveBonds
	}
	return SleeveEquities
}

// BondUniverse is the set of bond ETFs the bot may buy. Empty (BOND_MODE=off)
// means none. Held positions are classified from the full static table either
// way, so a held SHY is still a bond.
type BondUniverse struct{ allowed map[string]BondInfo }

// NewBondUniverse enables the listed symbols; a nil or empty list enables the
// whole curated table. Unknown symbols are ignored.
func NewBondUniverse(symbols []string) *BondUniverse {
	u := &BondUniverse{allowed: map[string]BondInfo{}}
	if len(symbols) == 0 {
		for _, b := range defaultBonds {
			u.allowed[b.Symbol] = b
		}
		return u
	}
	for _, s := range symbols {
		if b, ok := BondInfoFor(strings.TrimSpace(s)); ok {
			u.allowed[b.Symbol] = b
		}
	}
	return u
}

// DisabledBondUniverse allows no bond ETF purchases (BOND_MODE=off).
func DisabledBondUniverse() *BondUniverse { return &BondUniverse{allowed: map[string]BondInfo{}} }

func (u *BondUniverse) Allowed(sym string) bool {
	if u == nil {
		return false
	}
	_, ok := u.allowed[strings.ToUpper(sym)]
	return ok
}

// Symbols lists the allowed ETFs in the curated order.
func (u *BondUniverse) Symbols() []string {
	if u == nil {
		return nil
	}
	var out []string
	for _, b := range defaultBonds {
		if _, ok := u.allowed[b.Symbol]; ok {
			out = append(out, b.Symbol)
		}
	}
	return out
}

// Infos lists the allowed ETFs' metadata in the curated order.
func (u *BondUniverse) Infos() []BondInfo {
	var out []BondInfo
	for _, s := range u.Symbols() {
		out = append(out, u.allowed[s])
	}
	return out
}
