package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/francocaccia/quantengine/internal/portfolio"
	"github.com/francocaccia/quantengine/internal/regime"
)

// Inputs is everything the PORTFOLIO block reports about the account.
type Inputs struct {
	Policy portfolio.Policy
	Book   portfolio.Book
	Regime regime.Regime

	Vol, Beta float64
	HaveRisk  bool

	DrawdownPct float64 // from the equity peak, positive = below peak
	HaveDD      bool

	// Returns since TRACK_START_DATE, percent; nil when not available yet.
	BotReturn, SPYReturn, Bench6040 *float64
}

// Candidate describes how one symbol would fit if bought.
type Candidate struct {
	Symbol string
	Sector string
	Corr   *Correlation // nil: unavailable
	AddPct float64      // hypothetical purchase, % of equity
	Room   float64      // dollars the IPS allows, from Policy.Room
	Bind   string       // binding constraint of Room
}

func pctStr(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", *v)
}

// Render builds the PORTFOLIO block. The model may quote any datapoint in it
// as a macro catalyst for a bond/ETF-sleeve trade, so the same string is kept
// for the substring check. c may be nil (portfolio review).
func Render(in Inputs, c *Candidate) string {
	var sb strings.Builder
	b, p := in.Book, in.Policy
	sb.WriteString("## PORTFOLIO\n")

	w := b.SleeveWeights()
	fmt.Fprintf(&sb, "Sleeves vs IPS bands (%% of equity): equities %.1f%% (band %.0f-%.0f%%, target %.0f%%) | bonds %.1f%% (band %.0f-%.0f%%, target %.0f%%) | cash %.1f%% (band %.0f-%.0f%%, target %.0f%%)\n",
		w[portfolio.SleeveEquities], p.EquityMin, p.EquityMax, p.TargetEquity,
		w[portfolio.SleeveBonds], p.BondMin, p.BondMax, p.TargetBond,
		w[portfolio.SleeveCash], p.CashMin, p.CashMax, p.TargetCash)
	if b.Equity > 0 {
		fmt.Fprintf(&sb, "Cash: %.1f%% of equity is actual cash (T-bill ETFs count toward the cash sleeve)\n", b.Cash/b.Equity*100)
	}

	// Top 5 positions.
	hs := append([]portfolio.Holding(nil), b.Holdings...)
	sort.Slice(hs, func(i, j int) bool { return hs[i].Value > hs[j].Value })
	if len(hs) == 0 {
		sb.WriteString("Top positions: none\n")
	} else {
		var parts []string
		for i, h := range hs {
			if i >= 5 {
				break
			}
			parts = append(parts, fmt.Sprintf("%s %.1f%%", h.Symbol, h.Value/b.Equity*100))
		}
		fmt.Fprintf(&sb, "Top positions: %s\n", strings.Join(parts, " | "))
	}

	// Sector weights (equity sleeve).
	sectors := b.SectorValues()
	if len(sectors) > 0 {
		keys := make([]string, 0, len(sectors))
		for k := range sectors {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if sectors[keys[i]] != sectors[keys[j]] {
				return sectors[keys[i]] > sectors[keys[j]]
			}
			return keys[i] < keys[j]
		})
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %.1f%%", k, sectors[k]/b.Equity*100))
		}
		fmt.Fprintf(&sb, "Sector weights (limit %.0f%%, Unknown is one bucket): %s\n", p.SectorMaxPct, strings.Join(parts, " | "))
	}
	if years, val := b.BondDuration(); val > 0 {
		fmt.Fprintf(&sb, "Bond duration: %.1f years weighted (cap %.0f)\n", years, p.MaxDuration)
	}

	if in.HaveRisk {
		fmt.Fprintf(&sb, "Portfolio volatility (60d, annualized): %.1f%% | beta to SPY: %.2f\n", in.Vol, in.Beta)
	} else {
		sb.WriteString("Portfolio volatility / beta: unavailable\n")
	}
	if in.HaveDD {
		fmt.Fprintf(&sb, "Drawdown from peak: %.1f%%\n", in.DrawdownPct)
	}
	fmt.Fprintf(&sb, "Since tracking start: bot %s | SPY %s | 60/40 SPY/AGG %s\n", pctStr(in.BotReturn), pctStr(in.SPYReturn), pctStr(in.Bench6040))

	sb.WriteString(in.Regime.Text())

	if c != nil {
		fmt.Fprintf(&sb, "\n## CANDIDATE FIT: %s\n", c.Symbol)
		if c.Corr == nil {
			sb.WriteString("Average 60d correlation to current holdings: unavailable\n")
		} else {
			fmt.Fprintf(&sb, "Average 60d correlation to current holdings: %.2f over %d holdings (highest: %s %.2f)\n", c.Corr.Avg, c.Corr.N, c.Corr.MaxSym, c.Corr.Max)
		}
		if b.Equity > 0 {
			add := b.Equity * c.AddPct / 100
			sleeve := portfolio.Sleeve(c.Symbol)
			after := map[string]float64{}
			for k, v := range b.SleeveValues() {
				after[k] = v
			}
			if sleeve != portfolio.SleeveCash {
				after[sleeve] += add
				after[portfolio.SleeveCash] -= add
			}
			fmt.Fprintf(&sb, "If bought at %.0f%% of equity: %s sleeve %.1f%% -> %.1f%%, cash sleeve %.1f%% -> %.1f%%",
				c.AddPct, sleeve, w[sleeve], after[sleeve]/b.Equity*100, w[portfolio.SleeveCash], after[portfolio.SleeveCash]/b.Equity*100)
			if sleeve == portfolio.SleeveEquities {
				key := portfolio.SectorKey(c.Symbol, c.Sector)
				fmt.Fprintf(&sb, ", %s sector %.1f%% -> %.1f%%", key, sectors[key]/b.Equity*100, (sectors[key]+add)/b.Equity*100)
			}
			sb.WriteString("\n")
			if c.Bind != "" {
				fmt.Fprintf(&sb, "IPS room for this symbol: $%.0f (binding: %s)\n", c.Room, c.Bind)
			}
		}
	}
	return sb.String()
}
