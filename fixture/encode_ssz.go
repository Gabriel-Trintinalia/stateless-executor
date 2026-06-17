package fixture

// SSZ encoder for SszStatelessInput (bal-devnet-7 / zkevm@v0.4.1).
//
// Implements the container layout from stateless_ssz.py, matched to what
// zesu's ssz.zig decoder expects. Key divergences from spec:
//   - SszWithdrawal.amount encoded as uint64 (8 bytes), not uint256 (32 bytes)
//   - base_fee_per_gas encoded as uint256 (32 bytes LE); zesu reads low 8 bytes only
//
// Stateless input bytes layout (v0.4.1):
//   [0..2]    schema_id (big-endian uint16, fixed at 0x0001)
//   --- SszStatelessInput container ---
//   [0..4]    offset → new_payload_request   (variable)
//   [4..8]    offset → witness               (variable)
//   [8..12]   offset → chain_config          (variable; SszChainConfig)
//   [12..16]  offset → public_keys           (variable; packed ByteVector[65])
//
// SszChainConfig embeds the full active fork descriptor (fork enum,
// activation timestamps, blob schedule). For mainnet/Amsterdam the body is
// a 68-byte constant (sszChainConfigAmsterdamMainnet) — the only target of
// the v0.4.1 zkevm fixtures.
//
// SszExecutionPayload fixed region (540 bytes): see encodeSszExecutionPayload.

import (
	"bytes"
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// statelessInputSchemaID is the 2-byte big-endian prefix on every
// bal-devnet-7 / zkevm@v0.4.1 stateless input. See STATELESS_INPUT_SCHEMA_ID
// in stateless_ssz.py.
const statelessInputSchemaID = uint16(0x0001)

// sszChainConfigAmsterdamMainnet is the SSZ-encoded body of SszChainConfig
// for mainnet at Amsterdam — { chain_id: 1, active_fork: { fork: Amsterdam,
// activation: { block_number: [], timestamp: [0] }, blob_schedule: [{
// target: 14, max: 21, base_fee_update_fraction: 0xB24B3F }] } }.
//
// The constant matches the trailer hardcoded by zesu's ssz_output.zig.
// 68 bytes total (offsets relative to start of chain_config).
var sszChainConfigAmsterdamMainnet = [68]byte{
	// chain_id = 1 (uint64 LE)
	0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	// offset → active_fork (= 12)
	0x0c, 0x00, 0x00, 0x00,
	// active_fork.fork = 24 (Amsterdam ProtocolFork enum, uint64 LE)
	0x18, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	// offset → activation (= 16, within active_fork)
	0x10, 0x00, 0x00, 0x00,
	// offset → blob_schedule (= 32, within active_fork)
	0x20, 0x00, 0x00, 0x00,
	// activation.block_number — offset 8 (empty optional list)
	0x08, 0x00, 0x00, 0x00,
	// activation.timestamp — offset 8 (1-element optional list)
	0x08, 0x00, 0x00, 0x00,
	// activation.timestamp[0] = 0 (uint64 LE)
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	// blob_schedule[0].target = 14 (uint64 LE)
	0x0e, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	// blob_schedule[0].max = 21 (uint64 LE)
	0x15, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	// blob_schedule[0].base_fee_update_fraction = 0xB24B3F (uint64 LE)
	0x3f, 0x4b, 0xb2, 0x00, 0x00, 0x00, 0x00, 0x00,
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

// encodeSszStatelessInput serialises SszStatelessInput (v0.4.1).
//
// Layout:
//
//	[0..2]   schema_id (big-endian 0x0001) — outside the container
//	--- SszStatelessInput container (all 4 fields variable) ---
//	[0..4]   offset → new_payload_request
//	[4..8]   offset → witness
//	[8..12]  offset → chain_config
//	[12..16] offset → public_keys
//	[16..]   variable section, in order: npr, witness, chain_config, public_keys
//
// public_keys is SszList[ByteVector[65], MAX_PUBLIC_KEYS] — fixed-size 65-byte
// elements packed back-to-back. We always emit zero public keys (no pre-
// recovered signatures on the offline SSZ path) → 0 bytes.
func encodeSszStatelessInput(f *FixtureFile, txs types.Transactions, withdrawals []*types.Withdrawal, parentBeaconRoot common.Hash) ([]byte, error) {
	npr, err := encodeSszNewPayloadRequest(f, txs, withdrawals, parentBeaconRoot)
	if err != nil {
		return nil, err
	}
	wit, err := encodeSszExecutionWitness(f.StatelessInput.Witness)
	if err != nil {
		return nil, err
	}
	chainCfg := sszChainConfigAmsterdamMainnet[:]
	var pubKeys []byte // empty packed ByteVector[65] list

	// Fixed region: four uint32 offsets = 16 bytes.
	const fixedSize = 16
	offNPR := uint32(fixedSize)
	offWitness := offNPR + uint32(len(npr))
	offChainCfg := offWitness + uint32(len(wit))
	offPubKeys := offChainCfg + uint32(len(chainCfg))

	var out bytes.Buffer
	// Schema-id prefix (big-endian uint16).
	var sid [2]byte
	binary.BigEndian.PutUint16(sid[:], statelessInputSchemaID)
	out.Write(sid[:])
	// Container body.
	writeU32LE(&out, offNPR)
	writeU32LE(&out, offWitness)
	writeU32LE(&out, offChainCfg)
	writeU32LE(&out, offPubKeys)
	out.Write(npr)
	out.Write(wit)
	out.Write(chainCfg)
	out.Write(pubKeys)
	return out.Bytes(), nil
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
	fix.Write(mustHexToBytes(h.ParentHash))              // [0..32]
	writeAddress(&fix, h.Beneficiary)                    // [32..52]
	fix.Write(mustHexToBytes(h.StateRoot))               // [52..84]
	fix.Write(mustHexToBytes(h.ReceiptsRoot))            // [84..116]
	writeBloom(&fix, h.LogsBloom)                        // [116..372]
	fix.Write(mustHexToBytes(h.MixHash))                 // [372..404]
	binary.Write(&fix, binary.LittleEndian, h.Number)    // [404..412]
	binary.Write(&fix, binary.LittleEndian, h.GasLimit)  // [412..420]
	binary.Write(&fix, binary.LittleEndian, h.GasUsed)   // [420..428]
	binary.Write(&fix, binary.LittleEndian, h.Timestamp) // [428..436]
	writeU32LE(&fix, extraDataOff)                       // [436..440]
	fix.Write(sszUint256(baseFee))                       // [440..472]
	fix.Write(make([]byte, 32))                          // [472..504] block_hash (zeros — unused for execution)
	writeU32LE(&fix, txsOff)                             // [504..508]
	writeU32LE(&fix, wdsOff)                             // [508..512]
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
	binary.Write(&fix, binary.LittleEndian, slotNumber) // [532..540] slot_number (0 = absent)

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
