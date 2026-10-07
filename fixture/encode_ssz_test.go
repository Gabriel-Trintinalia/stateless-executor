package fixture

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// referenceInputPrefix is the first 18 bytes of statelessInputBytes from a
// tests-zkevm@v21.0.5 fixture (blockchain_tests/for_amsterdam/shanghai/
// eip3860_initcode/initcode/contract_creating_tx.json), i.e. bytes produced by
// the reference serializer rather than by us:
//
//	1501      schema_id (big-endian) = Amsterdam(0x15) | revision(0x01)
//	10000000  offset → new_payload_request = 16 (the fixed-region size)
//	1e040200  offset → witness
//	0100000000000000  chain_id = 1, inline
var referenceInputPrefix = []byte{
	0x15, 0x01,
	0x10, 0x00, 0x00, 0x00,
	0x1e, 0x04, 0x02, 0x00,
	0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

// referenceInputLen is the length of that fixture's statelessInputBytes.
const referenceInputLen = 136253

// TestContainerMatchesReferenceLayout pins the outer container against the
// reference bytes: schema id, the 16-byte fixed region (v21.0.1 dropped the
// public_keys offset), and chain_id inline.
func TestContainerMatchesReferenceLayout(t *testing.T) {
	if got := binary.BigEndian.Uint16(referenceInputPrefix[:2]); got != statelessInputSchemaID {
		t.Fatalf("schema id = 0x%04x, reference fixture has 0x%04x", statelessInputSchemaID, got)
	}

	refNPROff := binary.LittleEndian.Uint32(referenceInputPrefix[2:6])
	refWitOff := binary.LittleEndian.Uint32(referenceInputPrefix[6:10])
	refChainID := binary.LittleEndian.Uint64(referenceInputPrefix[10:18])

	// Rebuild the same header from sections sized to match the reference.
	npr := make([]byte, refWitOff-refNPROff)
	wit := make([]byte, referenceInputLen-2-int(refWitOff))
	got := encodeStatelessInputContainer(npr, wit, refChainID, statelessInputSchemaID)

	if len(got) < len(referenceInputPrefix) {
		t.Fatalf("encoded %d bytes, want at least %d", len(got), len(referenceInputPrefix))
	}
	if !bytes.Equal(got[:len(referenceInputPrefix)], referenceInputPrefix) {
		t.Errorf("container header mismatch\ngot:  %x\nwant: %x", got[:len(referenceInputPrefix)], referenceInputPrefix)
	}
	if len(got) != referenceInputLen {
		t.Errorf("total length = %d, want %d (schema id + fixed region + sections)", len(got), referenceInputLen)
	}
}

// TestExecutionRequestsMatchesReference pins the empty SszExecutionRequests
// container against the reference bytes from the same v0.8.0 fixture: five
// variable fields (EIP-8282 added builder_deposits and builder_exits to the
// original deposits/withdrawals/consolidations), so five offsets of 20.
func TestExecutionRequestsMatchesReference(t *testing.T) {
	want := []byte{
		0x14, 0x00, 0x00, 0x00,
		0x14, 0x00, 0x00, 0x00,
		0x14, 0x00, 0x00, 0x00,
		0x14, 0x00, 0x00, 0x00,
		0x14, 0x00, 0x00, 0x00,
	}
	if got := encodeSszExecutionRequests(); !bytes.Equal(got, want) {
		t.Errorf("execution requests mismatch\ngot:  %x\nwant: %x", got, want)
	}
}

// TestContainerSectionsAreContiguous checks the offsets actually delimit the
// sections, for section sizes unrelated to the reference fixture.
func TestContainerSectionsAreContiguous(t *testing.T) {
	npr := bytes.Repeat([]byte{0xaa}, 100)
	wit := bytes.Repeat([]byte{0xbb}, 37)
	got := encodeStatelessInputContainer(npr, wit, 7014190335, schemaIDFor(forkBPO2))

	if id := binary.BigEndian.Uint16(got[:2]); id != 0x1401 {
		t.Errorf("schema id = 0x%04x, want 0x1401 (ProtocolFork.BPO2 << 8 | revision 1)", id)
	}

	body := got[2:]
	offNPR := binary.LittleEndian.Uint32(body[0:4])
	offWit := binary.LittleEndian.Uint32(body[4:8])
	chainID := binary.LittleEndian.Uint64(body[8:16])

	if offNPR != 16 {
		t.Errorf("new_payload_request offset = %d, want 16", offNPR)
	}
	if chainID != 7014190335 {
		t.Errorf("chain_id = %d, want 7014190335", chainID)
	}
	if !bytes.Equal(body[offNPR:offWit], npr) {
		t.Error("new_payload_request section does not match its offsets")
	}
	if !bytes.Equal(body[offWit:], wit) {
		t.Error("witness section does not match its offsets")
	}
}
