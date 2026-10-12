package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
)

// validSymbol guards URL path/query construction against odd characters
// coming from news feeds or LLM output.
var validSymbol = regexp.MustCompile(`^[A-Z]{1,5}(\.[A-Z])?$`)

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

// bracketTimeInForce is GTC so the stop-loss and take-profit legs stay live
// overnight; with "day" they would expire at the close and leave a swing
// position unprotected.
const bracketTimeInForce = "gtc"

// PlaceBracketOrder submits a market entry order with attached take-profit
// and stop-loss legs (order_class: bracket).
func (c *AlpacaClient) PlaceBracketOrder(symbol string, qty int, side Side, stopLoss, takeProfit float64) error {
	body := bracketOrderRequest{
		Symbol:      symbol,
		Qty:         qty,
		Side:        side,
		Type:        "market",
		TimeInForce: bracketTimeInForce,
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

// GetAsset returns Alpaca's /v2/assets/{symbol} payload (class, exchange,
// status, tradable). A 404 is reported as ErrNotFound.
func (c *AlpacaClient) GetAsset(symbol string) ([]byte, error) {
	if !validSymbol.MatchString(symbol) {
		return nil, fmt.Errorf("invalid symbol %q", symbol)
	}
	data, status, err := c.doRequest("GET", "/v2/assets/"+symbol, nil)
	if err != nil {
		if status == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

// GetPortfolioHistory returns the daily equity curve for period (e.g. "30D").
func (c *AlpacaClient) GetPortfolioHistory(period string) ([]byte, error) {
	data, _, err := c.doRequest("GET", "/v2/account/portfolio/history?timeframe=1D&period="+period, nil)
	return data, err
}

// GetClosedOrders returns up to 500 closed orders submitted after the given
// RFC3339 timestamp, oldest first.
func (c *AlpacaClient) GetClosedOrders(after string) ([]byte, error) {
	q := url.Values{"status": {"closed"}, "direction": {"asc"}, "limit": {"500"}, "after": {after}}
	data, _, err := c.doRequest("GET", "/v2/orders?"+q.Encode(), nil)
	return data, err
}

// GetCalendar returns the trading-calendar entry for a single date
// (YYYY-MM-DD); the array is empty on weekends and holidays.
func (c *AlpacaClient) GetCalendar(date string) ([]byte, error) {
	data, _, err := c.doRequest("GET", "/v2/calendar?start="+date+"&end="+date, nil)
	return data, err
}

// CancelOpenOrders cancels every open order for symbol (including the legs of
// a bracket). It must run before closing a position, because shares reserved
// by open stop/limit legs cannot be sold. Returns how many orders were
// cancelled.
func (c *AlpacaClient) CancelOpenOrders(symbol string) (int, error) {
	if !validSymbol.MatchString(symbol) {
		return 0, fmt.Errorf("invalid symbol %q", symbol)
	}
	data, _, err := c.doRequest("GET", "/v2/orders?status=open&limit=100&symbols="+symbol, nil)
	if err != nil {
		return 0, err
	}
	var orders []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &orders); err != nil {
		return 0, err
	}
	cancelled := 0
	var firstErr error
	for _, o := range orders {
		if _, status, err := c.doRequest("DELETE", "/v2/orders/"+o.ID, nil); err != nil {
			// 422 means the order already filled/cancelled (e.g. a sibling leg).
			if status != http.StatusUnprocessableEntity && firstErr == nil {
				firstErr = err
			}
			continue
		}
		cancelled++
	}
	return cancelled, firstErr
}
