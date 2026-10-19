// Package risk holds the pure, side-effect-free risk rules: position sizing,
// bracket levels, circuit breakers and sector limits.
package risk

import (
	"fmt"
	"math"
	"time"
)

const (
	// DefaultMinConviction is the lowest conviction that may open or add to a
	// position unless MIN_BUY_CONVICTION says otherwise.
	DefaultMinConviction = 8
	// DefaultMaxRiskPct is the default cap on loss-at-stop per trade, % of equity.
	DefaultMaxRiskPct = 1.0
	// MaxSectorPct is the most equity allowed in a single sector.
	MaxSectorPct = 30.0
	// StopATRMultiple places the stop-loss this many ATRs below entry.
	StopATRMultiple = 2.0
	// MinRewardRisk is the minimum take-profit distance / stop distance.
	MinRewardRisk = 2.0
	// DailyLossLimitPct stops new entries when equity is this far below the day's start.
	DailyLossLimitPct = 3.0
	// MaxDrawdownPct halts BUYs when equity is this far below its peak.
	MaxDrawdownPct = 10.0
	// EntryBuffer is the no-new-BUY window after the open and before the close.
	EntryBuffer = 10 * time.Minute
)

// TargetPct maps conviction to a base position size as % of equity:
// 8 -> 5%, 9 -> 7%, 10 -> 10%. Convictions below minConviction (or below 1)
// return 0; with a minConviction under 8 the lower ones also get 5%, so the
// smallest allowed buy is always 5%.
func TargetPct(conviction, minConviction int) float64 {
	if conviction < 1 || conviction < minConviction {
		return 0
	}
	switch {
	case conviction >= 10:
		return 10
	case conviction == 9:
		return 7
	default:
		return 5
	}
}

type SizeInput struct {
	Equity               float64
	Spendable            float64 // cash available without margin
	Price                float64
	CurrentPositionValue float64 // existing market value in this symbol
	Conviction           int
	MinConviction        int     // <= 0 means DefaultMinConviction
	MaxPositionPct       float64 // hard cap, % of equity
	MaxPositionUSD       float64 // optional absolute cap; <= 0 disables
}

// Size returns the number of shares to buy (0 with a reason when none).
// The target position value is equity * conviction pct, capped by
// MaxPositionPct and MaxPositionUSD; what is already held counts toward it,
// and the order is limited by spendable cash.
func Size(in SizeInput) (qty int, reason string) {
	minConv := in.MinConviction
	if minConv <= 0 {
		minConv = DefaultMinConviction
	}
	pct := TargetPct(in.Conviction, minConv)
	if pct == 0 {
		return 0, fmt.Sprintf("conviction %d below minimum %d", in.Conviction, minConv)
	}
	if in.Equity <= 0 || in.Price <= 0 {
		return 0, fmt.Sprintf("invalid equity (%.2f) or price (%.2f)", in.Equity, in.Price)
	}
	if in.MaxPositionPct > 0 && pct > in.MaxPositionPct {
		pct = in.MaxPositionPct
	}
	target := in.Equity * pct / 100
	if in.MaxPositionUSD > 0 && target > in.MaxPositionUSD {
		target = in.MaxPositionUSD
	}
	add := target - in.CurrentPositionValue
	if add <= 0 {
		return 0, fmt.Sprintf("position $%.2f already at target $%.2f", in.CurrentPositionValue, target)
	}
	if add > in.Spendable {
		add = in.Spendable
	}
	qty = int(math.Floor(add / in.Price))
	if qty < 1 {
		return 0, fmt.Sprintf("insufficient buying power ($%.2f) or size for one share at $%.2f", in.Spendable, in.Price)
	}
	return qty, ""
}

// CapQtyByRisk limits qty so that qty*(price-stop) does not exceed maxRiskPct %
// of equity (the loss if the stop is hit). It returns the possibly reduced
// quantity (0 when not even one share fits) and how far the stop is below
// price, in percent. A non-positive maxRiskPct, equity, or a stop at or above
// price leaves qty unchanged.
func CapQtyByRisk(qty int, price, stop, equity, maxRiskPct float64) (capped int, stopPct float64) {
	if price <= 0 || stop >= price {
		return qty, 0
	}
	perShare := price - stop
	stopPct = perShare / price * 100
	if maxRiskPct <= 0 || equity <= 0 {
		return qty, stopPct
	}
	maxQty := int(math.Floor(equity*maxRiskPct/100/perShare + 1e-9))
	if maxQty < qty {
		return maxQty, stopPct
	}
	return qty, stopPct
}

// SectorRoom returns the dollars that may still be added to sector without
// exceeding MaxSectorPct of equity. evaluated is false when the sector is
// unknown, in which case the rule does not apply.
func SectorRoom(sector string, valueBySector map[string]float64, equity float64) (room float64, evaluated bool) {
	if sector == "" || equity <= 0 {
		return 0, false
	}
	room = equity*MaxSectorPct/100 - valueBySector[sector]
	if room < 0 {
		room = 0
	}
	return room, true
}

// Levels are the bracket prices for a long entry.
type Levels struct {
	Stop, Take, RewardRisk float64
}

// Bracket computes the stop-loss at StopATRMultiple x ATR below price and a
// take-profit. proposedTP <= 0 means "choose for me" (exactly 2R); otherwise
// the proposal is used and rejected if its reward:risk is below MinRewardRisk.
func Bracket(price, atr, proposedTP float64) (Levels, error) {
	return BracketMult(price, atr, proposedTP, StopATRMultiple)
}

// EffectiveStopMultiple is the ATR multiple of a stock stop: at least
// StopATRMultiple, and at least min (MIN_STOP_ATR_MULT) when that is larger.
// The ATR is the average true range, which is never below the average daily
// range, so a stop of this many ATRs also clears a normal day's range.
func EffectiveStopMultiple(min float64) float64 {
	return math.Max(StopATRMultiple, min)
}

// BracketMult is Bracket with the stop mult x ATR below price.
func BracketMult(price, atr, proposedTP, mult float64) (Levels, error) {
	if price <= 0 || atr <= 0 {
		return Levels{}, fmt.Errorf("need positive price (%.2f) and ATR (%.4f)", price, atr)
	}
	stop := math.Floor((price-mult*atr)*100) / 100
	if stop <= 0 {
		return Levels{}, fmt.Errorf("ATR stop would be non-positive (price %.2f, ATR %.2f)", price, atr)
	}
	riskPerShare := price - stop
	if riskPerShare < 0.01 {
		return Levels{}, fmt.Errorf("stop distance %.4f too small", riskPerShare)
	}

	take := math.Ceil((price+MinRewardRisk*riskPerShare)*100) / 100
	if proposedTP > 0 {
		take = math.Round(proposedTP*100) / 100
	}
	rr := (take - price) / riskPerShare
	if rr < MinRewardRisk-1e-9 {
		return Levels{}, fmt.Errorf("reward:risk %.2f below %.1f (stop %.2f, take %.2f, price %.2f)", rr, MinRewardRisk, stop, take, price)
	}
	return Levels{Stop: stop, Take: take, RewardRisk: rr}, nil
}

// StopBelow returns the stop-loss price atrMultiple x ATR below price, rounded
// down to the cent, for positions that carry a stop but no take-profit (bond
// ETFs). It fails when the stop would be non-positive or too tight.
func StopBelow(price, atr, atrMultiple float64) (float64, error) {
	if price <= 0 || atr <= 0 || atrMultiple <= 0 {
		return 0, fmt.Errorf("need positive price (%.2f), ATR (%.4f) and multiple (%.1f)", price, atr, atrMultiple)
	}
	stop := math.Floor((price-atrMultiple*atr)*100) / 100
	if stop <= 0 {
		return 0, fmt.Errorf("ATR stop would be non-positive (price %.2f, ATR %.2f)", price, atr)
	}
	if price-stop < 0.01 {
		return 0, fmt.Errorf("stop distance %.4f too small", price-stop)
	}
	return stop, nil
}

// BondStopATRMultiple is the wider stop used for bond ETFs.
const BondStopATRMultiple = 3.0

// Breakers evaluates the equity circuit breakers.
//   - dayStop: equity is >= DailyLossLimitPct below the day's starting equity;
//     no new positions until the next session.
//   - drawdownHalt: equity is >= MaxDrawdownPct below the peak; BUYs halt.
func Breakers(equity, dayStartEquity, peakEquity float64) (dayStop, drawdownHalt bool, dayPct, drawdownPct float64) {
	if dayStartEquity > 0 {
		dayPct = (equity - dayStartEquity) / dayStartEquity * 100
		dayStop = dayPct <= -DailyLossLimitPct
	}
	if peakEquity > 0 {
		drawdownPct = (peakEquity - equity) / peakEquity * 100
		drawdownHalt = drawdownPct >= MaxDrawdownPct
	}
	return
}
