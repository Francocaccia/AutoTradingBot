package indicators

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

type Result struct {
	Symbol string  `json:"symbol"`
	Price  float64 `json:"price"`
	RSI    float64 `json:"rsi"`
	Volume int64   `json:"volume"`
	Beta   float64 `json:"beta"`
	PE     float64 `json:"pe_ratio"`

	ATR       float64 `json:"atr"`        // ATR(14), in dollars
	AvgVolume float64 `json:"avg_volume"` // mean daily volume over ~60 completed sessions
	Sector    string  `json:"sector"`     // empty when unavailable (e.g. ETFs)
	Name      string  `json:"name"`
	QuoteType string  `json:"quote_type"` // EQUITY, ETF, ...

	Ichimoku   IchimokuResult   `json:"ichimoku"`
	MACD       MACDResult       `json:"macd"`
	Stochastic StochasticResult `json:"stochastic"`
	Keltner    KeltnerResult    `json:"keltner"`
}

type IchimokuResult struct {
	Tenkan float64 `json:"tenkan"`
	Kijun  float64 `json:"kijun"`
	SpanA  float64 `json:"span_a"`
	SpanB  float64 `json:"span_b"`
	Chikou float64 `json:"chikou"`
}

type MACDResult struct {
	MACD      float64 `json:"macd"`
	Signal    float64 `json:"signal"`
	Histogram float64 `json:"histogram"`
}

type StochasticResult struct {
	K float64 `json:"k"`
	D float64 `json:"d"`
}

type KeltnerResult struct {
	Upper  float64 `json:"upper"`
	Middle float64 `json:"middle"`
	Lower  float64 `json:"lower"`
}

const calcTimeout = 60 * time.Second

func Calculate(symbol string) (*Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), calcTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "python/indicators/calculate.py", symbol)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("python script failed for %s: %w", symbol, err)
	}
	var result Result
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse indicators for %s: %w", symbol, err)
	}
	return &result, nil
}
