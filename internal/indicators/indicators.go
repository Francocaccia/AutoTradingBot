package indicators

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

type Result struct {
	Symbol string  `json:"symbol"`
	Price  float64 `json:"price"`
	RSI    float64 `json:"rsi"`
	Volume int64   `json:"volume"`
	Beta   float64 `json:"beta"`
	PE     float64 `json:"pe_ratio"`

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

func Calculate(symbol string) (*Result, error) {
	cmd := exec.Command("python3", "python/indicators/calculate.py", symbol)
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
