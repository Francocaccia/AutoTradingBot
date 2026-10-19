package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The daily and weekly trade caps are checked in the scheduler from a snapshot
// taken at the start of a cycle, and then kept up to date in memory. That is not
// enough on its own: two instances alive at once (Render keeps the old one up
// while a deploy starts) each have their own snapshot, and an order that is
// accepted but not yet filled is in nobody's. So the caps are checked again here,
// at the one place every order passes, from Alpaca's own list of orders, right
// before a BUY is sent.

// ErrCapReached means sending the order would exceed MAX_TRADES_PER_DAY or
// MAX_TRADES_PER_WEEK, counted from Alpaca. Nothing was sent.
var ErrCapReached = errors.New("trade cap reached")

// weekWindow is the number of trading days (today included) in the weekly cap.
const weekWindow = 5

// SetTradeCaps turns on the cap check for BUY orders. perDay and perWeek count
// the bot's own orders (client_order_id with ClientOrderPrefix: entries, exits and
// rebalancing trades, live or filled, never bracket legs); a value <= 0 means no
// limit. Orders dated before countFrom (YYYY-MM-DD, ET) are ignored, as with
// GUARDS_START_DATE. Sells are never blocked here: an exit must always be able to go.
func (c *AlpacaClient) SetTradeCaps(perDay, perWeek int, countFrom string) {
	c.capDay, c.capWeek, c.capFrom = perDay, perWeek, countFrom
}

// capOrder is the part of a listed order the cap count reads.
type capOrder struct {
	ClientOrderID string `json:"client_order_id"`
	Status        string `json:"status"`
	FilledQty     string `json:"filled_qty"`
	SubmittedAt   string `json:"submitted_at"`
	FilledAt      string `json:"filled_at"`
}

// countOrders counts the bot's orders that are live or filled: on `today`, and
// from `weekStart` through today. Orders before countFrom are left out.
func countOrders(orders []capOrder, today, weekStart, countFrom string) (day, week int) {
	for _, o := range orders {
		if !strings.HasPrefix(o.ClientOrderID, ClientOrderPrefix) {
			continue
		}
		if (orderInfo{Status: o.Status, FilledQty: o.FilledQty}).dead() {
			continue
		}
		stamp := o.FilledAt
		if stamp == "" {
			stamp = o.SubmittedAt
		}
		t, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		d := t.In(etLoc).Format("2006-01-02")
		if d < countFrom || d < weekStart || d > today {
			continue
		}
		week++
		if d == today {
			day++
		}
	}
	return day, week
}

// weekStartDate is the first of the last weekWindow trading days ending at
// today, from Alpaca's calendar (weekdays if that fails).
func (c *AlpacaClient) weekStartDate(now time.Time) string {
	now = now.In(etLoc)
	today := now.Format("2006-01-02")
	var days []string
	if data, err := c.GetCalendarRange(now.AddDate(0, 0, -14).Format("2006-01-02"), today); err == nil {
		var cal []struct {
			Date string `json:"date"`
		}
		if json.Unmarshal(data, &cal) == nil {
			for _, d := range cal {
				if d.Date <= today {
					days = append(days, d.Date)
				}
			}
		}
	}
	if len(days) == 0 {
		for d := now.AddDate(0, 0, -14); !d.After(now); d = d.AddDate(0, 0, 1) {
			if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
				days = append(days, d.Format("2006-01-02"))
			}
		}
	}
	sort.Strings(days)
	if len(days) > weekWindow {
		days = days[len(days)-weekWindow:]
	}
	if len(days) == 0 {
		return today
	}
	return days[0]
}

// checkTradeCaps refuses a BUY that would take the bot's order count over a cap.
// Not being able to count is an error too: the order is not sent blind.
func (c *AlpacaClient) checkTradeCaps() error {
	if c.capDay <= 0 && c.capWeek <= 0 {
		return nil
	}
	now := c.now().In(etLoc)
	today := now.Format("2006-01-02")
	weekStart := c.weekStartDate(now)
	first, _ := time.ParseInLocation("2006-01-02", weekStart, etLoc)
	q := url.Values{"status": {"all"}, "direction": {"asc"}, "limit": {"500"}, "after": {first.UTC().Format(time.RFC3339)}}
	data, _, err := c.doRequest("GET", "/v2/orders?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("counting today's orders for the trade caps: %w", err)
	}
	var orders []capOrder
	if err := json.Unmarshal(data, &orders); err != nil {
		return fmt.Errorf("counting today's orders for the trade caps: %w", err)
	}
	day, week := countOrders(orders, today, weekStart, c.capFrom)
	switch {
	case c.capDay > 0 && day+1 > c.capDay:
		return fmt.Errorf("%w: %d of %d orders already today (counted from Alpaca, open orders included)", ErrCapReached, day, c.capDay)
	case c.capWeek > 0 && week+1 > c.capWeek:
		return fmt.Errorf("%w: %d of %d orders already in the last %d trading days (counted from Alpaca, open orders included)", ErrCapReached, week, c.capWeek, weekWindow)
	}
	return nil
}

// prepareOrder is the one step before every order is sent: the duplicate check
// and the id, then, for a BUY, the cap check.
func (c *AlpacaClient) prepareOrder(symbol string, side Side, kind string) (string, error) {
	id, err := c.nextOrderID(symbol, side, kind)
	if err != nil {
		return "", err
	}
	if side == Buy {
		if err := c.checkTradeCaps(); err != nil {
			return "", err
		}
	}
	return id, nil
}
