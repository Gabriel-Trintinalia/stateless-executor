package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

// A well-formed 43-byte SszStatelessValidationResult in hex:
// root(32) ‖ valid(1) ‖ chain_id(8) ‖ schema_id(2).
func outputHex(valid bool) string {
	b := strings.Repeat("aa", 32)
	if valid {
		b += "01"
	} else {
		b += "00"
	}
	return b + "0100000000000000" + "0115"
}

func TestVerifyEEST(t *testing.T) {
	pass := outputHex(true)
	fail := outputHex(false)

	tests := []struct {
		name     string
		expected string
		got      string
		execErr  string
		runErr   error
		want     verdictKind
	}{
		{
			name: "matching valid output", expected: pass, got: pass, want: verdictPass,
		},
		{
			// The case the corpus rule would get wrong: the guest prints
			// "execution failed", exits 0, and matches its expected output.
			// execErr must stay display-only or this is a false failure.
			name:     "expected-invalid block whose output matches, with execErr",
			expected: fail, got: fail, execErr: "InvalidBlock", want: verdictPass,
		},
		{
			name: "output mismatch", expected: pass, got: fail, want: verdictFail,
		},
		{
			// ziskemu's -o writes the whole zero-padded output region, so got is
			// trimmed to expected's length before comparing.
			name: "got longer than expected is truncated", expected: pass,
			got: pass + strings.Repeat("00", 400), want: verdictPass,
		},
		{
			name: "no expected output cannot be verified", expected: "", got: pass, want: verdictUnverified,
		},
		{
			name: "missing statelessInputBytes is a skip", expected: pass,
			runErr: fmt.Errorf("wrapped: %w", fixture.ErrMissingStatelessInputBytes), want: verdictSkip,
		},
		{
			name: "run error", expected: pass, got: pass,
			runErr: errors.New("zkvm: exit status 1"), want: verdictError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc := encoded{
				ExpectedOutputHex: tc.expected,
				ExpectedSuccess:   expectedSuccessFromOutput(tc.expected),
			}
			got := verifyEEST(enc, emuResult{OutputHex: tc.got, ExecErr: tc.execErr}, tc.runErr)
			if got.Kind != tc.want {
				t.Errorf("Kind = %v, want %v (reason %q)", got.Kind, tc.want, got.Reason)
			}
			if got.Mode != "output-bytes" {
				t.Errorf("Mode = %q, want output-bytes", got.Mode)
			}
		})
	}
}

// The most likely regression: reintroducing expectException as the pass/fail
// signal. A block carrying an expectException whose output nonetheless matches
// must PASS — the stateless validation is what is being checked, and it agreed
// with the fixture.
func TestVerifyEESTIgnoresExpectException(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "case.json")
	out := outputHex(false)
	content := map[string]any{
		"a::case": map[string]any{
			"network":            "Amsterdam",
			"genesisBlockHeader": map[string]any{},
			"blocks": []any{map[string]any{
				"blockHeader":          map[string]any{"number": "0x01", "gasUsed": "0x039386aa"},
				"transactions":         []any{},
				"statelessInputBytes":  "0x" + strings.Repeat("11", 64),
				"statelessOutputBytes": "0x" + out,
				"expectException":      "INVALID_BLOCK_ACCESS_LIST",
			}},
		},
	}
	b, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}

	units, err := expandEEST(fileJob{Path: p, Kind: fixture.FormatZkevm, Suite: "s"})
	if err != nil {
		t.Fatalf("expandEEST: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}

	enc, err := units[0].Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// The expectation must come from output byte 32, not from expectException.
	if enc.ExpectedSuccess {
		t.Error("ExpectedSuccess should be false: output byte 32 is 0x00")
	}

	v := units[0].Verify(enc, emuResult{OutputHex: out, ExecErr: "InvalidBlock"}, nil)
	if v.Kind != verdictPass {
		t.Errorf("Kind = %v, want pass: expectException must not decide the verdict (reason %q)", v.Kind, v.Reason)
	}
}

func TestExpandEESTMetadataAndLabels(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "multi.json")

	block := func(num, gas string, types []string) map[string]any {
		txs := make([]any, 0, len(types))
		for _, ty := range types {
			txs = append(txs, map[string]any{"type": ty})
		}
		return map[string]any{
			"blockHeader":          map[string]any{"number": num, "gasUsed": gas},
			"transactions":         txs,
			"statelessInputBytes":  "0x" + strings.Repeat("22", 32),
			"statelessOutputBytes": "0x" + outputHex(true),
		}
	}

	content := map[string]any{
		// Deliberately out of lexical order: b before a in the map literal.
		"z::second": map[string]any{
			"network":            "Amsterdam",
			"genesisBlockHeader": map[string]any{},
			"blocks":             []any{block("0x01", "0x989680", []string{"0x00"})},
		},
		"a::first": map[string]any{
			"network":            "Amsterdam",
			"genesisBlockHeader": map[string]any{},
			"blocks": []any{
				block("0x01", "0x039386aa", []string{"0x00", "0x01", "0x02", "0x03", "0x04"}),
				block("0x02", "0x039386aa", []string{"0x01"}),
			},
		},
	}
	b, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}

	units, err := expandEEST(fileJob{Path: p, Kind: fixture.FormatZkevm, Suite: "for_amsterdam_at_0060M/compute"})
	if err != nil {
		t.Fatalf("expandEEST: %v", err)
	}
	if len(units) != 3 {
		t.Fatalf("got %d units, want 3", len(units))
	}

	// Test cases must come out sorted by name: the top-level keys land in a Go
	// map, whose iteration order is randomised per run.
	wantLabels := []string{"a::first/block0", "a::first/block1", "z::second"}
	for i, want := range wantLabels {
		if units[i].Meta.Label != want {
			t.Errorf("unit %d: Label = %q, want %q", i, units[i].Meta.Label, want)
		}
	}

	// A single-block case takes the bare test name; only multi-block cases get
	// the /blockN suffix. This has to match cmd/zkevm-runner exactly.
	if strings.Contains(units[2].Meta.Label, "/block") {
		t.Errorf("single-block case should not carry a /blockN suffix: %q", units[2].Meta.Label)
	}

	first := units[0].Meta
	if first.BlockNum != 1 || units[1].Meta.BlockNum != 2 {
		t.Errorf("BlockNum from blockHeader.number = %d, %d; want 1, 2", first.BlockNum, units[1].Meta.BlockNum)
	}
	// 0x03938700 is exactly 60,000,000, so 0x039386aa is 86 short of it.
	if first.Info.GasUsed != 59999914 {
		t.Errorf("GasUsed = %d, want 59999914 (0x039386aa)", first.Info.GasUsed)
	}
	if first.Info.TxCount != 5 {
		t.Errorf("TxCount = %d, want 5", first.Info.TxCount)
	}
	if first.Network != "Amsterdam" {
		t.Errorf("Network = %q, want Amsterdam", first.Network)
	}
	if first.Suite != "for_amsterdam_at_0060M/compute" {
		t.Errorf("Suite = %q", first.Suite)
	}
	// One tx of each type: 0x00 legacy, 0x01 2930, 0x02 1559, 0x03 4844, 0x04 7702.
	if first.Info.LegacyTxs != 1 || first.Info.Eip2930Txs != 1 || first.Info.Eip1559Txs != 1 ||
		first.Info.Eip4844Txs != 1 || first.Info.Eip7702Txs != 1 {
		t.Errorf("per-type counts wrong: %+v", first.Info)
	}
}

// A block with no statelessInputBytes must skip, not fail: a mixed-fork tree
// legitimately contains blocks this tool cannot run.
func TestExpandEESTMissingInputSkips(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "noinput.json")
	content := map[string]any{
		"a::case": map[string]any{
			"network":            "Amsterdam",
			"genesisBlockHeader": map[string]any{},
			"blocks": []any{map[string]any{
				"blockHeader":         map[string]any{"number": "0x01", "gasUsed": "0x01"},
				"statelessInputBytes": "",
			}},
		},
	}
	b, _ := json.Marshal(content)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}

	units, err := expandEEST(fileJob{Path: p, Kind: fixture.FormatZkevm})
	if err != nil {
		t.Fatalf("expandEEST: %v", err)
	}
	enc, encErr := units[0].Encode()
	if !errors.Is(encErr, fixture.ErrMissingStatelessInputBytes) {
		t.Fatalf("Encode error = %v, want ErrMissingStatelessInputBytes", encErr)
	}
	if v := units[0].Verify(enc, emuResult{}, encErr); v.Kind != verdictSkip {
		t.Errorf("Kind = %v, want skip", v.Kind)
	}
}

func TestExpectedSuccessFromOutput(t *testing.T) {
	if !expectedSuccessFromOutput(outputHex(true)) {
		t.Error("valid=0x01 should be a success expectation")
	}
	if expectedSuccessFromOutput(outputHex(false)) {
		t.Error("valid=0x00 should be a failure expectation")
	}
	// Too short to carry byte 32: treated as a success expectation, matching
	// cmd/zkevm-runner.
	if !expectedSuccessFromOutput("aabb") {
		t.Error("a short output should default to a success expectation")
	}
}
