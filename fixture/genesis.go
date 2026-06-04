package fixture

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
)

// forkDef maps a genesis.json field name to its ProtocolFork SSZ enum index
// and blob-schedule key. Ordered from oldest to newest so the active-fork
// search can walk backwards and stop at the first activated entry.
//
// Enum indices match tuple(ProtocolFork) in stateless_ssz.py (Amsterdam spec):
//
//	Cancun=16 Prague=17 Osaka=18 BPO1=19 … BPO5=23 Amsterdam=24
var forkDefs = []struct {
	timeField string // genesis config key (e.g. "cancunTime")
	blobKey   string // blobSchedule map key (e.g. "cancun")
	enumVal   uint64 // ProtocolFork SSZ index
}{
	{"cancunTime", "cancun", 16},
	{"pragueTime", "prague", 17},
	{"osakaTime", "osaka", 24}, // genesis generators use osakaTime for Amsterdam activation
	{"bpo1Time", "bpo1", 19},
	{"bpo2Time", "bpo2", 20},
	{"bpo3Time", "bpo3", 21},
	{"bpo4Time", "bpo4", 22},
	{"bpo5Time", "bpo5", 23},
	{"amsterdamTime", "amsterdam", 24},
}

// GenesisChainConfig holds the executor-relevant subset of a genesis.json.
type GenesisChainConfig struct {
	ChainID uint64
	// activeForks contains only forks that have both an activation timestamp
	// and a blob schedule entry in genesis, ordered oldest-first.
	activeForks []genesisFork
}

type genesisFork struct {
	enumVal        uint64
	activationTime uint64
	blobTarget     uint64
	blobMax        uint64
	blobBaseFee    uint64
}

// raw genesis JSON shapes ─────────────────────────────────────────────────────

type blobEntry struct {
	Target                uint64 `json:"target"`
	Max                   uint64 `json:"max"`
	BaseFeeUpdateFraction uint64 `json:"baseFeeUpdateFraction"`
}

// ParseGenesisFile reads a genesis.json and returns the chain config needed
// to build SszChainConfig bytes for any block timestamp.
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

	var blobSchedule map[string]blobEntry
	if raw, ok := top.Config["blobSchedule"]; ok {
		_ = json.Unmarshal(raw, &blobSchedule)
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
		bs, ok := blobSchedule[fd.blobKey]
		if !ok {
			continue // fork defined but no blob schedule — skip
		}
		cfg.activeForks = append(cfg.activeForks, genesisFork{
			enumVal:        fd.enumVal,
			activationTime: t,
			blobTarget:     bs.Target,
			blobMax:        bs.Max,
			blobBaseFee:    bs.BaseFeeUpdateFraction,
		})
	}
	if len(cfg.activeForks) == 0 {
		return nil, fmt.Errorf("genesis: no supported forks with blob schedules found in %s", path)
	}
	return cfg, nil
}

// ForkCount returns the number of forks parsed from genesis.
func (g *GenesisChainConfig) ForkCount() int { return len(g.activeForks) }

// SszChainConfig returns the SSZ-encoded SszChainConfig body for the fork
// that is active at blockTimestamp. Returns nil if no fork has activated yet.
func (g *GenesisChainConfig) SszChainConfig(blockTimestamp uint64) []byte {
	// Walk from newest to oldest — return the first fork that has activated.
	for i := len(g.activeForks) - 1; i >= 0; i-- {
		f := g.activeForks[i]
		if blockTimestamp >= f.activationTime {
			return buildSszChainConfig(g.ChainID, f.enumVal, f.activationTime,
				f.blobTarget, f.blobMax, f.blobBaseFee)
		}
	}
	return nil
}

// buildSszChainConfig encodes SszChainConfig for a single active fork with one
// blob schedule entry. Layout mirrors sszChainConfigAmsterdamMainnet — see
// encode_ssz.go for the byte-by-byte commentary.
//
//	SszChainConfig       (12-byte fixed + variable)
//	  chain_id           uint64 LE           [0..8]
//	  offset_active_fork uint32 LE = 12      [8..12]
//	  SszForkConfig      (56 bytes)
//	    fork             uint64 LE           [0..8]
//	    offset_activation uint32 LE = 16     [8..12]
//	    offset_blob_sched uint32 LE = 32     [12..16]
//	    SszForkActivation (16 bytes)
//	      bn_offset      uint32 LE = 8       [0..4]
//	      ts_offset      uint32 LE = 8       [4..8]
//	      timestamp[0]   uint64 LE           [8..16]
//	    SszBlobSchedule  (24 bytes, fixed-size, no offset table)
//	      target         uint64 LE           [0..8]
//	      max            uint64 LE           [8..16]
//	      base_fee_frac  uint64 LE           [16..24]
func buildSszChainConfig(chainID, forkEnum, activationTime, blobTarget, blobMax, blobBaseFee uint64) []byte {
	out := make([]byte, 68)

	// SszChainConfig fixed region (12 bytes)
	binary.LittleEndian.PutUint64(out[0:], chainID)
	binary.LittleEndian.PutUint32(out[8:], 12) // offset → active_fork

	// SszForkConfig fixed region (16 bytes) at offset 12
	binary.LittleEndian.PutUint64(out[12:], forkEnum)
	binary.LittleEndian.PutUint32(out[20:], 16) // offset → activation (relative to fork_config start)
	binary.LittleEndian.PutUint32(out[24:], 32) // offset → blob_schedule

	// SszForkActivation (16 bytes) at offset 28
	binary.LittleEndian.PutUint32(out[28:], 8) // block_number offset (empty list)
	binary.LittleEndian.PutUint32(out[32:], 8) // timestamp offset (block_number is empty)
	binary.LittleEndian.PutUint64(out[36:], activationTime)

	// SszBlobSchedule (24 bytes) at offset 44
	binary.LittleEndian.PutUint64(out[44:], blobTarget)
	binary.LittleEndian.PutUint64(out[52:], blobMax)
	binary.LittleEndian.PutUint64(out[60:], blobBaseFee)

	return out
}
