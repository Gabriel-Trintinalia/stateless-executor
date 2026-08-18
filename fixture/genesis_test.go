package fixture

import (
	"encoding/json"
	"os"
	"testing"
)

// sampleGenesis covers Osaka at t=0, BPO1 at t=1000, Amsterdam at t=2000.
var sampleGenesis = map[string]interface{}{
	"config": map[string]interface{}{
		"chainId":       7014190335,
		"osakaTime":     0,
		"bpo1Time":      1000,
		"amsterdamTime": 2000,
		// amsterdam is deliberately absent from blobSchedule: kurtosis genesis
		// files declare amsterdamTime without one, and the fork must still be
		// recognised.
		"blobSchedule": map[string]interface{}{
			"osaka": map[string]uint64{"target": 9, "max": 12, "baseFeeUpdateFraction": 5007716},
			"bpo1":  map[string]uint64{"target": 14, "max": 21, "baseFeeUpdateFraction": 11685759},
		},
	},
}

func writeTempGenesis(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(sampleGenesis)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "genesis*.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestParseGenesisFile(t *testing.T) {
	g, err := ParseGenesisFile(writeTempGenesis(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if g.ChainID != 7014190335 {
		t.Errorf("chainID = %d, want 7014190335", g.ChainID)
	}
	if g.ForkCount() != 3 {
		t.Errorf("fork count = %d, want 3", g.ForkCount())
	}
}

func TestActiveProtocolFork(t *testing.T) {
	g, err := ParseGenesisFile(writeTempGenesis(t))
	if err != nil {
		t.Fatal(err)
	}
	// Osaka at t=0, BPO1 at t=1000, Amsterdam at t=2000 — the last declared
	// without a blob schedule.
	for _, tc := range []struct {
		ts   uint64
		want uint8
	}{
		{0, forkOsaka},
		{999, forkOsaka},
		{1000, forkBPO1},
		{1999, forkBPO1},
		{2000, forkAmsterdam},
		{99999, forkAmsterdam},
	} {
		if got := g.ActiveProtocolFork(tc.ts); got != tc.want {
			t.Errorf("ts=%d: fork = 0x%02x, want 0x%02x", tc.ts, got, tc.want)
		}
	}
}
