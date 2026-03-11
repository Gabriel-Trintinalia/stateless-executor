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

	"github.com/Gabriel-Trintinalia/stateless-executor/pool"
)

// witness mirrors the JSON returned by debug_executionWitness.
type witness struct {
	State   []string        `json:"state"`
	Codes   []string        `json:"codes"`
	Keys    []string        `json:"keys"`
	Headers json.RawMessage `json:"headers"` // []string (hex) or []object depending on client
}

// Fetch retrieves the block RLP and witness for blockNum, then encodes them
// into the binary guest input format. Returns the encoded bytes and the EL
// node hostname that served the witness.
func Fetch(ctx context.Context, p *pool.Pool, blockNum uint64) ([]byte, string, error) {
	rawURL := p.Pick()
	if rawURL == "" {
		return nil, "", fmt.Errorf("pipeline: no healthy EL node available")
	}
	elNode := hostname(rawURL)

	hexNum := "0x" + strconv.FormatUint(blockNum, 16)

	// Fetch raw block RLP.
	rawBlockResult, err := p.CallRaw(ctx, rawURL, "debug_getRawBlock", []interface{}{hexNum})
	if err != nil {
		return nil, elNode, fmt.Errorf("pipeline: debug_getRawBlock(%d): %w", blockNum, err)
	}
	blockRLP, err := decodeHexResult(rawBlockResult)
	if err != nil {
		return nil, elNode, fmt.Errorf("pipeline: decoding block RLP: %w", err)
	}

	// Fetch execution witness.
	witnessResult, err := p.CallRaw(ctx, rawURL, "debug_executionWitness", []interface{}{hexNum})
	if err != nil {
		return nil, elNode, fmt.Errorf("pipeline: debug_executionWitness(%d): %w", blockNum, err)
	}
	var w witness
	if err := json.Unmarshal(witnessResult, &w); err != nil {
		return nil, elNode, fmt.Errorf("pipeline: decoding witness JSON: %w", err)
	}

	// Decode headers: some clients return []hex-string; others (e.g. geth) return
	// an array of full header objects. If format is wrong, remove from pool.
	headers, formatOK, err := decodeHeaders(w.Headers)
	if err != nil {
		return nil, elNode, fmt.Errorf("pipeline: decoding headers: %w", err)
	}
	if !formatOK {
		log.Printf("pipeline: %s returned unsupported witness format; removing from pool", elNode)
		p.Remove(rawURL)
	}

	encoded, err := encode(blockRLP, w, headers)
	return encoded, elNode, err
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
// Returns the decoded bytes, a boolean indicating whether the format was
// recognised (false = unsupported, caller should remove that node), and any
// decode error.
func decodeHeaders(raw json.RawMessage) ([][]byte, bool, error) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "[]" {
		return nil, true, nil
	}
	// Expected format: []hex-string (besu, reth).
	var hexStrs []string
	if err := json.Unmarshal(raw, &hexStrs); err == nil {
		decoded, err := decodeHexArray(hexStrs)
		return decoded, true, err
	}
	// Unsupported format (e.g. geth returns full header objects).
	return nil, false, nil
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
