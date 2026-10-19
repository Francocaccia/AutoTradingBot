// Package marketdata wraps the Alpaca market-data endpoints the bot needs
// beyond news: the stock screener and daily bars.
package marketdata

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultBaseURL = "https://data.alpaca.markets"

type Client struct {
	apiKey    string
	apiSecret string
	baseURL   string
	http      *http.Client
}

func NewClient(apiKey, apiSecret string) *Client {
	return &Client{apiKey: apiKey, apiSecret: apiSecret, baseURL: defaultBaseURL, http: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) get(path string, out any) error {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.apiSecret)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("alpaca data %d: %s", resp.StatusCode, string(body))
	}
	return json.Unmarshal(body, out)
}

// MostActives returns the top symbols by volume.
func (c *Client) MostActives(top int) ([]string, error) {
	var r struct {
		MostActives []struct {
			Symbol string `json:"symbol"`
		} `json:"most_actives"`
	}
	if err := c.get(fmt.Sprintf("/v1beta1/screener/stocks/most-actives?by=volume&top=%d", top), &r); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r.MostActives))
	for _, m := range r.MostActives {
		out = append(out, m.Symbol)
	}
	return out, nil
}

// Movers returns the top gainers and top losers (in that order, each list
// ranked best-first).
func (c *Client) Movers(top int) (gainers, losers []string, err error) {
	var r struct {
		Gainers []struct {
			Symbol string `json:"symbol"`
		} `json:"gainers"`
		Losers []struct {
			Symbol string `json:"symbol"`
		} `json:"losers"`
	}
	if err := c.get(fmt.Sprintf("/v1beta1/screener/stocks/movers?top=%d", top), &r); err != nil {
		return nil, nil, err
	}
	for _, g := range r.Gainers {
		gainers = append(gainers, g.Symbol)
	}
	for _, l := range r.Losers {
		losers = append(losers, l.Symbol)
	}
	return gainers, losers, nil
}

// DailyBar is one daily bar: Date is the session (YYYY-MM-DD), Close its close.
type DailyBar struct {
	Date  string
	Close float64
}

// DailyBars returns split/dividend-adjusted daily bars (IEX feed) for symbol
// from start (YYYY-MM-DD) onward, oldest first.
func (c *Client) DailyBars(symbol, start string) ([]DailyBar, error) {
	var r struct {
		Bars []struct {
			C float64 `json:"c"`
			T string  `json:"t"`
		} `json:"bars"`
	}
	path := "/v2/stocks/" + symbol + "/bars?timeframe=1Day&adjustment=all&feed=iex&limit=1000&start=" + start
	if err := c.get(path, &r); err != nil {
		return nil, err
	}
	out := make([]DailyBar, 0, len(r.Bars))
	for _, b := range r.Bars {
		if b.C > 0 && len(b.T) >= 10 {
			out = append(out, DailyBar{Date: b.T[:10], Close: b.C})
		}
	}
	return out, nil
}

// SetBaseURL points the client at another host (tests).
func (c *Client) SetBaseURL(u string) { c.baseURL = u }
