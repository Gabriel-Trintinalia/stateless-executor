// Package fixture parses the stateless-validator JSON fixture format
// (produced by the reth/alloy serde_bincode_compat::Block serializer —
// snake_case field names, mixed int/hex types) and re-encodes blocks as
// the zesu-zkvm SSZ binary format consumed by ziskemu.
package fixture

import (
	"encoding/json"
	"fmt"
	"os"
)

// FixtureFile is the top-level JSON fixture format.
type FixtureFile struct {
	Name           string         `json:"name"`
	Network        string         `json:"network"`
	StatelessInput StatelessInput `json:"stateless_input"`
	Success        bool           `json:"success"`
}

// StatelessInput holds the decoded block and execution witness.
type StatelessInput struct {
	Block   FixtureBlock `json:"block"`
	Witness WitnessData  `json:"witness"`
	// chain_config intentionally skipped; not needed for zesu input
}

// FixtureBlock holds the decoded header + body.
type FixtureBlock struct {
	Header FixtureHeader `json:"header"`
	Body   FixtureBody   `json:"body"`
}

// WitnessData holds the execution witness arrays (already RLP/hex-encoded items).
type WitnessData struct {
	State   []string `json:"state"`
	Codes   []string `json:"codes"`
	Keys    []string `json:"keys"`
	Headers []string `json:"headers"`
}

// FixtureHeader mirrors the snake_case block header produced by reth's
// serde_bincode_compat serializer.  Numeric fields that can be either a
// plain JSON integer OR a 0x-hex string are captured as json.RawMessage and
// decoded lazily by hexOrIntToUint64 / hexOrIntToBigInt.
type FixtureHeader struct {
	ParentHash            string           `json:"parent_hash"`
	OmmersHash            string           `json:"ommers_hash"`
	Beneficiary           string           `json:"beneficiary"`
	StateRoot             string           `json:"state_root"`
	TransactionsRoot      string           `json:"transactions_root"`
	ReceiptsRoot          string           `json:"receipts_root"`
	WithdrawalsRoot       *string          `json:"withdrawals_root"`
	LogsBloom             string           `json:"logs_bloom"`
	Difficulty            string           `json:"difficulty"` // always hex string "0x..."
	Number                uint64           `json:"number"`
	GasLimit              uint64           `json:"gas_limit"`
	GasUsed               uint64           `json:"gas_used"`
	Timestamp             uint64           `json:"timestamp"`
	MixHash               string           `json:"mix_hash"`
	Nonce                 string           `json:"nonce"`             // hex "0x0000000000000000"
	BaseFeePerGas         json.RawMessage  `json:"base_fee_per_gas"` // int or "0x..." hex
	BlobGasUsed           *uint64          `json:"blob_gas_used"`
	ExcessBlobGas         *uint64          `json:"excess_blob_gas"`
	ParentBeaconBlockRoot *string          `json:"parent_beacon_block_root"`
	RequestsHash          *string          `json:"requests_hash"`
	SlotNumber            *uint64          `json:"slot_number"`
	ExtraData             string           `json:"extra_data"`
}

// FixtureBody holds the block body (transactions, ommers, withdrawals).
type FixtureBody struct {
	Transactions    []FixtureTx         `json:"transactions"`
	Ommers          []json.RawMessage   `json:"ommers"` // always empty for post-merge
	Withdrawals     []FixtureWithdrawal `json:"withdrawals"`
	BlockAccessList *string             `json:"block_access_list"` // hex-encoded RLP bytes; nil for pre-Amsterdam
}

// FixtureTx wraps a signature and a discriminated-union transaction.
type FixtureTx struct {
	Signature   FixtureSig                        `json:"signature"`
	Transaction map[string]json.RawMessage        `json:"transaction"` // key = type name
}

// FixtureSig holds the ECDSA signature components.
type FixtureSig struct {
	R       string `json:"r"`
	S       string `json:"s"`
	YParity string `json:"yParity"` // "0x0" or "0x1"
	V       string `json:"v"`       // raw v (same as yParity for typed txs)
}

// FixtureWithdrawal uses camelCase (as produced by alloy's serde).
type FixtureWithdrawal struct {
	Index          string `json:"index"`
	ValidatorIndex string `json:"validatorIndex"`
	Address        string `json:"address"`
	Amount         string `json:"amount"`
}

// --- Per-type transaction bodies ---

// Eip1559TxBody: snake_case, numeric fields as plain ints.
type Eip1559TxBody struct {
	ChainID              uint64          `json:"chain_id"`
	Nonce                uint64          `json:"nonce"`
	GasLimit             uint64          `json:"gas_limit"`
	MaxFeePerGas         uint64          `json:"max_fee_per_gas"`
	MaxPriorityFeePerGas uint64          `json:"max_priority_fee_per_gas"`
	To                   *string         `json:"to"`
	Value                string          `json:"value"` // hex string
	AccessList           json.RawMessage `json:"access_list"`
	Input                string          `json:"input"` // hex string
}

// LegacyTxBody: snake_case, numeric fields as ints; chain_id can be hex string or int.
type LegacyTxBody struct {
	ChainID  json.RawMessage `json:"chain_id"` // int 1 or "0x1"
	Nonce    uint64          `json:"nonce"`
	GasPrice uint64          `json:"gas_price"`
	GasLimit uint64          `json:"gas_limit"`
	To       *string         `json:"to"`
	Value    string          `json:"value"`
	Input    string          `json:"input"`
}

// Eip4844TxBody: camelCase, ALL fields are hex strings.
type Eip4844TxBody struct {
	ChainID              string          `json:"chainId"`
	Nonce                string          `json:"nonce"`
	Gas                  string          `json:"gas"` // note: "gas", not "gas_limit"
	MaxFeePerGas         string          `json:"maxFeePerGas"`
	MaxPriorityFeePerGas string          `json:"maxPriorityFeePerGas"`
	To                   string          `json:"to"`
	Value                string          `json:"value"`
	AccessList           json.RawMessage `json:"accessList"`
	BlobVersionedHashes  []string        `json:"blobVersionedHashes"`
	MaxFeePerBlobGas     string          `json:"maxFeePerBlobGas"`
	Input                string          `json:"input"`
}

// Eip2930TxBody: snake_case, numeric fields as ints (EIP-2930 access-list tx).
type Eip2930TxBody struct {
	ChainID    uint64          `json:"chain_id"`
	Nonce      uint64          `json:"nonce"`
	GasPrice   uint64          `json:"gas_price"`
	GasLimit   uint64          `json:"gas_limit"`
	To         *string         `json:"to"`
	Value      string          `json:"value"`
	AccessList json.RawMessage `json:"access_list"`
	Input      string          `json:"input"`
}

// Eip7702TxBody: snake_case, numeric fields as ints.
type Eip7702TxBody struct {
	ChainID              uint64              `json:"chain_id"`
	Nonce                uint64              `json:"nonce"`
	GasLimit             uint64              `json:"gas_limit"`
	MaxFeePerGas         uint64              `json:"max_fee_per_gas"`
	MaxPriorityFeePerGas uint64              `json:"max_priority_fee_per_gas"`
	To                   string              `json:"to"`
	Value                string              `json:"value"`
	AccessList           json.RawMessage     `json:"access_list"`
	AuthorizationList    []FixtureAuthorization `json:"authorization_list"`
	Input                string              `json:"input"`
}

// FixtureAuthorization is an EIP-7702 authorization tuple.
type FixtureAuthorization struct {
	Inner   AuthInner `json:"inner"`
	YParity string    `json:"yParity"`
	R       string    `json:"r"`
	S       string    `json:"s"`
}

// AuthInner is the inner content of an EIP-7702 authorization.
type AuthInner struct {
	ChainID string `json:"chainId"`
	Address string `json:"address"`
	Nonce   string `json:"nonce"`
}

// AccessTupleJSON is an access list entry (both snake_case and camelCase keys used).
type AccessTupleJSON struct {
	Address     string   `json:"address"`
	StorageKeys []string `json:"storageKeys"`
}

// LoadFile reads and parses a fixture JSON file.
func LoadFile(path string) (*FixtureFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f FixtureFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &f, nil
}

// ZkevmWitness holds the execution witness arrays from a zkevm blockchain test block.
type ZkevmWitness struct {
	State   []string `json:"state"`
	Codes   []string `json:"codes"`
	Headers []string `json:"headers"`
}

// ZkevmBlock is one block inside a zkevm blockchain test case.
type ZkevmBlock struct {
	ExecutionWitness     ZkevmWitness `json:"executionWitness"`
	StatelessInputBytes  string       `json:"statelessInputBytes"`  // hex-encoded SSZ SszStatelessInput (Amsterdam+)
	StatelessOutputBytes string       `json:"statelessOutputBytes"` // hex-encoded expected SSZ output
	// ExpectException is non-empty for blocks that are expected to be invalid.
	ExpectException string `json:"expectException"`
}

// ZkevmTestCase is one test case in the zkevm blockchain test format.
type ZkevmTestCase struct {
	Name    string       // top-level key from the JSON file
	Network string       `json:"network"`
	Blocks  []ZkevmBlock `json:"blocks"`
}

// LoadZkevmFile reads a zkevm blockchain test JSON file and returns all test cases.
// The format has one or more top-level keys, each naming a test case.
func LoadZkevmFile(path string) ([]*ZkevmTestCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var out []*ZkevmTestCase
	for name, v := range raw {
		var tc ZkevmTestCase
		if err := json.Unmarshal(v, &tc); err != nil {
			return nil, fmt.Errorf("parse test %q in %s: %w", name, path, err)
		}
		tc.Name = name
		out = append(out, &tc)
	}
	return out, nil
}

