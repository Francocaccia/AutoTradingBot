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
