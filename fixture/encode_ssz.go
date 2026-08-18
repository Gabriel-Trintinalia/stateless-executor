package fixture

// SSZ encoder for SszStatelessInput (glamsterdam-devnet-8 / zkevm@v0.8.0).
//
// Implements the container layout from stateless_ssz.py, verified field by
// field against the statelessInputBytes the reference emits in the
// tests-zkevm@v0.8.0 fixtures: 44-byte withdrawals (amount is a uint64),
// 32-byte little-endian base_fee_per_gas (zesu reads its low 8 bytes), and a
// 540-byte SszExecutionPayload fixed region.
//
// Stateless input bytes layout (v0.8.0):
//   [0..2]    schema_id (big-endian uint16, fixed at 0x1501)
//   --- SszStatelessInput container (20-byte fixed region) ---
//   [0..4]    offset → new_payload_request   (variable)
//   [4..8]    offset → witness               (variable)
//   [8..16]   chain_id                       (uint64 LE, inline)
//   [16..20]  offset → public_keys           (variable; packed ByteVector[65])
//
// v0.8.0 replaced the nested SszChainConfig — which carried the whole active
// fork descriptor (fork enum, activation timestamps, blob schedule) — with a
// bare inline chain_id, growing the fixed region from 16 to 20 bytes. The fork
// is now pinned by the schema id itself (0x15 = ProtocolFork.Amsterdam, 0x01 =
// schema revision), so no fork descriptor is encoded at all.
//
// The payload containers became EIP-7495 ProgressiveContainers and their lists
// EIP-7916 ProgressiveLists, but those serialize identically to the stable
// forms — only hash_tree_root changed, which is the guest's concern, not the
// encoder's. Every container below is therefore unchanged from v0.5.0.
//
// SszExecutionPayload fixed region (540 bytes): see encodeSszExecutionPayload.

import (
	"bytes"
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ProtocolFork indices, verbatim from the reference enum (execution-specs
// src/ethereum/forks/amsterdam/stateless.py at tests-zkevm@v0.8.0). The
// stateless input's schema id is `fork_index << 8 | revision`, so this is what
// tells the guest which rules to execute the block under.
const (
	forkShanghai  = 0x0F
	forkCancun    = 0x10
	forkPrague    = 0x11
	forkOsaka     = 0x12
	forkBPO1      = 0x13
	forkBPO2      = 0x14
	forkAmsterdam = 0x15
)

// statelessInputSchemaRevision is the payload encoding revision: 0x01 is
// SSZ encode(SszStatelessInput), the only revision defined.
const statelessInputSchemaRevision = 0x01

// statelessInputSchemaID is the Amsterdam schema id (0x1501) — what the
// zkevm@v0.8.0 fixtures carry, and the default when no fork can be resolved.
const statelessInputSchemaID = uint16(forkAmsterdam)<<8 | statelessInputSchemaRevision

// schemaIDFor builds the 2-byte schema id for a ProtocolFork index.
func schemaIDFor(fork uint8) uint16 {
	return uint16(fork)<<8 | statelessInputSchemaRevision
}

// activeProtocolFork returns the ProtocolFork index active at blockTimestamp,
// walking newest to oldest. Falls back to Amsterdam when the chain config is
// absent or names no activated fork — matching the guest's own default.
func activeProtocolFork(cc *FixtureChainConfig, blockTimestamp uint64) uint8 {
	if cc == nil {
		return forkAmsterdam
	}
	for _, f := range []struct {
		idx  uint8
		time *uint64
	}{
		{forkAmsterdam, cc.AmsterdamTime},
		{forkBPO2, cc.Bpo2Time},
		{forkBPO1, cc.Bpo1Time},
		{forkOsaka, cc.OsakaTime},
		{forkPrague, cc.PragueTime},
		{forkCancun, cc.CancunTime},
		{forkShanghai, cc.ShanghaiTime},
	} {
		if f.time != nil && blockTimestamp >= *f.time {
			return f.idx
		}
	}
	return forkAmsterdam
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

// encodeSszStatelessInput serialises SszStatelessInput (v0.8.0).
//
// Layout:
//
//	[0..2]   schema_id (big-endian 0x1501) — outside the container
//	--- SszStatelessInput container (20-byte fixed region) ---
//	[0..4]   offset → new_payload_request
//	[4..8]   offset → witness
//	[8..16]  chain_id (uint64 LE, inline — no longer a nested SszChainConfig)
//	[16..20] offset → public_keys
//	[20..]   variable section, in order: npr, witness, public_keys
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
	chainID := uint64(1)
	if f.StatelessInput.ChainConfig != nil && f.StatelessInput.ChainConfig.ChainID != 0 {
		chainID = f.StatelessInput.ChainConfig.ChainID
	}
	fork := activeProtocolFork(f.StatelessInput.ChainConfig, f.StatelessInput.Block.Header.Timestamp)

	return encodeStatelessInputContainer(npr, wit, chainID, schemaIDFor(fork)), nil
}

// encodeStatelessInputContainer emits the schema-id prefix and the
// SszStatelessInput container around already-encoded sections. Shared by the
// fixture and live paths so the two can never drift apart.
func encodeStatelessInputContainer(npr, wit []byte, chainID uint64, schemaID uint16) []byte {
	var pubKeys []byte // empty packed ByteVector[65] list

	// Fixed region: 4 (offset) + 4 (offset) + 8 (chain_id) + 4 (offset) = 20 bytes.
	const fixedSize = 20
	offNPR := uint32(fixedSize)
	offWitness := offNPR + uint32(len(npr))
	offPubKeys := offWitness + uint32(len(wit))

	var out bytes.Buffer
	// Schema-id prefix (big-endian uint16).
	var sid [2]byte
	binary.BigEndian.PutUint16(sid[:], schemaID)
	out.Write(sid[:])
	// Container body.
	writeU32LE(&out, offNPR)
	writeU32LE(&out, offWitness)
	binary.Write(&out, binary.LittleEndian, chainID)
	writeU32LE(&out, offPubKeys)
	out.Write(npr)
	out.Write(wit)
	out.Write(pubKeys)
	return out.Bytes()
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

// executionRequestTypes is the number of request lists in SszExecutionRequests.
// zkevm@v0.6.2 (EIP-8282) grew it from 3 (deposits, withdrawals, consolidations)
// to 5 by appending builder_deposits and builder_exits.
const executionRequestTypes = 5

// encodeSszExecutionRequests encodes an empty SszExecutionRequests container:
// fixed region = 4 bytes per variable field, every offset pointing just past it.
func encodeSszExecutionRequests() []byte {
	const fixedSize = 4 * executionRequestTypes
	var buf bytes.Buffer
	for i := 0; i < executionRequestTypes; i++ {
		writeU32LE(&buf, fixedSize)
	}
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
