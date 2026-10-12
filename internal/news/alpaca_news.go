package news

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const alpacaNewsURL = "https://data.alpaca.markets/v1beta1/news"

type Article struct {
	Headline  string    `json:"headline"`
	Summary   string    `json:"summary"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	Symbols   []string  `json:"symbols"`
}

type AlpacaNewsClient struct {
	apiKey    string
	apiSecret string
	http      *http.Client
}

func NewAlpacaNewsClient(apiKey, apiSecret string) *AlpacaNewsClient {
	return &AlpacaNewsClient{apiKey: apiKey, apiSecret: apiSecret, http: &http.Client{}}
}

type alpacaNewsResponse struct {
	News []Article `json:"news"`
}

// FetchLatest returns the newest articles. With no symbols it returns general
// market news, which is where new universe candidates come from.
func (c *AlpacaNewsClient) FetchLatest(symbols []string, limit int) ([]Article, error) {
	url := fmt.Sprintf("%s?limit=%d&sort=desc", alpacaNewsURL, limit)
	if len(symbols) > 0 {
		url += "&symbols=" + strings.Join(symbols, ",")
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("APCA-API-KEY-ID", c.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", c.apiSecret)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result alpacaNewsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse news: %w", err)
	}
	return result.News, nil
}
