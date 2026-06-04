// Package pipeline fetches a block and its execution witness from an EL node
// and encodes them into the SSZ binary format expected by zesu-zkvm guests.
//
// Output layout:
//
//	[u64 LE: ssz_len] [SszStatelessInput bytes, padded to 8-byte boundary]
//
// SSZ schema: 2-byte big-endian schema_id (0x0001) followed by SszStatelessInput
// as defined in stateless_ssz.py (zkevm@v0.4.1 / bal-devnet-7).
package pipeline

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
	"github.com/Gabriel-Trintinalia/stateless-executor/pool"
)

// BlockMeta holds all SszExecutionPayload fields for logging.
type BlockMeta struct {
	// header
	ParentHash    string
	Coinbase      string
	StateRoot     string
	ReceiptsRoot  string
	PrevRandao    string
	ExtraData     string
	// numeric
	TxCount       int
	WithdrawCount int
	GasUsed       uint64
	GasLimit      uint64
	Timestamp     uint64
	BaseFee       uint64
	BlobGasUsed   uint64
	ExcessBlobGas uint64
	SlotNumber    uint64
	BALBytes      int
}

// witness mirrors the JSON returned by debug_executionWitness.
type witness struct {
	State           []string        `json:"state"`
	Codes           []string        `json:"codes"`
	Keys            []string        `json:"keys"`
	Headers         json.RawMessage `json:"headers"` // []string (hex) or []object depending on client
}

// Fetch retrieves the block RLP and witness for blockNum, then encodes them
// into the SSZ guest input format. Returns the encoded bytes, the EL
// node hostname that served the witness, and lightweight block metadata.
// genesis is optional; if non-nil its SszChainConfig is derived from the
// block timestamp; if nil the hardcoded Amsterdam mainnet constant is used.
func Fetch(ctx context.Context, p *pool.Pool, blockNum uint64, genesis *fixture.GenesisChainConfig, verbose bool, engineURL, jwtSecretFile string) ([]byte, string, BlockMeta, error) {
	rawURL := p.Pick()
	if rawURL == "" {
		return nil, "", BlockMeta{}, fmt.Errorf("pipeline: no healthy EL node available")
	}
	elNode := hostname(rawURL)

	hexNum := "0x" + strconv.FormatUint(blockNum, 16)

	// Fetch raw block RLP.
	log.Printf("block #%d: fetching block", blockNum)
	rawBlockResult, err := p.CallRaw(ctx, rawURL, "debug_getRawBlock", []interface{}{hexNum})
	if err != nil {
		return nil, elNode, BlockMeta{}, fmt.Errorf("pipeline: debug_getRawBlock(%d): %w", blockNum, err)
	}
	blockRLP, err := decodeHexResult(rawBlockResult)
	if err != nil {
		return nil, elNode, BlockMeta{}, fmt.Errorf("pipeline: decoding block RLP: %w", err)
	}

	// Decode block RLP — required for both metadata and SSZ encoding.
	block, err := decodeBlock(blockRLP)
	if err != nil {
		return nil, elNode, BlockMeta{}, fmt.Errorf("pipeline: block RLP decode: %w", err)
	}
	var baseFee, blobGasUsed, excessBlobGas, slotNumber uint64
	if block.BaseFee() != nil {
		baseFee = block.BaseFee().Uint64()
	}
	if block.BlobGasUsed() != nil {
		blobGasUsed = *block.BlobGasUsed()
	}
	if block.ExcessBlobGas() != nil {
		excessBlobGas = *block.ExcessBlobGas()
	}
	if block.SlotNumber() != nil {
		slotNumber = *block.SlotNumber()
	}
	meta := BlockMeta{
		ParentHash:    block.ParentHash().Hex(),
		Coinbase:      block.Coinbase().Hex(),
		StateRoot:     block.Root().Hex(),
		ReceiptsRoot:  block.ReceiptHash().Hex(),
		PrevRandao:    block.MixDigest().Hex(),
		ExtraData:     fmt.Sprintf("0x%x", block.Extra()),
		TxCount:       block.Transactions().Len(),
		WithdrawCount: len(block.Withdrawals()),
		GasUsed:       block.GasUsed(),
		GasLimit:      block.GasLimit(),
		Timestamp:     block.Time(),
		BaseFee:       baseFee,
		BlobGasUsed:   blobGasUsed,
		ExcessBlobGas: excessBlobGas,
		SlotNumber:    slotNumber,
	}

	// Fetch execution witness.
	log.Printf("block #%d: fetching witness", blockNum)
	witnessResult, err := p.CallRaw(ctx, rawURL, "debug_executionWitness", []interface{}{hexNum})
	if err != nil {
		return nil, elNode, meta, fmt.Errorf("pipeline: debug_executionWitness(%d): %w", blockNum, err)
	}
	var w witness
	if err := json.Unmarshal(witnessResult, &w); err != nil {
		return nil, elNode, meta, fmt.Errorf("pipeline: decoding witness JSON: %w", err)
	}

	// Decode headers: reth/besu return []hex-string; geth returns []header-object.
	// Both are normalised to [][]byte (RLP-encoded headers).
	headers, err := decodeHeaders(w.Headers)
	if err != nil {
		return nil, elNode, meta, fmt.Errorf("pipeline: decoding headers: %w", err)
	}

	state, err := decodeHexArray(w.State)
	if err != nil {
		return nil, elNode, meta, fmt.Errorf("pipeline: decoding witness state: %w", err)
	}
	codes, err := decodeHexArray(w.Codes)
	if err != nil {
		return nil, elNode, meta, fmt.Errorf("pipeline: decoding witness codes: %w", err)
	}

	// Fetch BAL — prefer engine_getPayloadBodiesByHashV2 (raw RLP),
	// fall back to reconstructing from eth_getBlockAccessList JSON.
	log.Printf("block #%d: fetching BAL", blockNum)
	var balBytes []byte
	if engineURL != "" && jwtSecretFile != "" {
		balBytes, err = fetchBALFromEngine(ctx, engineURL, jwtSecretFile, block.Hash())
	}
	if balBytes == nil {
		var fallbackErr error
		balBytes, fallbackErr = fetchBAL(ctx, p, rawURL, hexNum)
		if fallbackErr != nil {
			log.Printf("block #%d: BAL fetch skipped: %v", blockNum, fallbackErr)
			balBytes = nil
		}
	}
	if len(balBytes) > 0 {
		verifyBALHash(blockNum, balBytes, block)
	}
	meta.BALBytes = len(balBytes)

	if verbose {
		log.Printf("block #%d [%s]: witness state=%d codes=%d keys=%d headers=%d (raw=%s)",
			blockNum, elNode, len(w.State), len(w.Codes), len(w.Keys), len(headers), w.Headers)
	}

	var chainCfg []byte
	if genesis != nil {
		chainCfg = genesis.SszChainConfig(block.Time())
	}

	encoded, err := fixture.ZesuInputSSZFromBlock(block, state, codes, headers, balBytes, chainCfg)
	return encoded, elNode, meta, err
}

// decodeHeaders parses the headers field from debug_executionWitness.
// Reth/besu return []hex-string (RLP-encoded); geth returns []header-object.
// Both are normalised to raw RLP bytes.
func decodeHeaders(raw json.RawMessage) ([][]byte, error) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "[]" {
		return nil, nil
	}
	// Reth/besu format: []hex-string, each already RLP-encoded.
	var hexStrs []string
	if err := json.Unmarshal(raw, &hexStrs); err == nil {
		return decodeHexArray(hexStrs)
	}
	// Geth format: []header-object — unmarshal and RLP-encode each one.
	var headers []*types.Header
	if err := json.Unmarshal(raw, &headers); err != nil {
		return nil, fmt.Errorf("unrecognised headers format: %w", err)
	}
	out := make([][]byte, len(headers))
	for i, h := range headers {
		b, err := rlp.EncodeToBytes(h)
		if err != nil {
			return nil, fmt.Errorf("RLP-encoding header %d: %w", i, err)
		}
		out[i] = b
	}
	return out, nil
}

// decodeHexResult unmarshals a JSON string like "\"0x...\"" into raw bytes.
func decodeHexResult(raw json.RawMessage) ([]byte, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return hexToBytes(s)
}

// decodeHexArray decodes a []string of 0x-prefixed hex values into [][]byte.
func decodeHexArray(arr []string) ([][]byte, error) {
	out := make([][]byte, len(arr))
	for i, s := range arr {
		b, err := hexToBytes(s)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		out[i] = b
	}
	return out, nil
}

func hexToBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

// fetchBALFromEngine calls engine_getPayloadBodiesByHashV2 with JWT auth
// and returns the raw RLP-encoded blockAccessList bytes.
func fetchBALFromEngine(ctx context.Context, engineURL, jwtSecretFile string, blockHash common.Hash) ([]byte, error) {
	secretHex, err := os.ReadFile(jwtSecretFile)
	if err != nil {
		return nil, fmt.Errorf("reading JWT secret: %w", err)
	}
	secret, err := hex.DecodeString(strings.TrimSpace(strings.TrimPrefix(string(secretHex), "0x")))
	if err != nil {
		return nil, fmt.Errorf("decoding JWT secret: %w", err)
	}

	// Build HS256 JWT with iat claim.
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pay := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"iat":%d}`, time.Now().Unix())))
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(hdr + "." + pay))
	jwt := hdr + "." + pay + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1,
		"method": "engine_getPayloadBodiesByHashV2",
		"params": []interface{}{[]string{blockHash.Hex()}},
	})

	req, err := http.NewRequestWithContext(ctx, "POST", engineURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Result []struct {
			BlockAccessList string `json:"blockAccessList"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("engine API: %s", result.Error.Message)
	}
	if len(result.Result) == 0 || result.Result[0].BlockAccessList == "" {
		return nil, nil
	}
	return hexToBytes(result.Result[0].BlockAccessList)
}

// fetchBAL calls eth_getBlockAccessList, parses the response into a
// ConstructionBlockAccessList, and returns its RLP encoding.
// Returns nil, nil for pre-Amsterdam blocks (method not found).
func fetchBAL(ctx context.Context, p *pool.Pool, rawURL, hexNum string) ([]byte, error) {
	raw, err := p.CallRaw(ctx, rawURL, "eth_getBlockAccessList", []interface{}{hexNum})
	if err != nil {
		return nil, err
	}

	var resp struct {
		AccountChanges []balAccountChangeJSON `json:"accountChanges"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decoding BAL JSON: %w", err)
	}

	cbal := bal.NewConstructionBlockAccessList()
	for _, ac := range resp.AccountChanges {
		addr := common.HexToAddress(ac.Address)
		cbal.AccountRead(addr)
		for _, slot := range ac.StorageReads {
			cbal.StorageRead(addr, common.HexToHash(slot))
		}
		for _, sw := range ac.StorageChanges {
			key := common.HexToHash(sw.Slot)
			for _, ch := range sw.Changes {
				val := common.HexToHash(ch.NewValue)
				cbal.StorageWrite(uint32(ch.TxIndex), addr, key, val)
			}
		}
		for _, bc := range ac.BalanceChanges {
			n, _ := uint256.FromHex(bc.PostBalance)
			if n == nil {
				n = uint256.NewInt(0)
			}
			cbal.BalanceChange(uint32(bc.TxIndex), addr, n)
		}
		for _, nc := range ac.NonceChanges {
			cbal.NonceChange(addr, uint32(nc.TxIndex), nc.NewNonce)
		}
		if ac.CodeChange != nil {
			code, _ := hexToBytes(ac.CodeChange.NewCode)
			cbal.CodeChange(addr, uint32(ac.CodeChange.TxIndex), code)
		}
	}

	var buf bytes.Buffer
	if err := cbal.EncodeRLP(&buf); err != nil {
		return nil, fmt.Errorf("RLP-encoding BAL: %w", err)
	}
	return buf.Bytes(), nil
}

// verifyBALHash logs a warning if the encoded BAL bytes don't hash to the
// expected hash from the block header.
func verifyBALHash(blockNum uint64, balBytes []byte, block *types.Block) {
	expected := block.Header().BlockAccessListHash
	if expected == nil {
		return
	}
	var enc bal.BlockAccessList
	if err := rlp.DecodeBytes(balBytes, &enc); err != nil {
		log.Printf("block #%d: BAL hash check: decode error: %v", blockNum, err)
		return
	}
	got := enc.Hash()
	if got != *expected {
		log.Printf("block #%d: BAL hash MISMATCH: got=%s want=%s", blockNum, got.Hex(), expected.Hex())
	} else {
		log.Printf("block #%d: BAL hash OK (%s)", blockNum, got.Hex()[:10])
	}
}

type balAccountChangeJSON struct {
	Address        string                `json:"address"`
	StorageReads   []string              `json:"storageReads"`
	StorageChanges []balStorageChangeJSON `json:"storageChanges"`
	BalanceChanges []balBalanceChangeJSON `json:"balanceChanges"`
	NonceChanges   []balNonceChangeJSON   `json:"nonceChanges"`
	CodeChange     *balCodeChangeJSON     `json:"codeChange"`
}

type balStorageChangeJSON struct {
	Slot    string               `json:"slot"`
	Changes []balStorageWriteJSON `json:"changes"`
}

type balStorageWriteJSON struct {
	TxIndex  int    `json:"txIndex"`
	NewValue string `json:"newValue"`
}

type balBalanceChangeJSON struct {
	TxIndex     int    `json:"txIndex"`
	PostBalance string `json:"postBalance"`
}

type balNonceChangeJSON struct {
	TxIndex  int    `json:"txIndex"`
	NewNonce uint64 `json:"newNonce"`
}

type balCodeChangeJSON struct {
	TxIndex int    `json:"txIndex"`
	NewCode string `json:"newCode"`
}

func decodeBlock(blockRLP []byte) (*types.Block, error) {
	var block types.Block
	if err := rlp.DecodeBytes(blockRLP, &block); err != nil {
		return nil, err
	}
	return &block, nil
}

// hostname extracts the host portion from a URL (e.g. "http://el-2-geth:8545" → "el-2-geth").
func hostname(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}
