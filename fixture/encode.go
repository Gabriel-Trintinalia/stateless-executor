package fixture

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

// ZesuInput encodes a fixture as a ziskemu-ready input file:
//
//	[u64 LE: payload_len] [RLP binary format]
//
// RLP binary format = zevm-stateless encoding (same layout as pipeline encodeRLP,
// including pre-recovered public keys so the guest can skip ECRECOVER).
//
// The guest reads input via zkvm_io.read_input_slice() which returns the bytes
// starting at offset 16 of the Zisk input region — i.e. exactly the payload.
// deserialize.fromBytes() then parses starting at byte 0 of that slice.
func ZesuInput(f *FixtureFile) ([]byte, error) {
	blockRLP, txs, err := blockRLPAndTxs(f.StatelessInput.Block)
	if err != nil {
		return nil, fmt.Errorf("block RLP: %w", err)
	}

	pubKeys, err := recoverPublicKeys(txs)
	if err != nil {
		return nil, fmt.Errorf("recover public keys: %w", err)
	}

	payload, err := encodeZesuRLP(blockRLP, f.StatelessInput.Witness, pubKeys)
	if err != nil {
		return nil, fmt.Errorf("encode RLP payload: %w", err)
	}

	// zesu-zkvm/src/main.zig expects input_data[0..32] = new_payload_request_root
	// (SSZ hash-tree-root of NewPayloadRequest, precomputed by host).
	// Prepend 32 zero bytes as a placeholder; the root does not affect execution cost.
	var root [32]byte
	payload = append(root[:], payload...)

	// ziskemu-0.16.1 requires the payload size to be a multiple of 8.
	for len(payload)%8 != 0 {
		payload = append(payload, 0)
	}

	// Ziskemu file framing: [u64 LE: len][bytes]
	var out bytes.Buffer
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(payload)))
	out.Write(lenBuf[:])
	out.Write(payload)
	return out.Bytes(), nil
}

// encodeZesuRLP serialises blockRLP + witness + pubKeys into the zevm-stateless
// big-endian length-prefixed binary format.
func encodeZesuRLP(blockRLP []byte, w WitnessData, pubKeys [][]byte) ([]byte, error) {
	var buf bytes.Buffer

	writeU64BE(&buf, uint64(len(blockRLP)))
	buf.Write(blockRLP)

	for _, arr := range [][]string{w.State, w.Codes, w.Keys} {
		decoded, err := decodeHexArray(arr)
		if err != nil {
			return nil, err
		}
		writeArray(&buf, decoded)
	}
	headers, err := decodeHexArray(w.Headers)
	if err != nil {
		return nil, fmt.Errorf("headers: %w", err)
	}
	writeArray(&buf, headers)

	// Pre-recovered public keys: 64 bytes each (uncompressed, no 0x04 prefix).
	// Sender = keccak256(pubkey)[12:], avoiding ECRECOVER in the guest.
	writeArray(&buf, pubKeys)

	return buf.Bytes(), nil
}

// BlockRLP re-encodes a fixture block as Ethereum block RLP:
// RLP([header, txs_list, ommers_list, withdrawals_list])
func BlockRLP(b FixtureBlock) ([]byte, error) {
	data, _, err := blockRLPAndTxs(b)
	return data, err
}

// blockRLPAndTxs returns the block RLP together with the decoded transaction
// objects so callers can recover sender public keys without re-parsing.
func blockRLPAndTxs(b FixtureBlock) ([]byte, types.Transactions, error) {
	header, err := buildHeader(b.Header)
	if err != nil {
		return nil, nil, fmt.Errorf("header: %w", err)
	}
	txs, err := buildTransactions(b.Body.Transactions)
	if err != nil {
		return nil, nil, fmt.Errorf("transactions: %w", err)
	}
	withdrawals, err := buildWithdrawals(b.Body.Withdrawals)
	if err != nil {
		return nil, nil, fmt.Errorf("withdrawals: %w", err)
	}

	type extblock struct {
		Header      *types.Header
		Txs         types.Transactions
		Uncles      []*types.Header
		Withdrawals []*types.Withdrawal `rlp:"optional"`
	}
	data, err := rlp.EncodeToBytes(&extblock{
		Header:      header,
		Txs:         txs,
		Uncles:      nil,
		Withdrawals: withdrawals,
	})
	return data, txs, err
}

// recoverPublicKeys recovers the 64-byte uncompressed secp256k1 public key
// (no 0x04 prefix) for every transaction in the block. The chain ID is inferred
// from the first typed transaction; legacy-only blocks default to mainnet (1).
func recoverPublicKeys(txs types.Transactions) ([][]byte, error) {
	if len(txs) == 0 {
		return nil, nil
	}
	chainID := big.NewInt(1)
	for _, tx := range txs {
		if cid := tx.ChainId(); cid != nil && cid.Sign() > 0 {
			chainID = cid
			break
		}
	}
	signer := types.LatestSignerForChainID(chainID)
	keys := make([][]byte, len(txs))
	for i, tx := range txs {
		pub, err := recoverTxPublicKey(signer, tx)
		if err != nil {
			return nil, fmt.Errorf("tx[%d]: %w", i, err)
		}
		keys[i] = pub
	}
	return keys, nil
}

// recoverTxPublicKey returns the 64-byte uncompressed public key for tx.
func recoverTxPublicKey(signer types.Signer, tx *types.Transaction) ([]byte, error) {
	v, r, s := tx.RawSignatureValues()

	sig := make([]byte, 65)
	r.FillBytes(sig[0:32])
	s.FillBytes(sig[32:64])

	var hash common.Hash
	switch tx.Type() {
	case types.LegacyTxType:
		// V < 35 means pre-EIP-155 (V = 27 + yParity).
		// V >= 35 means EIP-155 (V = chainID*2 + 35 + yParity).
		// We derive chain ID directly from V rather than using the block-level
		// signer, which would fail for pre-155 txs in a block that also has typed txs.
		if v.Cmp(big.NewInt(35)) < 0 {
			hash = types.HomesteadSigner{}.Hash(tx)
			sig[64] = byte(new(big.Int).Sub(v, big.NewInt(27)).Uint64())
		} else {
			txChainID := new(big.Int).Rsh(new(big.Int).Sub(v, big.NewInt(35)), 1)
			hash = types.NewEIP155Signer(txChainID).Hash(tx)
			yp := new(big.Int).Sub(v, new(big.Int).Add(
				new(big.Int).Mul(txChainID, big.NewInt(2)),
				big.NewInt(35),
			))
			sig[64] = byte(yp.Uint64())
		}
	default:
		hash = signer.Hash(tx)
		sig[64] = byte(v.Uint64())
	}

	full, err := crypto.Ecrecover(hash.Bytes(), sig)
	if err != nil {
		return nil, err
	}
	return full[1:], nil // strip 0x04 prefix → 64 bytes
}

// buildHeader converts the fixture snake_case header to a go-ethereum Header.
func buildHeader(h FixtureHeader) (*types.Header, error) {
	difficulty, err := hexToBigInt(h.Difficulty)
	if err != nil {
		return nil, fmt.Errorf("difficulty: %w", err)
	}
	nonce, err := hexToBlockNonce(h.Nonce)
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	baseFee, err := rawJSONToBigInt(h.BaseFeePerGas)
	if err != nil {
		return nil, fmt.Errorf("base_fee_per_gas: %w", err)
	}

	hdr := &types.Header{
		ParentHash:  hexToHash(h.ParentHash),
		UncleHash:   hexToHash(h.OmmersHash),
		Coinbase:    hexToAddress(h.Beneficiary),
		Root:        hexToHash(h.StateRoot),
		TxHash:      hexToHash(h.TransactionsRoot),
		ReceiptHash: hexToHash(h.ReceiptsRoot),
		Bloom:       hexToBloom(h.LogsBloom),
		Difficulty:  difficulty,
		Number:      new(big.Int).SetUint64(h.Number),
		GasLimit:    h.GasLimit,
		GasUsed:     h.GasUsed,
		Time:        h.Timestamp,
		Extra:       mustHexToBytes(h.ExtraData),
		MixDigest:   hexToHash(h.MixHash),
		Nonce:       nonce,
		BaseFee:     baseFee,
	}
	if h.WithdrawalsRoot != nil {
		wh := hexToHash(*h.WithdrawalsRoot)
		hdr.WithdrawalsHash = &wh
	}
	if h.BlobGasUsed != nil {
		v := *h.BlobGasUsed
		hdr.BlobGasUsed = &v
	}
	if h.ExcessBlobGas != nil {
		v := *h.ExcessBlobGas
		hdr.ExcessBlobGas = &v
	}
	if h.ParentBeaconBlockRoot != nil {
		pbr := hexToHash(*h.ParentBeaconBlockRoot)
		hdr.ParentBeaconRoot = &pbr
	}
	if h.RequestsHash != nil {
		rh := hexToHash(*h.RequestsHash)
		hdr.RequestsHash = &rh
	}
	return hdr, nil
}

func buildTransactions(txs []FixtureTx) (types.Transactions, error) {
	out := make(types.Transactions, 0, len(txs))
	for i, ft := range txs {
		tx, err := buildTx(ft)
		if err != nil {
			return nil, fmt.Errorf("tx[%d]: %w", i, err)
		}
		out = append(out, tx)
	}
	return out, nil
}

func buildTx(ft FixtureTx) (*types.Transaction, error) {
	if len(ft.Transaction) != 1 {
		return nil, fmt.Errorf("expected exactly one tx type key, got %d", len(ft.Transaction))
	}
	for txType, raw := range ft.Transaction {
		switch txType {
		case "Eip1559":
			return buildEip1559(raw, ft.Signature)
		case "Legacy":
			return buildLegacy(raw, ft.Signature)
		case "Eip2930":
			return buildEip2930(raw, ft.Signature)
		case "Eip4844":
			return buildEip4844(raw, ft.Signature)
		case "Eip7702":
			return buildEip7702(raw, ft.Signature)
		default:
			return nil, fmt.Errorf("unknown tx type %q", txType)
		}
	}
	return nil, fmt.Errorf("no tx type key")
}

func buildEip1559(raw json.RawMessage, sig FixtureSig) (*types.Transaction, error) {
	var b Eip1559TxBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	al, err := decodeAccessList(b.AccessList)
	if err != nil {
		return nil, err
	}
	yParity, r, s, err := parseSig(sig)
	if err != nil {
		return nil, err
	}
	return types.NewTx(&types.DynamicFeeTx{
		ChainID:    new(big.Int).SetUint64(b.ChainID),
		Nonce:      b.Nonce,
		GasTipCap:  new(big.Int).SetUint64(b.MaxPriorityFeePerGas),
		GasFeeCap:  new(big.Int).SetUint64(b.MaxFeePerGas),
		Gas:        b.GasLimit,
		To:         parseToAddrPtr(b.To),
		Value:      hexToBigInt2(b.Value),
		Data:       mustHexToBytes(b.Input),
		AccessList: al,
		V:          new(big.Int).SetUint64(yParity),
		R:          r,
		S:          s,
	}), nil
}

func buildLegacy(raw json.RawMessage, sig FixtureSig) (*types.Transaction, error) {
	var b LegacyTxBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	chainID, err := rawJSONToUint64(b.ChainID)
	if err != nil {
		return nil, fmt.Errorf("legacy chain_id: %w", err)
	}
	yParity, r, s, err := parseSig(sig)
	if err != nil {
		return nil, err
	}
	// EIP-155: v = chainID*2 + 35 + yParity; pre-155: v = 27 + yParity
	var v *big.Int
	if chainID > 0 {
		v = new(big.Int).SetUint64(chainID*2 + 35 + yParity)
	} else {
		v = new(big.Int).SetUint64(27 + yParity)
	}
	return types.NewTx(&types.LegacyTx{
		Nonce:    b.Nonce,
		GasPrice: new(big.Int).SetUint64(b.GasPrice),
		Gas:      b.GasLimit,
		To:       parseToAddrPtr(b.To),
		Value:    hexToBigInt2(b.Value),
		Data:     mustHexToBytes(b.Input),
		V:        v,
		R:        r,
		S:        s,
	}), nil
}

func buildEip2930(raw json.RawMessage, sig FixtureSig) (*types.Transaction, error) {
	var b Eip2930TxBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	al, err := decodeAccessList(b.AccessList)
	if err != nil {
		return nil, err
	}
	yParity, r, s, err := parseSig(sig)
	if err != nil {
		return nil, err
	}
	return types.NewTx(&types.AccessListTx{
		ChainID:    new(big.Int).SetUint64(b.ChainID),
		Nonce:      b.Nonce,
		GasPrice:   new(big.Int).SetUint64(b.GasPrice),
		Gas:        b.GasLimit,
		To:         parseToAddrPtr(b.To),
		Value:      hexToBigInt2(b.Value),
		Data:       mustHexToBytes(b.Input),
		AccessList: al,
		V:          new(big.Int).SetUint64(yParity),
		R:          r,
		S:          s,
	}), nil
}

func buildEip4844(raw json.RawMessage, sig FixtureSig) (*types.Transaction, error) {
	var b Eip4844TxBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	al, err := decodeAccessList(b.AccessList)
	if err != nil {
		return nil, err
	}
	yParity, _, _, err := parseSig(sig)
	if err != nil {
		return nil, err
	}
	chainID, err := hexToUint256(b.ChainID)
	if err != nil {
		return nil, fmt.Errorf("EIP-4844 chainId: %w", err)
	}
	nonce, err := hexToUint64(b.Nonce)
	if err != nil {
		return nil, err
	}
	gas, err := hexToUint64(b.Gas)
	if err != nil {
		return nil, err
	}
	maxFeePerGas, err := hexToUint256(b.MaxFeePerGas)
	if err != nil {
		return nil, err
	}
	maxPrioFeePerGas, err := hexToUint256(b.MaxPriorityFeePerGas)
	if err != nil {
		return nil, err
	}
	value, err := hexToUint256(b.Value)
	if err != nil {
		return nil, err
	}
	blobFeeCap, err := hexToUint256(b.MaxFeePerBlobGas)
	if err != nil {
		return nil, err
	}
	blobHashes := make([]common.Hash, len(b.BlobVersionedHashes))
	for i, h := range b.BlobVersionedHashes {
		blobHashes[i] = hexToHash(h)
	}
	rU, err := hexToUint256(sig.R)
	if err != nil {
		return nil, err
	}
	sU, err := hexToUint256(sig.S)
	if err != nil {
		return nil, err
	}
	vU := uint256.NewInt(yParity)

	return types.NewTx(&types.BlobTx{
		ChainID:    chainID,
		Nonce:      nonce,
		GasTipCap:  maxPrioFeePerGas,
		GasFeeCap:  maxFeePerGas,
		Gas:        gas,
		To:         common.HexToAddress(b.To),
		Value:      value,
		Data:       mustHexToBytes(b.Input),
		AccessList: al,
		BlobFeeCap: blobFeeCap,
		BlobHashes: blobHashes,
		V:          vU,
		R:          rU,
		S:          sU,
	}), nil
}

func buildEip7702(raw json.RawMessage, sig FixtureSig) (*types.Transaction, error) {
	var b Eip7702TxBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	al, err := decodeAccessList(b.AccessList)
	if err != nil {
		return nil, err
	}
	auths, err := buildAuthList(b.AuthorizationList)
	if err != nil {
		return nil, err
	}
	yParity, _, _, err := parseSig(sig)
	if err != nil {
		return nil, err
	}
	rU, err := hexToUint256(sig.R)
	if err != nil {
		return nil, err
	}
	sU, err := hexToUint256(sig.S)
	if err != nil {
		return nil, err
	}
	return types.NewTx(&types.SetCodeTx{
		ChainID:    uint256.NewInt(b.ChainID),
		Nonce:      b.Nonce,
		GasTipCap:  uint256.NewInt(b.MaxPriorityFeePerGas),
		GasFeeCap:  uint256.NewInt(b.MaxFeePerGas),
		Gas:        b.GasLimit,
		To:         common.HexToAddress(b.To),
		Value:      bigToUint256(hexToBigInt2(b.Value)),
		Data:       mustHexToBytes(b.Input),
		AccessList: al,
		AuthList:   auths,
		V:          uint256.NewInt(yParity),
		R:          rU,
		S:          sU,
	}), nil
}

func buildAuthList(auths []FixtureAuthorization) ([]types.SetCodeAuthorization, error) {
	out := make([]types.SetCodeAuthorization, len(auths))
	for i, a := range auths {
		chainID, err := hexToUint64(a.Inner.ChainID)
		if err != nil {
			return nil, fmt.Errorf("auth[%d] chainId: %w", i, err)
		}
		nonce, err := hexToUint64(a.Inner.Nonce)
		if err != nil {
			return nil, fmt.Errorf("auth[%d] nonce: %w", i, err)
		}
		yParity, err := hexToUint64(a.YParity)
		if err != nil {
			return nil, err
		}
		rU, err := hexToUint256(a.R)
		if err != nil {
			return nil, err
		}
		sU, err := hexToUint256(a.S)
		if err != nil {
			return nil, err
		}
		out[i] = types.SetCodeAuthorization{
			ChainID: *uint256.NewInt(chainID),
			Address: common.HexToAddress(a.Inner.Address),
			Nonce:   nonce,
			V:       uint8(yParity),
			R:       *rU,
			S:       *sU,
		}
	}
	return out, nil
}

func buildWithdrawals(wds []FixtureWithdrawal) ([]*types.Withdrawal, error) {
	out := make([]*types.Withdrawal, len(wds))
	for i, w := range wds {
		idx, err := hexToUint64(w.Index)
		if err != nil {
			return nil, fmt.Errorf("withdrawal[%d] index: %w", i, err)
		}
		vi, err := hexToUint64(w.ValidatorIndex)
		if err != nil {
			return nil, fmt.Errorf("withdrawal[%d] validatorIndex: %w", i, err)
		}
		amount, err := hexToUint64(w.Amount)
		if err != nil {
			return nil, fmt.Errorf("withdrawal[%d] amount: %w", i, err)
		}
		out[i] = &types.Withdrawal{
			Index:     idx,
			Validator: vi,
			Address:   common.HexToAddress(w.Address),
			Amount:    amount,
		}
	}
	return out, nil
}

// --- Access list ---

func decodeAccessList(raw json.RawMessage) (types.AccessList, error) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "[]" {
		return nil, nil
	}
	var tuples []AccessTupleJSON
	if err := json.Unmarshal(raw, &tuples); err != nil {
		return nil, fmt.Errorf("access_list: %w", err)
	}
	al := make(types.AccessList, len(tuples))
	for i, t := range tuples {
		keys := make([]common.Hash, len(t.StorageKeys))
		for j, k := range t.StorageKeys {
			keys[j] = hexToHash(k)
		}
		al[i] = types.AccessTuple{
			Address:     common.HexToAddress(t.Address),
			StorageKeys: keys,
		}
	}
	return al, nil
}

// --- Binary encoding helpers ---

func writeU64BE(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}

func writeArray(buf *bytes.Buffer, items [][]byte) {
	writeU64BE(buf, uint64(len(items)))
	for _, item := range items {
		writeU64BE(buf, uint64(len(item)))
		buf.Write(item)
	}
}

// --- Type conversion helpers ---

func hexToHash(s string) common.Hash          { return common.HexToHash(s) }
func hexToAddress(s string) common.Address    { return common.HexToAddress(s) }

func hexToBloom(s string) types.Bloom {
	b := mustHexToBytes(s)
	var bloom types.Bloom
	copy(bloom[:], b)
	return bloom
}

func hexToBlockNonce(s string) (types.BlockNonce, error) {
	b, err := hexStrToBytes(s)
	if err != nil {
		return types.BlockNonce{}, err
	}
	var nonce types.BlockNonce
	if len(b) > 8 {
		return types.BlockNonce{}, fmt.Errorf("nonce too long: %d bytes", len(b))
	}
	copy(nonce[8-len(b):], b)
	return nonce, nil
}

func hexToBigInt(s string) (*big.Int, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" || s == "0" {
		return new(big.Int), nil
	}
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return nil, fmt.Errorf("invalid hex big.Int: %q", s)
	}
	return n, nil
}

func hexToBigInt2(s string) *big.Int {
	n, _ := hexToBigInt(s)
	if n == nil {
		return new(big.Int)
	}
	return n
}

func hexToUint64(s string) (uint64, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return 0, nil
	}
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return 0, fmt.Errorf("invalid hex uint64: %q", s)
	}
	return n.Uint64(), nil
}

func hexToUint256(s string) (*uint256.Int, error) {
	n, err := hexToBigInt(s)
	if err != nil {
		return nil, err
	}
	return bigToUint256(n), nil
}

func bigToUint256(n *big.Int) *uint256.Int {
	if n == nil {
		return uint256.NewInt(0)
	}
	u, _ := uint256.FromBig(n)
	if u == nil {
		return uint256.NewInt(0)
	}
	return u
}

func rawJSONToBigInt(raw json.RawMessage) (*big.Int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return new(big.Int), nil
	}
	var n uint64
	if err := json.Unmarshal(raw, &n); err == nil {
		return new(big.Int).SetUint64(n), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("rawJSONToBigInt: %w", err)
	}
	return hexToBigInt(s)
}

func rawJSONToUint64(raw json.RawMessage) (uint64, error) {
	n, err := rawJSONToBigInt(raw)
	if err != nil {
		return 0, err
	}
	return n.Uint64(), nil
}

func parseSig(sig FixtureSig) (yParity uint64, r, s *big.Int, err error) {
	yParity, err = hexToUint64(sig.YParity)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("yParity: %w", err)
	}
	r = hexToBigInt2(sig.R)
	s = hexToBigInt2(sig.S)
	return
}

func mustHexToBytes(s string) []byte {
	b, _ := hexStrToBytes(s)
	return b
}

func hexStrToBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

func decodeHexArray(arr []string) ([][]byte, error) {
	out := make([][]byte, len(arr))
	for i, s := range arr {
		b, err := hexStrToBytes(s)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		out[i] = b
	}
	return out, nil
}

func parseToAddrPtr(s *string) *common.Address {
	if s == nil || *s == "" || *s == "null" {
		return nil
	}
	a := common.HexToAddress(*s)
	return &a
}
