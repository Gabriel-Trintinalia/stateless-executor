package fixture

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadZkevmFileEngine reads a blockchain_test_engine case, in the shape the
// witness generator emits: params[0] holds only blockNumber and gasUsed.
func TestLoadZkevmFileEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.json")
	content := `{"witness-generator-spec-cli::block_11856500_c2c4":{"network":"Amsterdam","config":{"chainid":"0xaa36a7"},` +
		`"engineNewPayloads":[{"params":[{"blockNumber":"0xb4ea74","gasUsed":"0x3b9c40f"}],` +
		`"statelessInputBytes":"0x1501","statelessOutputBytes":"0x10ed"}]}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tcs, err := LoadZkevmFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tcs) != 1 || len(tcs[0].Blocks) != 1 {
		t.Fatalf("got %d cases, want 1 case with 1 block", len(tcs))
	}
	b := tcs[0].Blocks[0]
	if b.Number() != 11856500 || b.GasUsed() != 62505999 {
		t.Errorf("number, gasUsed = %d, %d; want 11856500, 62505999", b.Number(), b.GasUsed())
	}
	if b.StatelessInputBytes != "0x1501" || b.StatelessOutputBytes != "0x10ed" {
		t.Errorf("stateless bytes = %q, %q", b.StatelessInputBytes, b.StatelessOutputBytes)
	}
	if tcs[0].Network != "Amsterdam" {
		t.Errorf("network = %q, want Amsterdam", tcs[0].Network)
	}
}
