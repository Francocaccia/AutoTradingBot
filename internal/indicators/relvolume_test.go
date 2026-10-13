package indicators

import "testing"

// rel_volume comes from the same Alpaca bars as the technicals, so fundamentals
// (Yahoo) must never overwrite it, even though they replace AvgVolume.
func TestRelVolumeSurvivesFundamentalsMerge(t *testing.T) {
	const bars = `{"AAA":{"price":10,"rsi":55,"volume":12345,"avg_volume":90000,"rel_volume":0.85,"source":"alpaca-iex"}}`
	p, _, _ := newTestProvider(bars, fundsJSON)
	res, err := p.Calculate("AAA")
	if err != nil {
		t.Fatal(err)
	}
	if res.RelVolume != 0.85 {
		t.Errorf("RelVolume = %v, want 0.85", res.RelVolume)
	}
	if res.AvgVolume != 5_000_000 {
		t.Errorf("AvgVolume must still come from fundamentals for the liquidity filter, got %v", res.AvgVolume)
	}

	// Fundamentals down: AvgVolume is zeroed (unknown) but RelVolume is kept.
	p, _, _ = newTestProvider(bars, `{"error":"Too Many Requests"}`)
	res, err = p.Calculate("AAA")
	if err != nil {
		t.Fatal(err)
	}
	if res.RelVolume != 0.85 || res.AvgVolume != 0 {
		t.Errorf("RelVolume=%v AvgVolume=%v, want 0.85 and 0", res.RelVolume, res.AvgVolume)
	}
}
