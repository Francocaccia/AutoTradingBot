package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// ErrNotFound is returned by position lookups when Alpaca responds 404
// (i.e. there is no open position for the symbol).
var ErrNotFound = errors.New("alpaca: not found")

type AlpacaClient struct {
	apiKey    string
	apiSecret string
	baseURL   string
	http      *http.Client
}

func NewAlpacaClient(apiKey, apiSecret, baseURL string) *AlpacaClient {
	if baseURL == "" {
		baseURL = "https://paper-api.alpaca.markets"
	}
	return &AlpacaClient{apiKey: apiKey, apiSecret: apiSecret, baseURL: baseURL, http: &http.Client{}}
}

func (c *AlpacaClient) doRequest(method, path string, body any) ([]byte, int, error) {
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = &bytes.Buffer{}
	}
	req, err := http.NewRequest(method, c.baseURL+path, buf)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.apiSecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return data, resp.StatusCode, fmt.Errorf("alpaca error %d: %s", resp.StatusCode, string(data))
	}
	return data, resp.StatusCode, nil
}

type orderRequest struct {
	Symbol      string `json:"symbol"`
	Qty         int    `json:"qty"`
	Side        Side   `json:"side"`
	Type        string `json:"type"`
	TimeInForce string `json:"time_in_force"`
}

func (c *AlpacaClient) PlaceMarketOrder(symbol string, qty int, side Side) error {
	body := orderRequest{Symbol: symbol, Qty: qty, Side: side, Type: "market", TimeInForce: "day"}
	_, _, err := c.doRequest("POST", "/v2/orders", body)
	return err
}

type bracketLeg struct {
	LimitPrice string `json:"limit_price,omitempty"`
	StopPrice  string `json:"stop_price,omitempty"`
}

type bracketOrderRequest struct {
	Symbol      string     `json:"symbol"`
	Qty         int        `json:"qty"`
	Side        Side       `json:"side"`
	Type        string     `json:"type"`
	TimeInForce string     `json:"time_in_force"`
	OrderClass  string     `json:"order_class"`
	TakeProfit  bracketLeg `json:"take_profit"`
	StopLoss    bracketLeg `json:"stop_loss"`
}

// PlaceBracketOrder submits a market entry order with attached take-profit
// and stop-loss legs (order_class: bracket).
func (c *AlpacaClient) PlaceBracketOrder(symbol string, qty int, side Side, stopLoss, takeProfit float64) error {
	body := bracketOrderRequest{
		Symbol:      symbol,
		Qty:         qty,
		Side:        side,
		Type:        "market",
		TimeInForce: "day",
		OrderClass:  "bracket",
		TakeProfit:  bracketLeg{LimitPrice: fmt.Sprintf("%.2f", takeProfit)},
		StopLoss:    bracketLeg{StopPrice: fmt.Sprintf("%.2f", stopLoss)},
	}
	_, _, err := c.doRequest("POST", "/v2/orders", body)
	return err
}

func (c *AlpacaClient) GetPositions() ([]byte, error) {
	data, _, err := c.doRequest("GET", "/v2/positions", nil)
	return data, err
}

func (c *AlpacaClient) GetPosition(symbol string) ([]byte, error) {
	data, status, err := c.doRequest("GET", "/v2/positions/"+symbol, nil)
	if err != nil {
		if status == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

func (c *AlpacaClient) GetAccount() ([]byte, error) {
	data, _, err := c.doRequest("GET", "/v2/account", nil)
	return data, err
}

// GetClock returns Alpaca's /v2/clock payload, which reflects exchange
// holidays and half days in addition to regular trading hours.
func (c *AlpacaClient) GetClock() ([]byte, error) {
	data, _, err := c.doRequest("GET", "/v2/clock", nil)
	return data, err
}
