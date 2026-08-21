package fixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSniffFormat(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Format
	}{
		{
			name:    "corpus",
			content: `{"name":"rpc_block_24758569","stateless_input":{"block":{}},"success":true}`,
			want:    FormatCorpus,
		},
		{
			name: "zkevm via genesisBlockHeader",
			content: `{"tests/benchmark/compute/instruction/test_stack.py::test_swap[fork_Amsterdam-blockchain_test-opcode_SWAP1-benchmark-gas-value_10M]":` +
				`{"network":"Amsterdam","genesisBlockHeader":{"parentHash":"0x00"},"blocks":[]}}`,
			want: FormatZkevm,
		},
		{
			name:    "zkevm via blocks only",
			content: `{"some::case":{"network":"Amsterdam","blocks":[{"statelessInputBytes":"0xaa"}]}}`,
			want:    FormatZkevm,
		},
		{
			name:    "meta index is not a fixture",
			content: `{"root_hash":"0x6496","created_at":"2026-08-19T11:03:54Z","test_count":335273,"forks":["Amsterdam"]}`,
			want:    FormatUnknown,
		},
		{
			name:    "garbage",
			content: "this is not json at all\n",
			want:    FormatUnknown,
		},
		{
			name:    "empty",
			content: "",
			want:    FormatUnknown,
		},
		{
			// The marker must be found even behind a very long leading key, and
			// must not be missed because the file is larger than the sniff window.
			name:    "marker behind padding, body far beyond the sniff limit",
			content: `{"` + strings.Repeat("x", 200) + `":{"network":"Amsterdam","genesisBlockHeader":{}},"pad":"` + strings.Repeat("y", 3*sniffLimit) + `"}`,
			want:    FormatZkevm,
		},
		{
			// A marker past the sniff window is deliberately NOT found; the file
			// is reported unknown rather than read in full.
			name:    "marker beyond the sniff limit is not found",
			content: `{"pad":"` + strings.Repeat("y", 2*sniffLimit) + `","stateless_input":{}}`,
			want:    FormatUnknown,
		},
	}

	dir := t.TempDir()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".json")
			if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := SniffFormat(p)
			if err != nil {
				t.Fatalf("SniffFormat: %v", err)
			}
			if got != tc.want {
				t.Errorf("SniffFormat = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSniffFormatMissingFile(t *testing.T) {
	if _, err := SniffFormat(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
