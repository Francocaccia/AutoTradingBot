package gemini

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BatchHeadlines is how many of the most recent headlines per symbol a batch
// prompt shows. The catalyst check must use the same count.
const BatchHeadlines = 5

// batchRules adds the batch framing to the shared decision rules.
const batchRules = `BATCH:
- Below are several symbols. Decide EACH one independently, using only that symbol's own section plus the shared PORTFOLIO block. One symbol's answer must not depend on another's.
- Return one JSON object per symbol, with "symbol" copied exactly as given. Do not skip a symbol: answer HOLD when there is nothing to do.
- The PORTFOLIO block is shared. A symbol's own CANDIDATE FIT lines (when present) belong to that symbol only. A catalyst quoted from the PORTFOLIO block may be one line of the shared block or of that symbol's CANDIDATE FIT.
- A stock BUY catalyst must be a headline from THAT symbol's RECENT NEWS section, not from another symbol's.
- Several BUYs at once are fine only if each one fits the portfolio; the bot enforces trade caps, so rank by conviction honestly.

`

// BuildBatchPrompt renders one prompt that asks for a decision on every item.
// shared is the PORTFOLIO block (rendered once); each item's Portfolio field is
// only that symbol's own part (its CANDIDATE FIT), and its News is cut to the
// BatchHeadlines most recent.
func BuildBatchPrompt(shared string, items []PromptInput) string {
	var sb strings.Builder
	sb.WriteString("You are QuantEngine, an autonomous AI paper-trading portfolio manager.\n")
	sb.WriteString(goal)
	syms := make([]string, len(items))
	for i, it := range items {
		syms[i] = it.Symbol
	}
	sb.WriteString(fmt.Sprintf("Make a decision for each of these %d symbols based ONLY on the context below: %s.\n\n", len(items), strings.Join(syms, ", ")))
	sb.WriteString(rules)
	sb.WriteString(batchRules)
	if shared != "" {
		sb.WriteString(shared)
		sb.WriteString("\n")
	}
	for _, it := range items {
		it.MaxHeadlines = BatchHeadlines
		sb.WriteString(fmt.Sprintf("# SYMBOL: %s\n", it.Symbol))
		writeContext(&sb, it)
		sb.WriteString("\n")
	}
	sb.WriteString(`## RESPONSE FORMAT
Respond with a JSON array with exactly one object per symbol above. Each object has:
"symbol" (exactly as given), "action" (BUY, SELL or HOLD), "conviction" (integer 1-10), "role" (core, growth, income, hedge, cash_parking, or none for HOLD),
"thesis" (one sentence), "reasoning" (brief, citing the data), "catalyst" (stock: the exact headline text from that symbol's RECENT NEWS that supports a BUY; bond ETF: ONE verbatim line from the PORTFOLIO block; otherwise the word none),
"portfolio_rationale" (one or two sentences on how it fits the portfolio and changes risk; required for BUY and SELL), "replaces" (held symbol being rotated out, or empty), "take_profit" (0 to let the bot set it),
"suggested_tickers" (up to 3 other US-listed symbols from the news worth evaluating next cycle, or an empty list).`)
	return sb.String()
}

// batchSchema is the responseSchema that makes Gemini return the batch as a
// JSON array of decision objects.
func batchSchema() map[string]any {
	str := map[string]any{"type": "STRING"}
	return map[string]any{
		"type": "ARRAY",
		"items": map[string]any{
			"type": "OBJECT",
			"properties": map[string]any{
				"symbol":              str,
				"action":              map[string]any{"type": "STRING", "enum": []string{"BUY", "SELL", "HOLD"}},
				"conviction":          map[string]any{"type": "INTEGER"},
				"role":                map[string]any{"type": "STRING", "enum": append(append([]string{}, Roles...), "none")},
				"thesis":              str,
				"reasoning":           str,
				"catalyst":            str,
				"portfolio_rationale": str,
				"replaces":            str,
				"take_profit":         map[string]any{"type": "NUMBER"},
				"suggested_tickers":   map[string]any{"type": "ARRAY", "items": str},
			},
			"required": []string{"symbol", "action", "conviction", "role", "thesis", "catalyst", "portfolio_rationale"},
		},
	}
}

// BatchResult is one element of a batch answer. Items are parsed one at a
// time: a malformed element has Err set (and Symbol when it could be read)
// without affecting the others.
type BatchResult struct {
	Symbol   string
	Decision *Decision
	Err      error
}

// batchItem is a Decision plus the symbol it answers.
type batchItem struct {
	Symbol string `json:"symbol"`
	Decision
}

// parseBatch reads a batch answer. Only a reply that is not a JSON array at
// all is an error; each element is validated on its own.
func parseBatch(text string) ([]BatchResult, error) {
	text = extractJSONArray(text)
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(text), &raws); err != nil {
		return nil, fmt.Errorf("batch reply is not a JSON array: %s", truncate(text, 200))
	}
	out := make([]BatchResult, 0, len(raws))
	for i, raw := range raws {
		var item batchItem
		if err := json.Unmarshal(raw, &item); err != nil {
			var s struct {
				Symbol string `json:"symbol"`
			}
			_ = json.Unmarshal(raw, &s)
			out = append(out, BatchResult{Symbol: strings.ToUpper(strings.TrimSpace(s.Symbol)), Err: fmt.Errorf("item %d: %v", i, err)})
			continue
		}
		item.Decision.normalize()
		sym := strings.ToUpper(strings.TrimSpace(item.Symbol))
		if sym == "" {
			out = append(out, BatchResult{Err: fmt.Errorf("item %d: no symbol", i)})
			continue
		}
		d := item.Decision
		out = append(out, BatchResult{Symbol: sym, Decision: &d})
	}
	return out, nil
}
