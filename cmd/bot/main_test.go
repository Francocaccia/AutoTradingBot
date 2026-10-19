package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// /health answers from memory: no upstream is configured here, so any call
// outside the process would fail or hang.
func TestHealthIsFastAndIndependentOfEverything(t *testing.T) {
	start := time.Now()
	rec := httptest.NewRecorder()
	healthHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("GET = %d %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("a health check must not be cached")
	}
	head := httptest.NewRecorder()
	healthHandler(head, httptest.NewRequest(http.MethodHead, "/health", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD = %d %q", head.Code, head.Body)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Errorf("/health took %v", time.Since(start))
	}
}

func TestEnvList(t *testing.T) {
	t.Setenv("X_LIST", " agg, shy ,,Bnd")
	got := envList("X_LIST")
	if len(got) != 3 || got[0] != "AGG" || got[1] != "SHY" || got[2] != "BND" {
		t.Errorf("envList = %v", got)
	}
	if envList("X_UNSET_LIST") != nil {
		t.Error("unset list is nil")
	}
}
