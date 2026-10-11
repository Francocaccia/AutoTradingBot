package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/francocaccia/quantengine/internal/indicators"
	"github.com/francocaccia/quantengine/internal/news"
	"github.com/francocaccia/quantengine/internal/portfolio"
)

const geminiAPI = "https://generativelanguage.googleapis.com/v1/models/gemini-2.5-flash:generateContent"

type Client struct {
	apiKey string
	http   *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{apiKey: apiKey, http: &http.Client{}}
}

type Decision struct {
	Action     string  `json:"action"`
	Quantity   int     `json:"quantity"`
	Reasoning  string  `json:"reasoning"`
	StopLoss   float64 `json:"stop_loss"`
	TakeProfit float64 `json:"take_profit"`
}

type PromptInput struct {
	Symbol     string
	Indicators *indicators.Result
	Position   *portfolio.Position
	News       []news.Article
}

func BuildPrompt(input PromptInput) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("You are QuantEngine, an autonomous AI trading bot.\nMake a trading decision for %s based on the context below.\n\nRULES:\n- Only stocks, ETFs, bonds. No crypto, no options.\n- Swing/position trading only — hold days to months.\n- Do NOT trade without a clear edge. If uncertain: HOLD.\n- Every decision must be justified by technicals AND news/macro.\n\n", input.Symbol))

	if input.Indicators != nil {
		ind := input.Indicators
		sb.WriteString("## TECHNICALS\n")
		sb.WriteString(fmt.Sprintf("Price: $%.2f | RSI: %.2f | Volume: %d\n", ind.Price, ind.RSI, ind.Volume))
		sb.WriteString(fmt.Sprintf("Beta: %.2f | P/E: %.2f\n", ind.Beta, ind.PE))
		sb.WriteString(fmt.Sprintf("Ichimoku — Tenkan: %.2f | Kijun: %.2f | SpanA: %.2f | SpanB: %.2f | Chikou: %.2f\n", ind.Ichimoku.Tenkan, ind.Ichimoku.Kijun, ind.Ichimoku.SpanA, ind.Ichimoku.SpanB, ind.Ichimoku.Chikou))
		sb.WriteString(fmt.Sprintf("MACD: %.4f | Signal: %.4f | Histogram: %.4f\n", ind.MACD.MACD, ind.MACD.Signal, ind.MACD.Histogram))
		sb.WriteString(fmt.Sprintf("Stoch Fast — K: %.2f | D: %.2f\n", ind.Stochastic.K, ind.Stochastic.D))
		sb.WriteString(fmt.Sprintf("Keltner — Upper: %.2f | Mid: %.2f | Lower: %.2f\n\n", ind.Keltner.Upper, ind.Keltner.Middle, ind.Keltner.Lower))
	}

	sb.WriteString("## CURRENT POSITION\n")
	if input.Position == nil {
		sb.WriteString("No open position.\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("Qty: %d @ avg $%.2f | P&L: %.2f%%\n\n", input.Position.Qty, input.Position.AvgPrice, input.Position.PLPercent))
	}

	sb.WriteString("## RECENT NEWS\n")
	for i, a := range input.News {
		if i >= 10 {
			break
		}
		sb.WriteString(fmt.Sprintf("- [%s] %s\n", a.Source, a.Headline))
	}

	sb.WriteString("\n## RESPONSE FORMAT\nRespond ONLY with valid JSON, no markdown, no backticks:\n{\n  \"action\": \"BUY\" or \"SELL\" or \"HOLD\",\n  \"quantity\": 0,\n  \"reasoning\": \"your reasoning here\",\n  \"stop_loss\": 0.0,\n  \"take_profit\": 0.0\n}")
	return sb.String()
}

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func (c *Client) Decide(prompt string) (*Decision, error) {
	reqBody, _ := json.Marshal(geminiRequest{
		Contents: []geminiContent{
			{Parts: []geminiPart{{Text: prompt}}},
		},
	})

	req, _ := http.NewRequest("POST", geminiAPI, bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var apiResp geminiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	if len(apiResp.Candidates) == 0 {
		return nil, fmt.Errorf("empty Gemini response: %s", string(body))
	}

	text := apiResp.Candidates[0].Content.Parts[0].Text
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	var decision Decision
	if err := json.Unmarshal([]byte(text), &decision); err != nil {
		return nil, fmt.Errorf("invalid JSON from Gemini: %s", text)
	}

	return &decision, nil
}
