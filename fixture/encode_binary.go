package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// ZesuInputBinary encodes a fixture as a ziskemu-ready input using the binary format
// expected by zesu-zkvm's deserialize.zig.
//
// Wire format:
//
//	[8 bytes LE: content_len]         — ziskemu input-size framing header
//	[32 bytes: zeros]                 — new_payload_request_root placeholder
//	[8 bytes BE: block_rlp_len]
//	[block_rlp_len bytes: RLP block]
//	[8 bytes BE: state_count]   [for each: 8 bytes BE len + bytes]
//	[8 bytes BE: codes_count]   [for each: 8 bytes BE len + bytes]
//	[8 bytes BE: keys_count]    [for each: 8 bytes BE len + bytes]
//	[8 bytes BE: headers_count] [for each: 8 bytes BE len + bytes]
func ZesuInputBinary(f *FixtureFile) ([]byte, error) {
	txs, err := buildTransactions(f.StatelessInput.Block.Body.Transactions)
	if err != nil {
		return nil, err
	}
	withdrawals, err := buildWithdrawals(f.StatelessInput.Block.Body.Withdrawals)
	if err != nil {
		return nil, err
	}

	blockRLP, err := buildBlockRLP(f, txs, withdrawals)
	if err != nil {
		return nil, fmt.Errorf("block RLP: %w", err)
	}

	state, err := decodeHexArray(f.StatelessInput.Witness.State)
	if err != nil {
		return nil, err
	}
	codes, err := decodeHexArray(f.StatelessInput.Witness.Codes)
	if err != nil {
		return nil, err
	}
	keys, err := decodeHexArray(f.StatelessInput.Witness.Keys)
	if err != nil {
		return nil, err
	}
	headers, err := decodeHexArray(f.StatelessInput.Witness.Headers)
	if err != nil {
		return nil, err
	}

	var content bytes.Buffer
	content.Write(make([]byte, 32)) // new_payload_request_root placeholder (zeros)
	writeBE64(&content, uint64(len(blockRLP)))
	content.Write(blockRLP)
	writeBinaryArray(&content, state)
	writeBinaryArray(&content, codes)
	writeBinaryArray(&content, keys)
	writeBinaryArray(&content, headers)

	payload := content.Bytes()
	contentLen := len(payload)
	for len(payload)%8 != 0 {
		payload = append(payload, 0)
	}
	var out bytes.Buffer
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(contentLen))
	out.Write(lenBuf[:])
	out.Write(payload)
	return out.Bytes(), nil
}

// rlpBlockBody mirrors go-ethereum's internal extblock for RLP encoding.
type rlpBlockBody struct {
	Header      *types.Header
	Txs         []*types.Transaction
	Uncles      []*types.Header
	Withdrawals []*types.Withdrawal `rlp:"optional"`
}

func buildBlockRLP(f *FixtureFile, txs types.Transactions, withdrawals []*types.Withdrawal) ([]byte, error) {
	h := f.StatelessInput.Block.Header

	difficulty, err := hexToBigInt(h.Difficulty)
	if err != nil {
		return nil, fmt.Errorf("difficulty: %w", err)
	}
	baseFee, err := rawJSONToBigInt(h.BaseFeePerGas)
	if err != nil {
		return nil, fmt.Errorf("baseFee: %w", err)
	}

	nonceBytes := mustHexToBytes(h.Nonce)
	var nonce types.BlockNonce
	copy(nonce[:], nonceBytes)

	header := &types.Header{
		ParentHash:    hexToHash(h.ParentHash),
		UncleHash:     hexToHash(h.OmmersHash),
		Coinbase:      hexToAddress(h.Beneficiary),
		Root:          hexToHash(h.StateRoot),
		TxHash:        hexToHash(h.TransactionsRoot),
		ReceiptHash:   hexToHash(h.ReceiptsRoot),
		Bloom:         hexToBloom(h.LogsBloom),
		Difficulty:    difficulty,
		Number:        new(big.Int).SetUint64(h.Number),
		GasLimit:      h.GasLimit,
		GasUsed:       h.GasUsed,
		Time:          h.Timestamp,
		Extra:         mustHexToBytes(h.ExtraData),
		MixDigest:     hexToHash(h.MixHash),
		Nonce:         nonce,
		BaseFee:       baseFee,
		BlobGasUsed:   h.BlobGasUsed,
		ExcessBlobGas: h.ExcessBlobGas,
	}
	if h.WithdrawalsRoot != nil {
		wr := hexToHash(*h.WithdrawalsRoot)
		header.WithdrawalsHash = &wr
	}
	if h.ParentBeaconBlockRoot != nil {
		pbr := hexToHash(*h.ParentBeaconBlockRoot)
		header.ParentBeaconRoot = &pbr
	}
	if h.RequestsHash != nil {
		rh := hexToHash(*h.RequestsHash)
		header.RequestsHash = &rh
	}

	return rlp.EncodeToBytes(&rlpBlockBody{
		Header:      header,
		Txs:         txs,
		Uncles:      []*types.Header{},
		Withdrawals: withdrawals,
	})
}

func writeBE64(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}

func writeBinaryArray(buf *bytes.Buffer, items [][]byte) {
	writeBE64(buf, uint64(len(items)))
	for _, item := range items {
		writeBE64(buf, uint64(len(item)))
		buf.Write(item)
	}
}
