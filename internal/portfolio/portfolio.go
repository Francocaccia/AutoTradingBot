package portfolio

import (
	"encoding/json"
	"errors"

	"github.com/francocaccia/quantengine/internal/broker"
)

type Position struct {
	Symbol    string  `json:"symbol"`
	Qty       int     `json:"qty,string"`
	AvgPrice  float64 `json:"avg_entry_price,string"`
	MarketVal float64 `json:"market_value,string"`
	PLPercent float64 `json:"unrealized_plpc,string"`
}

type Portfolio struct {
	broker *broker.AlpacaClient
}

func NewPortfolio(b *broker.AlpacaClient) *Portfolio {
	return &Portfolio{broker: b}
}

// GetPosition returns the open position for symbol, or (nil, nil) if there
// is no position. Any error other than "not found" is returned to the
// caller so it can skip the symbol instead of assuming it is flat.
func (p *Portfolio) GetPosition(symbol string) (*Position, error) {
	data, err := p.broker.GetPosition(symbol)
	if err != nil {
		if errors.Is(err, broker.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var pos Position
	if err := json.Unmarshal(data, &pos); err != nil {
		return nil, err
	}
	return &pos, nil
}

func (p *Portfolio) GetAllPositions() ([]Position, error) {
	data, err := p.broker.GetPositions()
	if err != nil {
		return nil, err
	}
	var positions []Position
	if err := json.Unmarshal(data, &positions); err != nil {
		return nil, err
	}
	return positions, nil
}

// Account is the subset of /v2/account the bot needs. Cash (not
// buying_power) bounds sizing so a margin account's 2-4x buying power is
// never used.
type Account struct {
	Equity      float64 `json:"equity,string"`
	LastEquity  float64 `json:"last_equity,string"`
	Cash        float64 `json:"cash,string"`
	BuyingPower float64 `json:"buying_power,string"`
}

// Spendable is the amount that can be committed to new longs without margin.
func (a Account) Spendable() float64 {
	v := a.BuyingPower
	if a.Cash < v {
		v = a.Cash
	}
	if v < 0 {
		return 0
	}
	return v
}

func (p *Portfolio) GetAccount() (*Account, error) {
	data, err := p.broker.GetAccount()
	if err != nil {
		return nil, err
	}
	var acct Account
	if err := json.Unmarshal(data, &acct); err != nil {
		return nil, err
	}
	return &acct, nil
}
