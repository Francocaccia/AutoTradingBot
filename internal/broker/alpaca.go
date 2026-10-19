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
	"strconv"
	"strings"
	"time"
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
	now       func() time.Time // overridden in tests

	capDay, capWeek int    // trade caps checked before every BUY (SetTradeCaps); <= 0 means none
	capFrom         string // orders before this ET date are not counted
}

func NewAlpacaClient(apiKey, apiSecret, baseURL string) *AlpacaClient {
	if baseURL == "" {
		baseURL = "https://paper-api.alpaca.markets"
	}
	return &AlpacaClient{apiKey: apiKey, apiSecret: apiSecret, baseURL: baseURL, http: &http.Client{Timeout: 30 * time.Second}, now: time.Now}
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

// ClientOrderPrefix tags every order the bot submits on its own decision. The
// trade counters count only filled orders carrying it, so bracket legs (whose
// client_order_id Alpaca generates) and manual orders never count.
const ClientOrderPrefix = "atb-"

// Order kinds, the last part of a client_order_id.
const (
	KindEntry     = "entry"     // a decision to open or add to a position
	KindExit      = "exit"      // a decision to close a position
	KindRebalance = "rebalance" // an investment-policy tranche toward a sleeve's minimum band
)

// ErrDuplicateOrder means the bot already submitted an order for the same
// symbol and side on this ET date (or has failed to place one too many times),
// so nothing was sent. It makes a restart or a repeated cycle harmless.
var ErrDuplicateOrder = errors.New("order already submitted today")

// maxOrderAttempts is how many orders per symbol, side and day may be tried
// when earlier ones were canceled, expired or rejected.
const maxOrderAttempts = 3

var etLoc = func() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.FixedZone("ET", -5*3600)
}()

// orderDate is the ET calendar date of t as YYYYMMDD.
func orderDate(t time.Time) string { return t.In(etLoc).Format("20060102") }

// ClientOrderID derives the id of an order from what it is: the ET date, the
// symbol, the side and the kind, e.g. "atb-20261016-NVDA-buy-entry". The same
// decision therefore always has the same id, which the broker rejects as a
// duplicate. attempt > 1 appends "-rN" for a retry after a failed order.
func ClientOrderID(date, symbol string, side Side, kind string, attempt int) string {
	id := fmt.Sprintf("%s%s-%s-%s-%s", ClientOrderPrefix, date, symbol, side, kind)
	if attempt > 1 {
		id += fmt.Sprintf("-r%d", attempt)
	}
	return id
}

// ParseClientOrderID returns the kind encoded in a client_order_id built by
// ClientOrderID. ok is false for legacy random ids and for orders that are not
// the bot's.
func ParseClientOrderID(id string) (kind string, ok bool) {
	parts := strings.Split(id, "-")
	if len(parts) < 5 || parts[0]+"-" != ClientOrderPrefix || len(parts[1]) != 8 {
		return "", false
	}
	switch parts[4] {
	case KindEntry, KindExit, KindRebalance:
		return parts[4], true
	}
	return "", false
}

// orderInfo is the part of a listed order the duplicate check reads.
type orderInfo struct {
	ClientOrderID string `json:"client_order_id"`
	Side          string `json:"side"`
	Status        string `json:"status"`
	FilledQty     string `json:"filled_qty"`
}

// dead reports whether the order can no longer fill: it was canceled, expired
// or rejected without filling anything.
func (o orderInfo) dead() bool {
	if q, _ := strconv.ParseFloat(o.FilledQty, 64); q > 0 {
		return false
	}
	switch o.Status {
	case "canceled", "expired", "rejected", "replaced", "done_for_day", "suspended":
		return true
	}
	return false
}

// nextOrderID returns the client_order_id for a new order, after looking at
// today's orders for the symbol: if one for the same side is live or filled it
// returns ErrDuplicateOrder. The lookup failing is an error too: not knowing
// whether an order exists is a reason not to send another.
func (c *AlpacaClient) nextOrderID(symbol string, side Side, kind string) (string, error) {
	if !validSymbol.MatchString(symbol) {
		return "", fmt.Errorf("invalid symbol %q", symbol)
	}
	now := c.now().In(etLoc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, etLoc)
	q := url.Values{"status": {"all"}, "symbols": {symbol}, "direction": {"asc"}, "limit": {"100"},
		"after": {midnight.UTC().Format(time.RFC3339)}}
	data, _, err := c.doRequest("GET", "/v2/orders?"+q.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("checking today's orders for %s: %w", symbol, err)
	}
	var orders []orderInfo
	if err := json.Unmarshal(data, &orders); err != nil {
		return "", fmt.Errorf("checking today's orders for %s: %w", symbol, err)
	}
	prefix := fmt.Sprintf("%s%s-%s-%s-", ClientOrderPrefix, orderDate(now), symbol, side)
	failed := 0
	for _, o := range orders {
		if !strings.HasPrefix(o.ClientOrderID, prefix) || o.Side != string(side) {
			continue
		}
		if !o.dead() {
			return "", fmt.Errorf("%w: %s %s (%s, %s)", ErrDuplicateOrder, side, symbol, o.ClientOrderID, o.Status)
		}
		failed++
	}
	if failed >= maxOrderAttempts {
		return "", fmt.Errorf("%w: %d earlier %s %s orders today failed", ErrDuplicateOrder, failed, side, symbol)
	}
	return ClientOrderID(orderDate(now), symbol, side, kind, failed+1), nil
}

// CheckOrder reports whether an order of this symbol and side could be placed
// now without duplicating one from today (nil), or why not (ErrDuplicateOrder,
// or the lookup error). It sends nothing, so a caller can ask before it does
// anything irreversible such as cancelling a position's stop orders.
func (c *AlpacaClient) CheckOrder(symbol string, side Side, kind string) error {
	_, err := c.nextOrderID(symbol, side, kind)
	return err
}

type orderRequest struct {
	Symbol        string `json:"symbol"`
	Qty           int    `json:"qty"`
	Side          Side   `json:"side"`
	Type          string `json:"type"`
	TimeInForce   string `json:"time_in_force"`
	ClientOrderID string `json:"client_order_id"`
}

// PlaceMarketOrder submits a plain market order of the given kind (KindEntry,
// KindExit or KindRebalance). See nextOrderID for the duplicate protection.
func (c *AlpacaClient) PlaceMarketOrder(symbol string, qty int, side Side, kind string) error {
	id, err := c.prepareOrder(symbol, side, kind)
	if err != nil {
		return err
	}
	body := orderRequest{Symbol: symbol, Qty: qty, Side: side, Type: "market", TimeInForce: "day", ClientOrderID: id}
	_, _, err = c.doRequest("POST", "/v2/orders", body)
	return err
}

type bracketLeg struct {
	LimitPrice string `json:"limit_price,omitempty"`
	StopPrice  string `json:"stop_price,omitempty"`
}

type bracketOrderRequest struct {
	Symbol        string     `json:"symbol"`
	Qty           int        `json:"qty"`
	Side          Side       `json:"side"`
	Type          string     `json:"type"`
	TimeInForce   string     `json:"time_in_force"`
	OrderClass    string     `json:"order_class"`
	ClientOrderID string     `json:"client_order_id"`
	TakeProfit    bracketLeg `json:"take_profit"`
	StopLoss      bracketLeg `json:"stop_loss"`
}

// bracketTimeInForce is GTC so the stop-loss and take-profit legs stay live
// overnight; with "day" they would expire at the close and leave a swing
// position unprotected.
const bracketTimeInForce = "gtc"

// PlaceBracketOrder submits a market entry order with attached take-profit
// and stop-loss legs (order_class: bracket).
func (c *AlpacaClient) PlaceBracketOrder(symbol string, qty int, side Side, stopLoss, takeProfit float64, kind string) error {
	id, err := c.prepareOrder(symbol, side, kind)
	if err != nil {
		return err
	}
	body := bracketOrderRequest{
		Symbol:        symbol,
		Qty:           qty,
		Side:          side,
		Type:          "market",
		TimeInForce:   bracketTimeInForce,
		OrderClass:    "bracket",
		ClientOrderID: id,
		TakeProfit:    bracketLeg{LimitPrice: fmt.Sprintf("%.2f", takeProfit)},
		StopLoss:      bracketLeg{StopPrice: fmt.Sprintf("%.2f", stopLoss)},
	}
	_, _, err = c.doRequest("POST", "/v2/orders", body)
	return err
}

type otoOrderRequest struct {
	Symbol        string     `json:"symbol"`
	Qty           int        `json:"qty"`
	Side          Side       `json:"side"`
	Type          string     `json:"type"`
	TimeInForce   string     `json:"time_in_force"`
	OrderClass    string     `json:"order_class"`
	ClientOrderID string     `json:"client_order_id"`
	StopLoss      bracketLeg `json:"stop_loss"`
}

// PlaceOTOOrder submits a market entry with an attached stop-loss leg only
// (order_class: oto). Used for bond ETFs and policy rebalancing trades, which
// are reviewed on a schedule instead of carrying a take-profit.
func (c *AlpacaClient) PlaceOTOOrder(symbol string, qty int, side Side, stopLoss float64, kind string) error {
	id, err := c.prepareOrder(symbol, side, kind)
	if err != nil {
		return err
	}
	body := otoOrderRequest{
		Symbol: symbol, Qty: qty, Side: side, Type: "market", TimeInForce: bracketTimeInForce,
		OrderClass: "oto", ClientOrderID: id, StopLoss: bracketLeg{StopPrice: fmt.Sprintf("%.2f", stopLoss)},
	}
	_, _, err = c.doRequest("POST", "/v2/orders", body)
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

// GetPortfolioHistoryRange returns the equity curve between two dates
// (YYYY-MM-DD) at the given timeframe ("1Min" is accepted for ranges of at most
// seven days, "5Min" up to thirty, "1D" for anything longer).
func (c *AlpacaClient) GetPortfolioHistoryRange(timeframe, start, end string) ([]byte, error) {
	q := url.Values{"timeframe": {timeframe}, "date_start": {start}, "date_end": {end}}
	data, _, err := c.doRequest("GET", "/v2/account/portfolio/history?"+q.Encode(), nil)
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

// GetCalendarRange returns the trading-calendar entries between two dates
// (YYYY-MM-DD, inclusive); weekends and holidays are absent.
func (c *AlpacaClient) GetCalendarRange(start, end string) ([]byte, error) {
	q := url.Values{"start": {start}, "end": {end}}
	data, _, err := c.doRequest("GET", "/v2/calendar?"+q.Encode(), nil)
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
