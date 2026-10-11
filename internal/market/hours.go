package market

import (
	"encoding/json"
	"fmt"

	"github.com/francocaccia/quantengine/internal/broker"
)

type clockResponse struct {
	IsOpen bool `json:"is_open"`
}

// IsMarketOpen queries Alpaca's /v2/clock endpoint, which accounts for
// exchange holidays and half days in addition to regular trading hours.
func IsMarketOpen(b *broker.AlpacaClient) (bool, error) {
	data, err := b.GetClock()
	if err != nil {
		return false, fmt.Errorf("fetching market clock: %w", err)
	}
	var clock clockResponse
	if err := json.Unmarshal(data, &clock); err != nil {
		return false, fmt.Errorf("parsing market clock: %w", err)
	}
	return clock.IsOpen, nil
}
