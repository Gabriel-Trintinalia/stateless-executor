// Package pipeline fetches a block and its execution witness from an EL node
// and encodes them into the binary format expected by zevm-stateless guests.
//
// Binary layout (all integers big-endian, from src/io.zig):
//
//	[u64: block_rlp_len] [block_rlp_bytes]
//	[u64: state_count]   ( [u64: len] [bytes] ) * state_count
//	[u64: codes_count]   ( [u64: len] [bytes] ) * codes_count
//	[u64: keys_count]    ( [u64: len] [bytes] ) * keys_count
//	[u64: headers_count] ( [u64: len] [bytes] ) * headers_count
package pipeline

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/Gabriel-Trintinalia/stateless-executor/pool"
)

// BlockMeta holds lightweight block metadata derived from the fetched block RLP.
type BlockMeta struct {
	TxCount int
	GasUsed uint64
}

// witness mirrors the JSON returned by debug_executionWitness.
type witness struct {
	State   []string        `json:"state"`
	Codes   []string        `json:"codes"`
	Keys    []string        `json:"keys"`
	Headers json.RawMessage `json:"headers"` // []string (hex) or []object depending on client
}

// Fetch retrieves the block RLP and witness for blockNum, then encodes them
// into the binary guest input format. Returns the encoded bytes, the EL
// node hostname that served the witness, and lightweight block metadata.
func Fetch(ctx context.Context, p *pool.Pool, blockNum uint64) ([]byte, string, BlockMeta, error) {
	rawURL := p.Pick()
	if rawURL == "" {
		return nil, "", BlockMeta{}, fmt.Errorf("pipeline: no healthy EL node available")
	}
	elNode := hostname(rawURL)

	hexNum := "0x" + strconv.FormatUint(blockNum, 16)

	// Fetch raw block RLP.
	rawBlockResult, err := p.CallRaw(ctx, rawURL, "debug_getRawBlock", []interface{}{hexNum})
	if err != nil {
		return nil, elNode, BlockMeta{}, fmt.Errorf("pipeline: debug_getRawBlock(%d): %w", blockNum, err)
	}
	blockRLP, err := decodeHexResult(rawBlockResult)
	if err != nil {
		return nil, elNode, BlockMeta{}, fmt.Errorf("pipeline: decoding block RLP: %w", err)
	}

	// Decode block RLP to extract metadata.
	var meta BlockMeta
	var block types.Block
	if err := rlp.DecodeBytes(blockRLP, &block); err == nil {
		meta.TxCount = block.Transactions().Len()
		meta.GasUsed = block.GasUsed()
	}

	// Fetch execution witness.
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

	log.Printf("block #%d [%s]: witness state=%d codes=%d keys=%d headers=%d (raw=%s)",
		blockNum, elNode, len(w.State), len(w.Codes), len(w.Keys), len(headers), w.Headers)

	encoded, err := encode(blockRLP, w, headers)
	return encoded, elNode, meta, err
}

// encode serialises blockRLP + witness into the binary guest format.
func encode(blockRLP []byte, w witness, headers [][]byte) ([]byte, error) {
	var buf bytes.Buffer

	// [u64: block_rlp_len] [block_rlp_bytes]
	if err := writeUint64(&buf, uint64(len(blockRLP))); err != nil {
		return nil, err
	}
	buf.Write(blockRLP)

	// Three hex-string arrays: state, codes, keys.
	for _, arr := range [][]string{w.State, w.Codes, w.Keys} {
		decoded, err := decodeHexArray(arr)
		if err != nil {
			return nil, err
		}
		if err := writeArray(&buf, decoded); err != nil {
			return nil, err
		}
	}

	// Headers (already decoded).
	if err := writeArray(&buf, headers); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
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

// writeArray writes [u64 count] followed by [u64 len][bytes] for each element.
func writeArray(buf *bytes.Buffer, items [][]byte) error {
	if err := writeUint64(buf, uint64(len(items))); err != nil {
		return err
	}
	for _, item := range items {
		if err := writeUint64(buf, uint64(len(item))); err != nil {
			return err
		}
		buf.Write(item)
	}
	return nil
}

func writeUint64(buf *bytes.Buffer, v uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, err := buf.Write(b[:])
	return err
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

// hostname extracts the host portion from a URL (e.g. "http://el-2-geth:8545" → "el-2-geth").
func hostname(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}
