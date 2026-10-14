package broker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Bond modes (BOND_MODE).
const (
	BondModeAuto   = "auto"
	BondModeETF    = "etf"
	BondModeNative = "native"
	BondModeOff    = "off"
)

// Capabilities is what the paper account reports about fixed-income trading.
type Capabilities struct {
	// FixedIncomeStatus is the HTTP status of GET /v2/assets?asset_class=fixed_income
	// (0 when the request itself failed).
	FixedIncomeStatus int
	FixedIncomeCount  int    // assets returned when the status was 200
	Detail            string // truncated response body or error text, for the log
}

// NativeBonds reports whether individual fixed-income instruments are
// tradable: the endpoint answered 200 with at least one asset.
func (c Capabilities) NativeBonds() bool {
	return c.FixedIncomeStatus == http.StatusOK && c.FixedIncomeCount > 0
}

// String is the one-line capabilities log entry.
func (c Capabilities) String() string {
	switch {
	case c.NativeBonds():
		return fmt.Sprintf("fixed_income: HTTP 200, %d assets (native bonds available)", c.FixedIncomeCount)
	case c.FixedIncomeStatus == http.StatusOK:
		return "fixed_income: HTTP 200, 0 assets (native bonds NOT available)"
	case c.FixedIncomeStatus != 0:
		return fmt.Sprintf("fixed_income: HTTP %d %q (native bonds NOT available)", c.FixedIncomeStatus, c.Detail)
	}
	return fmt.Sprintf("fixed_income: request failed (%s); treated as NOT available", c.Detail)
}

// ProbeCapabilities asks Alpaca for active fixed-income assets. Nothing is
// assumed: any non-200 answer, an empty list or a transport error means native
// bonds are unavailable.
func (c *AlpacaClient) ProbeCapabilities() Capabilities {
	q := url.Values{"status": {"active"}, "asset_class": {"fixed_income"}}
	data, status, err := c.doRequest("GET", "/v2/assets?"+q.Encode(), nil)
	caps := Capabilities{FixedIncomeStatus: status}
	if err != nil {
		caps.Detail = truncateBody(err.Error(), 160)
		return caps
	}
	var assets []json.RawMessage
	if json.Unmarshal(data, &assets) != nil {
		caps.Detail = truncateBody(string(data), 160)
		return caps
	}
	caps.FixedIncomeCount = len(assets)
	return caps
}

// ResolveBondMode turns BOND_MODE and the probed capabilities into the mode
// requested/resolved and the mode the bot can actually execute.
//
//   - auto: native if the account supports it, otherwise etf.
//   - native: requires support; without it falls back to etf.
//   - etf / off: as given.
//
// The bot has no native fixed-income order flow, so a resolved "native" is
// executed as "etf" and the note says so: bond trading is never faked.
func ResolveBondMode(mode string, caps Capabilities) (resolved, effective, note string) {
	switch mode {
	case BondModeETF, BondModeOff, BondModeNative, BondModeAuto:
	default:
		mode = BondModeAuto
	}
	switch mode {
	case BondModeOff:
		return BondModeOff, BondModeOff, "bond sleeve disabled"
	case BondModeETF:
		return BondModeETF, BondModeETF, "bond ETFs"
	}
	if caps.NativeBonds() {
		return BondModeNative, BondModeETF, "native fixed income is available on this account, but the bot has no native order flow; trading bond ETFs instead"
	}
	if mode == BondModeNative {
		return BondModeETF, BondModeETF, "BOND_MODE=native requested but the account does not support native bonds; using bond ETFs"
	}
	return BondModeETF, BondModeETF, "native bonds not supported on this account; using bond ETFs"
}

func truncateBody(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
