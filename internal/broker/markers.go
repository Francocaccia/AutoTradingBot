package broker

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Markers are small facts the bot must remember across restarts that no order
// records, for example "the weekly review ran". The service keeps nothing on disk
// and its memory is wiped by every restart, so the fact is written to the one
// place that survives: the Alpaca account. A marker is an empty watchlist whose
// name is the fact (for example "atb-weekly-review-2026-10-19"). Nothing else uses
// watchlists, so they do not mix with anything, and reading them is one call.

type watchlist struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *AlpacaClient) watchlists() ([]watchlist, error) {
	data, _, err := c.doRequest("GET", "/v2/watchlists", nil)
	if err != nil {
		return nil, err
	}
	var out []watchlist
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// HasMarker reports whether the marker exists. An error means "unknown", which a
// caller must not read as "absent".
func (c *AlpacaClient) HasMarker(name string) (bool, error) {
	lists, err := c.watchlists()
	if err != nil {
		return false, fmt.Errorf("reading markers: %w", err)
	}
	for _, w := range lists {
		if w.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// SetMarker records the marker. Setting one that exists is not an error.
func (c *AlpacaClient) SetMarker(name string) error {
	if has, err := c.HasMarker(name); err != nil || has {
		return err
	}
	if _, _, err := c.doRequest("POST", "/v2/watchlists", map[string]any{"name": name}); err != nil {
		return fmt.Errorf("writing marker: %w", err)
	}
	return nil
}

// ClearMarker removes the marker, if any.
func (c *AlpacaClient) ClearMarker(name string) error {
	lists, err := c.watchlists()
	if err != nil {
		return fmt.Errorf("reading markers: %w", err)
	}
	for _, w := range lists {
		if w.Name != name {
			continue
		}
		if _, status, err := c.doRequest("DELETE", "/v2/watchlists/"+w.ID, nil); err != nil && status != http.StatusNotFound {
			return fmt.Errorf("removing marker: %w", err)
		}
	}
	return nil
}
