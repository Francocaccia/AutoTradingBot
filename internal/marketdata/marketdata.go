// Package marketdata wraps the Alpaca market-data endpoints the bot needs
// beyond news: the stock screener and daily-bar snapshots.
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

// DailyCloses returns the latest daily bar's close and date (YYYY-MM-DD, the
// session it belongs to) and the previous session's close for symbol. Callers
// must check barDate: before the day's bar exists, the "latest" bar is the
// previous session's.
func (c *Client) DailyCloses(symbol string) (last, prev float64, barDate string, err error) {
	var r struct {
		DailyBar struct {
			C float64 `json:"c"`
			T string  `json:"t"`
		} `json:"dailyBar"`
		PrevDailyBar struct {
			C float64 `json:"c"`
		} `json:"prevDailyBar"`
	}
	if err := c.get("/v2/stocks/"+symbol+"/snapshot?feed=iex", &r); err != nil {
		return 0, 0, "", err
	}
	if r.DailyBar.C <= 0 || r.PrevDailyBar.C <= 0 || len(r.DailyBar.T) < 10 {
		return 0, 0, "", fmt.Errorf("snapshot for %s missing daily bars", symbol)
	}
	return r.DailyBar.C, r.PrevDailyBar.C, r.DailyBar.T[:10], nil
}
