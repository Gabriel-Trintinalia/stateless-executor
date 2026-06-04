package fixture

import (
	"bytes"
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
		"blobSchedule": map[string]interface{}{
			"osaka":     map[string]uint64{"target": 9, "max": 12, "baseFeeUpdateFraction": 5007716},
			"bpo1":      map[string]uint64{"target": 14, "max": 21, "baseFeeUpdateFraction": 11685759},
			"amsterdam": map[string]uint64{"target": 14, "max": 21, "baseFeeUpdateFraction": 11685759},
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

func TestSszChainConfig_ActiveFork(t *testing.T) {
	g, err := ParseGenesisFile(writeTempGenesis(t))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		ts          uint64
		wantFork    uint64 // ProtocolFork enum index
		wantBlobMax uint64
	}{
		{0, 24, 12},    // osakaTime maps to Amsterdam (enum 24)
		{999, 24, 12},  // still Amsterdam before BPO1
		{1000, 19, 21}, // BPO1 (enum 19) activates at t=1000
		{1999, 19, 21}, // still BPO1 before Amsterdam
		{2000, 24, 21}, // Amsterdam (enum 24) activates at t=2000
		{9999, 24, 21}, // Amsterdam stays active
	}

	for _, tc := range cases {
		cfg := g.SszChainConfig(tc.ts)
		if cfg == nil {
			t.Errorf("ts=%d: got nil chain config", tc.ts)
			continue
		}
		// Fork enum is at bytes [12..20] (uint64 LE) within SszChainConfig.
		// chain_id(8) + offset(4) = 12 byte offset to SszForkConfig start,
		// then fork enum is first field of SszForkConfig.
		gotFork := readU64LE(cfg, 12)
		// BlobMax is at bytes [52..60] (uint64 LE):
		// 12 (chain_config fixed) + 16 (fork_config fixed) + 16 (activation) + 8 (target) = 52
		gotMax := readU64LE(cfg, 52)

		if gotFork != tc.wantFork {
			t.Errorf("ts=%d: fork enum = %d, want %d", tc.ts, gotFork, tc.wantFork)
		}
		if gotMax != tc.wantBlobMax {
			t.Errorf("ts=%d: blob max = %d, want %d", tc.ts, gotMax, tc.wantBlobMax)
		}
	}
}

func TestBuildSszChainConfigMatchesConstant(t *testing.T) {
	got := buildSszChainConfig(1, 24, 0, 14, 21, 0xB24B3F)
	want := sszChainConfigAmsterdamMainnet[:]
	if !bytes.Equal(got, want) {
		t.Errorf("mismatch\ngot:  %x\nwant: %x", got, want)
	}
}

func readU64LE(b []byte, off int) uint64 {
	_ = b[off+7]
	return uint64(b[off]) | uint64(b[off+1])<<8 | uint64(b[off+2])<<16 |
		uint64(b[off+3])<<24 | uint64(b[off+4])<<32 | uint64(b[off+5])<<40 |
		uint64(b[off+6])<<48 | uint64(b[off+7])<<56
}
