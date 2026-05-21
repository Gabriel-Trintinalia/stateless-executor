package fixture

// SSZ encoder for SszStatelessInput (Amsterdam stateless spec).
//
// Implements the container layout from stateless_ssz.py, matched to what
// zesu's ssz.zig decoder expects. Key divergence from spec:
//   - SszWithdrawal.amount encoded as uint64 (8 bytes), not uint256 (32 bytes)
//   - base_fee_per_gas encoded as uint256 (32 bytes LE); zesu reads low 8 bytes only
//
// SszStatelessInput fixed region (20 bytes):
//   [0..4]   offset → new_payload_request
//   [4..8]   offset → witness
//   [8..16]  chain_config.chain_id (uint64 LE)
//   [16..20] offset → public_keys
//
// SszExecutionPayload fixed region (540 bytes): see encodeSszExecutionPayload.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// injectForkName takes raw statelessInputBytes (hex) and rebuilds the SSZ with a 24-byte
// fixed region that adds fork_name as a 5th variable field. Without this, test fixtures
// with synthetic timestamps (e.g. 1000) resolve to Frontier via mainnetSpec.
//
// Original layout (off_npr == 20):
//   [0..4] off_npr [4..8] off_witness [8..16] chain_id [16..20] off_pubkeys
//
// Extended layout (off_npr == 24):
//   [0..4] off_npr [4..8] off_witness [8..16] chain_id [16..20] off_pubkeys [20..24] off_fork_name
//
// Returns (padded_payload, content_len). The ziskemu input_len header must be set to
// content_len (not len(padded_payload)) so that fork_name_bytes = data[off_fork_name..content_len]
// contains exactly the fork name with no trailing padding zeros.
func injectForkName(statelessInputHex string, forkName string) ([]byte, int, error) {
	ssz, err := hexStrToBytes(statelessInputHex)
	if err != nil {
		return nil, 0, fmt.Errorf("decode hex: %w", err)
	}
	if len(ssz) < 20 {
		return nil, 0, fmt.Errorf("SSZ too short (%d bytes)", len(ssz))
	}

	offNPR := binary.LittleEndian.Uint32(ssz[0:4])
	if offNPR != 20 {
		// Already extended or unknown format — return as-is.
		contentLen := len(ssz)
		for len(ssz)%8 != 0 {
			ssz = append(ssz, 0)
		}
		return ssz, contentLen, nil
	}

	// All variable-field offsets shift by 4 (fixed region grows 20→24).
	offWitness := binary.LittleEndian.Uint32(ssz[4:8]) + 4
	chainID := binary.LittleEndian.Uint64(ssz[8:16])
	offPubkeys := binary.LittleEndian.Uint32(ssz[16:20]) + 4

	varData := ssz[20:] // NPR + Witness + PubKeys
	forkNameBytes := []byte(forkName)
	offForkName := uint32(24) + uint32(len(varData)) // fork_name comes after all existing variable data

	var hdr bytes.Buffer
	writeU32LE(&hdr, 24) // off_npr — marks extended format
	writeU32LE(&hdr, offWitness)
	binary.Write(&hdr, binary.LittleEndian, chainID)
	writeU32LE(&hdr, offPubkeys)
	writeU32LE(&hdr, offForkName)

	content := append(hdr.Bytes(), varData...)
	content = append(content, forkNameBytes...)
	contentLen := len(content)
	for len(content)%8 != 0 {
		content = append(content, 0)
	}
	return content, contentLen, nil
}

// ZesuInputSSZPlain encodes a fixture as a plain SSZ blob with no zisk framing.
func ZesuInputSSZPlain(f *FixtureFile) ([]byte, error) {
	txs, err := buildTransactions(f.StatelessInput.Block.Body.Transactions)
	if err != nil {
		return nil, err
	}
	withdrawals, err := buildWithdrawals(f.StatelessInput.Block.Body.Withdrawals)
	if err != nil {
		return nil, err
	}

	var parentBeaconRoot common.Hash
	if f.StatelessInput.Block.Header.ParentBeaconBlockRoot != nil {
		parentBeaconRoot = hexToHash(*f.StatelessInput.Block.Header.ParentBeaconBlockRoot)
	}

	return encodeSszStatelessInput(f, txs, withdrawals, parentBeaconRoot)
}

// ZesuInputSSZ encodes a fixture as a zkvm-ready input using the SSZ path.
// No 32-byte root prefix — zesu-zkvm computes new_payload_request_root internally.
func ZesuInputSSZ(f *FixtureFile) ([]byte, error) {
	payload, err := ZesuInputSSZPlain(f)
	if err != nil {
		return nil, err
	}

	// Ziskemu requires file payload to be a multiple of 8 bytes (memory alignment).
	// Write the exact SSZ content length in the framing header so that
	// read_input_slice() returns only the SSZ bytes — trailing padding zeros would
	// otherwise land in pubkeys_data and trigger InvalidSsz (first_off == 0).
	sszLen := len(payload)
	for len(payload)%8 != 0 {
		payload = append(payload, 0)
	}
	var out bytes.Buffer
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(sszLen))
	out.Write(lenBuf[:])
	out.Write(payload)
	return out.Bytes(), nil
}

// encodeSszStatelessInput serialises SszStatelessInput.
func encodeSszStatelessInput(f *FixtureFile, txs types.Transactions, withdrawals []*types.Withdrawal, parentBeaconRoot common.Hash) ([]byte, error) {
	npr, err := encodeSszNewPayloadRequest(f, txs, withdrawals, parentBeaconRoot)
	if err != nil {
		return nil, err
	}
	wit, err := encodeSszExecutionWitness(f.StatelessInput.Witness)
	if err != nil {
		return nil, err
	}
	pubKeys := encodeSszByteListList(nil) // no pre-recovered keys on SSZ path

	// Fixed region: 4 (npr offset) + 4 (wit offset) + 8 (chain_id) + 4 (pk offset) = 20
	const fixedSize = 20
	var fix bytes.Buffer
	writeU32LE(&fix, uint32(fixedSize))
	writeU32LE(&fix, uint32(fixedSize+len(npr)))
	binary.Write(&fix, binary.LittleEndian, uint64(1)) // chain_id = 1 (mainnet)
	writeU32LE(&fix, uint32(fixedSize+len(npr)+len(wit)))

	return append(fix.Bytes(), append(npr, append(wit, pubKeys...)...)...), nil
}

// encodeSszNewPayloadRequest serialises SszNewPayloadRequest.
// Fixed region: 4+4+32+4 = 44 bytes.
func encodeSszNewPayloadRequest(f *FixtureFile, txs types.Transactions, withdrawals []*types.Withdrawal, parentBeaconRoot common.Hash) ([]byte, error) {
	ep, err := encodeSszExecutionPayload(f, txs, withdrawals)
	if err != nil {
		return nil, err
	}
	vh := encodeSszVersionedHashes(txs)
	er := encodeSszExecutionRequests() // empty for pre-Prague blocks

	// Fixed: 4 (ep offset) + 4 (vh offset) + 32 (parent_beacon_block_root) + 4 (er offset) = 44
	const fixedSize = 44
	var fix bytes.Buffer
	writeU32LE(&fix, uint32(fixedSize))
	writeU32LE(&fix, uint32(fixedSize+len(ep)))
	fix.Write(parentBeaconRoot[:])
	writeU32LE(&fix, uint32(fixedSize+len(ep)+len(vh)))

	return append(fix.Bytes(), append(ep, append(vh, er...)...)...), nil
}

// encodeSszExecutionPayload serialises SszExecutionPayload.
// Fixed region: 540 bytes (see layout in file header comment).
func encodeSszExecutionPayload(f *FixtureFile, txs types.Transactions, withdrawals []*types.Withdrawal) ([]byte, error) {
	h := f.StatelessInput.Block.Header

	extraData := mustHexToBytes(h.ExtraData)

	// Encode raw transaction bytes for SSZ (signed envelope: type || RLP for typed, RLP for legacy).
	rawTxs := make([][]byte, len(txs))
	for i, tx := range txs {
		b, err := tx.MarshalBinary()
		if err != nil {
			return nil, err
		}
		rawTxs[i] = b
	}
	txsSSZ := encodeSszByteListList(rawTxs)

	wdsSSZ := encodeSszWithdrawalList(withdrawals)

	baseFee, err := rawJSONToBigInt(h.BaseFeePerGas)
	if err != nil {
		return nil, err
	}

	// BAL bytes: nil for pre-Amsterdam blocks.
	var balBytes []byte
	if f.StatelessInput.Block.Body.BlockAccessList != nil {
		balBytes = mustHexToBytes(*f.StatelessInput.Block.Body.BlockAccessList)
	}

	// Variable fields and their offsets relative to start of EP.
	const fixedSize = 540
	extraDataOff := uint32(fixedSize)
	txsOff := extraDataOff + uint32(len(extraData))
	wdsOff := txsOff + uint32(len(txsSSZ))
	balOff := wdsOff + uint32(len(wdsSSZ))

	var fix bytes.Buffer
	fix.Write(mustHexToBytes(h.ParentHash))          // [0..32]
	writeAddress(&fix, h.Beneficiary)                // [32..52]
	fix.Write(mustHexToBytes(h.StateRoot))            // [52..84]
	fix.Write(mustHexToBytes(h.ReceiptsRoot))         // [84..116]
	writeBloom(&fix, h.LogsBloom)                     // [116..372]
	fix.Write(mustHexToBytes(h.MixHash))              // [372..404]
	binary.Write(&fix, binary.LittleEndian, h.Number) // [404..412]
	binary.Write(&fix, binary.LittleEndian, h.GasLimit) // [412..420]
	binary.Write(&fix, binary.LittleEndian, h.GasUsed)  // [420..428]
	binary.Write(&fix, binary.LittleEndian, h.Timestamp) // [428..436]
	writeU32LE(&fix, extraDataOff)                    // [436..440]
	fix.Write(sszUint256(baseFee))                    // [440..472]
	fix.Write(make([]byte, 32))                        // [472..504] block_hash (zeros — unused for execution)
	writeU32LE(&fix, txsOff)                          // [504..508]
	writeU32LE(&fix, wdsOff)                          // [508..512]
	blobGasUsed := uint64(0)
	if h.BlobGasUsed != nil {
		blobGasUsed = *h.BlobGasUsed
	}
	excessBlobGas := uint64(0)
	if h.ExcessBlobGas != nil {
		excessBlobGas = *h.ExcessBlobGas
	}
	binary.Write(&fix, binary.LittleEndian, blobGasUsed)   // [512..520]
	binary.Write(&fix, binary.LittleEndian, excessBlobGas) // [520..528]
	writeU32LE(&fix, balOff)                               // [528..532] block_access_list offset
	slotNumber := uint64(0)
	if h.SlotNumber != nil {
		slotNumber = *h.SlotNumber
	}
	binary.Write(&fix, binary.LittleEndian, slotNumber)    // [532..540] slot_number (0 = absent)

	var out bytes.Buffer
	out.Write(fix.Bytes())
	out.Write(extraData)
	out.Write(txsSSZ)
	out.Write(wdsSSZ)
	out.Write(balBytes)
	return out.Bytes(), nil
}

// encodeSszExecutionWitness serialises SszExecutionWitness.
// Fixed region: 4+4+4 = 12 bytes (3 variable fields).
func encodeSszExecutionWitness(w WitnessData) ([]byte, error) {
	state, err := decodeHexArray(w.State)
	if err != nil {
		return nil, err
	}
	codes, err := decodeHexArray(w.Codes)
	if err != nil {
		return nil, err
	}
	headers, err := decodeHexArray(w.Headers)
	if err != nil {
		return nil, err
	}

	stateSSZ := encodeSszByteListList(state)
	codesSSZ := encodeSszByteListList(codes)
	headersSSZ := encodeSszByteListList(headers)

	const fixedSize = 12
	var fix bytes.Buffer
	writeU32LE(&fix, uint32(fixedSize))
	writeU32LE(&fix, uint32(fixedSize+len(stateSSZ)))
	writeU32LE(&fix, uint32(fixedSize+len(stateSSZ)+len(codesSSZ)))

	return append(fix.Bytes(), append(stateSSZ, append(codesSSZ, headersSSZ...)...)...), nil
}

// encodeSszByteListList encodes a List[ByteList[N], M] (variable-size elements).
// Encoding: [offset_0, ..., offset_n-1, data_0, ..., data_n-1]
// Each offset is uint32 LE, relative to start of the list.
func encodeSszByteListList(items [][]byte) []byte {
	if len(items) == 0 {
		return []byte{}
	}
	headerSize := 4 * len(items)
	var offsets, data bytes.Buffer
	off := uint32(headerSize)
	for _, item := range items {
		writeU32LE(&offsets, off)
		data.Write(item)
		off += uint32(len(item))
	}
	return append(offsets.Bytes(), data.Bytes()...)
}

// encodeSszWithdrawalList encodes List[SszWithdrawal, N] (fixed-size elements, no offset table).
// SszWithdrawal: index(8) + validator_index(8) + address(20) + amount(uint64=8) = 44 bytes.
func encodeSszWithdrawalList(wds []*types.Withdrawal) []byte {
	var buf bytes.Buffer
	for _, w := range wds {
		binary.Write(&buf, binary.LittleEndian, w.Index)
		binary.Write(&buf, binary.LittleEndian, w.Validator)
		addr := w.Address.Bytes()
		buf.Write(addr)
		binary.Write(&buf, binary.LittleEndian, w.Amount)
	}
	return buf.Bytes()
}

// encodeSszVersionedHashes encodes List[Bytes32, 4096] (fixed-size, no offset table).
// Collects blob versioned hashes from all blob transactions.
func encodeSszVersionedHashes(txs types.Transactions) []byte {
	var buf bytes.Buffer
	for _, tx := range txs {
		if tx.Type() == types.BlobTxType {
			for _, h := range tx.BlobHashes() {
				buf.Write(h[:])
			}
		}
	}
	return buf.Bytes()
}

// encodeSszExecutionRequests encodes an empty SszExecutionRequests container.
// Container has 3 variable fields (deposits, withdrawals, consolidations).
// Empty: fixed region = 12 bytes, all offsets point to 12 (no variable data).
func encodeSszExecutionRequests() []byte {
	var buf bytes.Buffer
	writeU32LE(&buf, 12)
	writeU32LE(&buf, 12)
	writeU32LE(&buf, 12)
	return buf.Bytes()
}

// ── SSZ primitive helpers ─────────────────────────────────────────────────────

func writeU32LE(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

// sszUint256 encodes a *big.Int as 32 bytes little-endian.
func sszUint256(n *big.Int) []byte {
	b := make([]byte, 32)
	if n != nil && n.Sign() > 0 {
		nb := n.Bytes() // big-endian from big.Int
		for i, byt := range nb {
			b[len(nb)-1-i] = byt // reverse to little-endian
		}
	}
	return b
}

func writeAddress(buf *bytes.Buffer, hex string) {
	addr := hexToAddress(hex)
	buf.Write(addr[:])
}

func writeBloom(buf *bytes.Buffer, hex string) {
	bloom := hexToBloom(hex)
	buf.Write(bloom[:])
}
