package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

// EEST labels contain commas, brackets, colons and quotes, so the CSV has to
// quote them — and reading the file back has to yield the original label.
func TestWriteCSVQuotesLabels(t *testing.T) {
	label := `tests/benchmark/test_x.py::test_y[fork_Amsterdam-blockchain_test-opcode_ADD,SUB-gas-value_60M]`
	rows := []BlockResult{
		{
			Kind: fixture.FormatZkevm, Label: label, Suite: "for_amsterdam_at_0060M/compute",
			BlockNum: 1, TxCount: 4, GasUsed: 60000000,
			Costs:   CostReport{Base: 1, Main: 2, Opcodes: 3, Precompiles: 4, Memory: 5, Total: 15, Steps: 99},
			Elapsed: 1500 * time.Millisecond,
		},
	}

	p := filepath.Join(t.TempDir(), "out.csv")
	if err := writeCSV(p, rows); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("the CSV does not round-trip: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if len(recs[0]) != len(csvHeader) {
		t.Errorf("header has %d columns, want %d", len(recs[0]), len(csvHeader))
	}
	// New columns are appended, so the first fifteen keep their positions.
	if recs[0][13] != "total" || recs[0][14] != "elapsed_ms" {
		t.Errorf("column 14/15 moved: %q, %q", recs[0][13], recs[0][14])
	}
	if got := recs[1][17]; got != label {
		t.Errorf("label did not round-trip:\n got %q\nwant %q", got, label)
	}
	if recs[1][15] != "99" {
		t.Errorf("steps = %q, want 99", recs[1][15])
	}
	if recs[1][16] != "for_amsterdam_at_0060M/compute" {
		t.Errorf("suite = %q", recs[1][16])
	}
}

// Corpus rows must stay byte-identical to the hand-rolled Fprintf that
// encoding/csv replaced — nothing in the first fifteen columns gets quoted.
func TestWriteCSVCorpusRowsUnquoted(t *testing.T) {
	rows := []BlockResult{{
		Kind: fixture.FormatCorpus, Label: "rpc_block_24758569", Suite: ".",
		BlockNum: 24758569, TxCount: 156, GasUsed: 25658526, LegacyTxs: 156,
		Costs:   CostReport{Base: 293601280, Total: 18108116470},
		Elapsed: 11813 * time.Millisecond,
	}}
	p := filepath.Join(t.TempDir(), "out.csv")
	if err := writeCSV(p, rows); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Split(string(b), "\n")[1]
	prefix := "24758569,156,25658526,156,0,0,0,0,293601280,0,0,0,0,18108116470,11813,"
	if !strings.HasPrefix(line, prefix) {
		t.Errorf("corpus row changed shape:\n got %q\nwant prefix %q", line, prefix)
	}
	if strings.Contains(line, `"`) {
		t.Errorf("corpus row was quoted: %q", line)
	}
}

func TestToJS(t *testing.T) {
	// Corpus keeps a numeric axis, byte-for-byte what the old concatenation
	// produced.
	if got := string(toJS([]uint64{24758569, 24758570})); got != "[24758569,24758570]" {
		t.Errorf("numeric labels = %s", got)
	}
	// A nil slice must not become "null" — the charts call array methods on it.
	if got := string(toJS([]uint64(nil))); got != "[]" {
		t.Errorf("nil slice = %s, want []", got)
	}
	// The characters that would break hand-built JS must be escaped.
	got := string(toJS([]string{`a"b`, `x[0]::y,z`}))
	if strings.Contains(got, `"a"b"`) {
		t.Errorf("quote not escaped: %s", got)
	}
	if !strings.Contains(got, `x[0]::y,z`) {
		t.Errorf("label mangled: %s", got)
	}
}

// Corpus rows sort by block number, since labels sort wrong across the
// 8-to-9-digit boundary.
func TestSortResultsCorpusByBlockNum(t *testing.T) {
	rs := []BlockResult{
		{Kind: fixture.FormatCorpus, BlockNum: 100000000, Label: "rpc_block_100000000"},
		{Kind: fixture.FormatCorpus, BlockNum: 99999999, Label: "rpc_block_99999999"},
	}
	sortResults(rs)
	if rs[0].BlockNum != 99999999 {
		t.Errorf("got %d first; label ordering would have put 100000000 first", rs[0].BlockNum)
	}
}

// EEST rows sort by (suite, label, block number): their block numbers are all
// 1 or 2 and cannot order anything.
func TestSortResultsEESTBySuiteThenLabel(t *testing.T) {
	rs := []BlockResult{
		{Kind: fixture.FormatZkevm, Suite: "b", Label: "a::x", BlockNum: 1},
		{Kind: fixture.FormatZkevm, Suite: "a", Label: "z::y", BlockNum: 2},
		{Kind: fixture.FormatZkevm, Suite: "a", Label: "a::y", BlockNum: 1},
	}
	sortResults(rs)
	want := []string{"a::y", "z::y", "a::x"}
	for i, w := range want {
		if rs[i].Label != w {
			t.Errorf("position %d: got %q, want %q", i, rs[i].Label, w)
		}
	}
}

func TestUnitCell(t *testing.T) {
	if got := unitCell(BlockResult{Kind: fixture.FormatCorpus, BlockNum: 24758569, Label: "rpc_block_24758569"}); got != "24758569" {
		t.Errorf("corpus unit cell = %q, want the bare block number", got)
	}
	if got := unitCell(BlockResult{Kind: fixture.FormatZkevm, BlockNum: 1, Label: "a::case"}); got != "a::case" {
		t.Errorf("EEST unit cell = %q, want the label", got)
	}
}

func TestSuiteMedians(t *testing.T) {
	good := []BlockResult{
		{Suite: "0060M", Costs: CostReport{Total: 30}},
		{Suite: "0010M", Costs: CostReport{Total: 10}},
		{Suite: "0060M", Costs: CostReport{Total: 50}},
		{Suite: "0010M", Costs: CostReport{Total: 20}},
	}
	names, meds := suiteMedians(good)
	if len(names) != 2 || names[0] != "0010M" || names[1] != "0060M" {
		t.Fatalf("suites = %v, want sorted [0010M 0060M]", names)
	}
	if meds[0] != 20 || meds[1] != 50 {
		t.Errorf("medians = %v", meds)
	}
}

// Above the per-unit chart limit the report must switch to aggregate charts and
// say so, rather than silently emitting a chart the browser cannot draw.
func TestWriteReportScaleGating(t *testing.T) {
	mk := func(n int) []BlockResult {
		rs := make([]BlockResult, n)
		for i := range rs {
			rs[i] = BlockResult{
				Kind: fixture.FormatZkevm, Suite: "s", Label: "t", BlockNum: 1,
				Costs: CostReport{Total: uint64(i + 1)}, ValidationOK: true,
				Verdict: verdict{Kind: verdictPass},
			}
		}
		return rs
	}

	for _, tc := range []struct {
		name       string
		n          int
		wantScaled bool
	}{
		{"under the limit", 10, false},
		{"over the limit", maxChartUnits + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "r.html")
			rs := mk(tc.n)
			if err := writeReport(p, rs, rs, "zisk"); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			html := string(b)
			hasRank := strings.Contains(html, "rankChart")
			hasStacked := strings.Contains(html, "stackedChart")
			if hasRank != tc.wantScaled {
				t.Errorf("rankChart present = %v, want %v", hasRank, tc.wantScaled)
			}
			if hasStacked == tc.wantScaled {
				t.Errorf("stackedChart present = %v, want %v", hasStacked, !tc.wantScaled)
			}
			if tc.wantScaled && !strings.Contains(html, "exceeds the") {
				t.Error("the chart swap must be stated in the report, not left implicit")
			}
		})
	}
}

// Skipped and unverified units get their own tables; they must not appear as
// errors.
func TestWriteReportSkipAndUnverifiedTables(t *testing.T) {
	rs := []BlockResult{
		{Kind: fixture.FormatZkevm, Label: "skipped::case", Verdict: verdict{Kind: verdictSkip, Reason: "no statelessInputBytes"}},
		{Kind: fixture.FormatZkevm, Label: "unverified::case", Verdict: verdict{Kind: verdictUnverified, Reason: "fixture has no statelessOutputBytes"}},
	}
	p := filepath.Join(t.TempDir(), "r.html")
	if err := writeReport(p, nil, rs, "zisk"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	html := string(b)

	for _, want := range []string{"skipTable", "skipped::case", "unverifiedTable", "unverified::case"} {
		if !strings.Contains(html, want) {
			t.Errorf("report is missing %q", want)
		}
	}
	// The stylesheet mentions errTable unconditionally, so assert on the table
	// element itself.
	if strings.Contains(html, `<table id="errTable">`) {
		t.Error("skips and unverified units must not render as errors")
	}
}
