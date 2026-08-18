package fixture

import (
	"bytes"
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ZesuInputSSZFromBlock encodes a live-pipeline block as zkvm-ready SSZ input.
// block is decoded from debug_getRawBlock RLP; state, codes, headers are
// already-decoded witness byte arrays from debug_executionWitness.
// balBytes is the RLP-encoded BlockAccessList (nil for pre-Amsterdam).
// chainID is inlined into SszStatelessInput (v0.8.0); 0 falls back to mainnet (1).
// fork is the ProtocolFork index the block must be executed under, stamped into
// the schema id; 0 falls back to Amsterdam.
func ZesuInputSSZFromBlock(block *types.Block, state, codes, headers [][]byte, balBytes []byte, chainID uint64, fork uint8) ([]byte, error) {
	var parentBeaconRoot common.Hash
	if r := block.BeaconRoot(); r != nil {
		parentBeaconRoot = *r
	}

	return encodeStatelessInputFromBlock(block, state, codes, headers, parentBeaconRoot, balBytes, chainID, fork)
}

func encodeStatelessInputFromBlock(block *types.Block, state, codes, headers [][]byte, parentBeaconRoot common.Hash, balBytes []byte, chainID uint64, fork uint8) ([]byte, error) {
	npr, err := encodeNewPayloadRequestFromBlock(block, parentBeaconRoot, balBytes)
	if err != nil {
		return nil, err
	}
	wit := encodeExecutionWitnessFromArrays(state, codes, headers)
	if chainID == 0 {
		chainID = 1 // mainnet
	}
	if fork == 0 {
		fork = forkAmsterdam
	}

	return encodeStatelessInputContainer(npr, wit, chainID, schemaIDFor(fork)), nil
}

func encodeNewPayloadRequestFromBlock(block *types.Block, parentBeaconRoot common.Hash, balBytes []byte) ([]byte, error) {
	ep, err := encodeExecutionPayloadFromBlock(block, balBytes)
	if err != nil {
		return nil, err
	}
	vh := encodeSszVersionedHashes(block.Transactions())
	er := encodeSszExecutionRequests()

	// Fixed: 4 (ep offset) + 4 (vh offset) + 32 (parent_beacon_block_root) + 4 (er offset) = 44
	const fixedSize = 44
	var fix bytes.Buffer
	writeU32LE(&fix, uint32(fixedSize))
	writeU32LE(&fix, uint32(fixedSize+len(ep)))
	fix.Write(parentBeaconRoot[:])
	writeU32LE(&fix, uint32(fixedSize+len(ep)+len(vh)))

	return append(fix.Bytes(), append(ep, append(vh, er...)...)...), nil
}

// encodeExecutionPayloadFromBlock serialises SszExecutionPayload from a decoded block.
// Fixed region: 540 bytes — layout mirrors encodeSszExecutionPayload in encode_ssz.go.
func encodeExecutionPayloadFromBlock(block *types.Block, balBytes []byte) ([]byte, error) {
	h := block.Header()
	extraData := h.Extra

	rawTxs := make([][]byte, block.Transactions().Len())
	for i, tx := range block.Transactions() {
		b, err := tx.MarshalBinary()
		if err != nil {
			return nil, err
		}
		rawTxs[i] = b
	}
	txsSSZ := encodeSszByteListList(rawTxs)
	wdsSSZ := encodeSszWithdrawalList(block.Withdrawals())

	baseFee := h.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	}

	const fixedSize = 540
	extraDataOff := uint32(fixedSize)
	txsOff := extraDataOff + uint32(len(extraData))
	wdsOff := txsOff + uint32(len(txsSSZ))
	balOff := wdsOff + uint32(len(wdsSSZ))

	var fix bytes.Buffer
	fix.Write(h.ParentHash[:])                                 // [0..32]
	fix.Write(h.Coinbase[:])                                   // [32..52]
	fix.Write(h.Root[:])                                       // [52..84]
	fix.Write(h.ReceiptHash[:])                                // [84..116]
	fix.Write(h.Bloom[:])                                      // [116..372]
	fix.Write(h.MixDigest[:])                                  // [372..404]
	binary.Write(&fix, binary.LittleEndian, h.Number.Uint64()) // [404..412]
	binary.Write(&fix, binary.LittleEndian, h.GasLimit)        // [412..420]
	binary.Write(&fix, binary.LittleEndian, h.GasUsed)         // [420..428]
	binary.Write(&fix, binary.LittleEndian, h.Time)            // [428..436]
	writeU32LE(&fix, extraDataOff)                             // [436..440]
	fix.Write(sszUint256(baseFee))                             // [440..472]
	fix.Write(make([]byte, 32))                                // [472..504] block_hash (zeros — unused for execution)
	writeU32LE(&fix, txsOff)                                   // [504..508]
	writeU32LE(&fix, wdsOff)                                   // [508..512]
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
	writeU32LE(&fix, balOff)                               // [528..532]
	slotNumber := uint64(0)
	if h.SlotNumber != nil {
		slotNumber = *h.SlotNumber
	}
	binary.Write(&fix, binary.LittleEndian, slotNumber) // [532..540]

	var out bytes.Buffer
	out.Write(fix.Bytes())
	out.Write(extraData)
	out.Write(txsSSZ)
	out.Write(wdsSSZ)
	out.Write(balBytes)
	return out.Bytes(), nil
}

// encodeExecutionWitnessFromArrays serialises SszExecutionWitness from pre-decoded arrays.
// Fixed region: 4+4+4 = 12 bytes (3 variable fields: state, codes, headers).
// Keys from debug_executionWitness are intentionally dropped — they have no
// corresponding field in SszExecutionWitness.
func encodeExecutionWitnessFromArrays(state, codes, headers [][]byte) []byte {
	stateSSZ := encodeSszByteListList(state)
	codesSSZ := encodeSszByteListList(codes)
	headersSSZ := encodeSszByteListList(headers)

	const fixedSize = 12
	var fix bytes.Buffer
	writeU32LE(&fix, uint32(fixedSize))
	writeU32LE(&fix, uint32(fixedSize+len(stateSSZ)))
	writeU32LE(&fix, uint32(fixedSize+len(stateSSZ)+len(codesSSZ)))

	return append(fix.Bytes(), append(stateSSZ, append(codesSSZ, headersSSZ...)...)...)
}
