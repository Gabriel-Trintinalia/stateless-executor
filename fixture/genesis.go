package fixture

import (
	"encoding/json"
	"fmt"
	"os"
)

// forkDef maps a genesis.json field name to its ProtocolFork index, oldest to
// newest.
//
// Indices are the reference enum verbatim (execution-specs
// src/ethereum/forks/amsterdam/stateless.py at tests-zkevm@v0.8.0) — they are
// stamped into the stateless input's schema id, which is how the guest learns
// which fork to execute the block under.
var forkDefs = []struct {
	timeField string // genesis config key (e.g. "cancunTime")
	enumVal   uint8  // ProtocolFork index
}{
	{"cancunTime", forkCancun},
	{"pragueTime", forkPrague},
	{"osakaTime", forkOsaka},
	{"bpo1Time", forkBPO1},
	{"bpo2Time", forkBPO2},
	{"amsterdamTime", forkAmsterdam},
}

// GenesisChainConfig holds the executor-relevant subset of a genesis.json.
type GenesisChainConfig struct {
	ChainID uint64
	// activeForks contains only forks that have both an activation timestamp
	// and a blob schedule entry in genesis, ordered oldest-first.
	activeForks []genesisFork
}

type genesisFork struct {
	enumVal        uint8
	activationTime uint64
}

// ParseGenesisFile reads a genesis.json and returns the chain config the
// pipeline needs. zkevm@v0.8.0 dropped SszChainConfig from the stateless
// input in favour of a bare chain_id, so the parsed fork list is now only
// informational (ForkCount).
func ParseGenesisFile(path string) (*GenesisChainConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("genesis: read %s: %w", path, err)
	}

	// Parse config as a raw field map so we can extract the *Time fields
	// without enumerating every fork name in a struct.
	var top struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("genesis: parse %s: %w", path, err)
	}

	var chainID uint64
	if raw, ok := top.Config["chainId"]; ok {
		_ = json.Unmarshal(raw, &chainID)
	}

	cfg := &GenesisChainConfig{ChainID: chainID}
	for _, fd := range forkDefs {
		raw, ok := top.Config[fd.timeField]
		if !ok {
			continue
		}
		var t uint64
		if err := json.Unmarshal(raw, &t); err != nil {
			continue // field present but not a plain uint64 — skip
		}
		// Keyed off the activation timestamp alone. Requiring a blobSchedule
		// entry (as this did when the fork descriptor still carried one) drops
		// forks that genesis declares without one — kurtosis emits exactly that
		// for amsterdamTime, and the fork would silently resolve to BPO2.
		cfg.activeForks = append(cfg.activeForks, genesisFork{
			enumVal:        fd.enumVal,
			activationTime: t,
		})
	}
	if len(cfg.activeForks) == 0 {
		return nil, fmt.Errorf("genesis: no supported fork activation times found in %s", path)
	}
	return cfg, nil
}

// DefaultMainnetChainConfig returns the mainnet chain config to fall back to
// when no GENESIS_FILE is supplied. Activation timestamps are mirrored from
// zesu's own mainnet schedule (src/stateless/hardfork.zig mainnetSpec) — the
// guest's single source of truth for which fork is live at a given block
// time. Amsterdam has no timestamp there yet (mainnet is currently on BPO2),
// so it's omitted here too; add it once zesu pins it, so this stays in
// lockstep with the guest instead of guessing ahead of it.
func DefaultMainnetChainConfig() *GenesisChainConfig {
	return &GenesisChainConfig{
		ChainID: 1,
		activeForks: []genesisFork{
			{enumVal: forkCancun, activationTime: 1710338135},
			{enumVal: forkPrague, activationTime: 1746612311},
			{enumVal: forkOsaka, activationTime: 1764798551},
			{enumVal: forkBPO1, activationTime: 1765290071},
			{enumVal: forkBPO2, activationTime: 1767747671},
		},
	}
}

// ForkCount returns the number of forks parsed from genesis.
func (g *GenesisChainConfig) ForkCount() int { return len(g.activeForks) }

// ActiveProtocolFork returns the ProtocolFork index active at blockTimestamp,
// or 0 if no fork in the genesis has activated yet (callers fall back to
// Amsterdam).
func (g *GenesisChainConfig) ActiveProtocolFork(blockTimestamp uint64) uint8 {
	for i := len(g.activeForks) - 1; i >= 0; i-- {
		if blockTimestamp >= g.activeForks[i].activationTime {
			return g.activeForks[i].enumVal
		}
	}
	return 0
}
