package broker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every order type the bot submits carries an "atb-" client_order_id, and ids are unique.
func TestOrdersCarryClientOrderIDPrefix(t *testing.T) {
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			ClientOrderID string `json:"client_order_id"`
		}
		json.Unmarshal(b, &body)
		ids = append(ids, body.ClientOrderID)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewAlpacaClient("k", "s", srv.URL)
	c.PlaceMarketOrder("SPY", 1, Buy)
	c.PlaceBracketOrder("SPY", 1, Buy, 90, 120)
	c.PlaceOTOOrder("IEF", 1, Buy, 90)
	if len(ids) != 3 {
		t.Fatalf("ids = %v", ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !strings.HasPrefix(id, ClientOrderPrefix) || len(id) > 128 || seen[id] {
			t.Errorf("bad client_order_id %q (ids %v)", id, ids)
		}
		seen[id] = true
	}
}
