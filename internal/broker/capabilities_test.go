package broker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func assetsServer(t *testing.T, status int, body string) *AlpacaClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/assets" || r.URL.Query().Get("asset_class") != "fixed_income" || r.URL.Query().Get("status") != "active" {
			t.Errorf("unexpected probe request: %s", r.URL.String())
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewAlpacaClient("k", "s", srv.URL)
}

func TestProbeCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantNative bool
		wantStatus int
		wantLog    string
	}{
		{"unsupported class is rejected", 422, `{"code":42210000,"message":"invalid asset_class: fixed_income"}`, false, 422, "NOT available"},
		{"forbidden", 403, `{"message":"forbidden"}`, false, 403, "HTTP 403"},
		{"empty list", 200, `[]`, false, 200, "0 assets"},
		{"native bonds listed", 200, `[{"symbol":"US912828ZT04","class":"fixed_income"}]`, true, 200, "native bonds available"},
		{"garbage body", 200, `<html>`, false, 200, "NOT available"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caps := assetsServer(t, tc.status, tc.body).ProbeCapabilities()
			if caps.NativeBonds() != tc.wantNative || caps.FixedIncomeStatus != tc.wantStatus {
				t.Errorf("caps = %+v", caps)
			}
			if !strings.Contains(caps.String(), tc.wantLog) {
				t.Errorf("log line %q lacks %q", caps.String(), tc.wantLog)
			}
		})
	}
	// An unreachable API is "not available", never "available".
	c := NewAlpacaClient("k", "s", "http://127.0.0.1:1")
	if caps := c.ProbeCapabilities(); caps.NativeBonds() || !strings.Contains(caps.String(), "request failed") {
		t.Errorf("unreachable: %+v / %s", caps, caps)
	}
}

func TestResolveBondModeAutoUsesETFsWhenNativeIsUnsupported(t *testing.T) {
	unsupported := assetsServer(t, 422, `{"message":"invalid asset_class"}`).ProbeCapabilities()
	supported := assetsServer(t, 200, `[{"symbol":"X"}]`).ProbeCapabilities()

	cases := []struct {
		mode, wantResolved, wantEffective string
		caps                              Capabilities
		wantNote                          string
	}{
		{"auto", "etf", "etf", unsupported, "not supported"},
		{"", "etf", "etf", unsupported, "not supported"},
		{"AUTO-typo", "etf", "etf", unsupported, "not supported"},
		{"native", "etf", "etf", unsupported, "does not support native"},
		{"etf", "etf", "etf", unsupported, "bond ETFs"},
		{"etf", "etf", "etf", supported, "bond ETFs"},
		{"off", "off", "off", unsupported, "disabled"},
		// Supported natively, but the bot has no native order flow: resolved says
		// native, effective stays ETF and the note says so. Bonds are never faked.
		{"auto", "native", "etf", supported, "no native order flow"},
		{"native", "native", "etf", supported, "no native order flow"},
	}
	for _, c := range cases {
		resolved, effective, note := ResolveBondMode(c.mode, c.caps)
		if resolved != c.wantResolved || effective != c.wantEffective || !strings.Contains(note, c.wantNote) {
			t.Errorf("ResolveBondMode(%q, native=%v) = %s/%s %q, want %s/%s containing %q",
				c.mode, c.caps.NativeBonds(), resolved, effective, note, c.wantResolved, c.wantEffective, c.wantNote)
		}
	}
}

func TestPlaceOTOOrderBody(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // the duplicate check
			w.Write([]byte(`[]`))
			return
		}
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		got = string(b[:n])
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := NewAlpacaClient("k", "s", srv.URL).PlaceOTOOrder("SHY", 50, Buy, 82.1, KindEntry); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"order_class":"oto"`, `"stop_price":"82.10"`, `"time_in_force":"gtc"`, `"symbol":"SHY"`} {
		if !strings.Contains(got, want) {
			t.Errorf("body %s lacks %s", got, want)
		}
	}
	if strings.Contains(got, "take_profit") {
		t.Errorf("an OTO order carries no take-profit: %s", got)
	}
}
