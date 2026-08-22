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
			// Assert on chart construction, not on the identifier: the
			// zero-gas toggle's refresh helper mentions every chart by name.
			hasRank := strings.Contains(html, `getElementById('rankChart')`)
			hasStacked := strings.Contains(html, `getElementById('stackedChart')`)
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

// The zero-gas toggle only appears when the run actually contains such units,
// so corpus reports are unaffected.
func TestWriteReportZeroGasToggleOnlyWhenRelevant(t *testing.T) {
	mk := func(gas ...uint64) []BlockResult {
		rs := make([]BlockResult, len(gas))
		for i, g := range gas {
			rs[i] = BlockResult{
				Kind: fixture.FormatZkevm, Suite: "s", Label: "t", GasUsed: g,
				Costs: CostReport{Total: 100 + uint64(i)}, ValidationOK: true,
				Verdict: verdict{Kind: verdictPass},
			}
		}
		return rs
	}

	t.Run("no zero-gas units", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "r.html")
		rs := mk(1000, 2000)
		if err := writeReport(p, rs, rs, "zisk"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		// The JS lookup is emitted unconditionally and is inert without the
		// checkbox, so assert on the input element itself.
		if strings.Contains(string(b), `id="excludeEmpty"`) {
			t.Error("the toggle must not render when every unit used gas")
		}
	})

	t.Run("some zero-gas units", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "r.html")
		rs := mk(0, 0, 5000)
		if err := writeReport(p, rs, rs, "zisk"); err != nil {
			t.Fatal(err)
		}
		html := string(mustRead(t, p))
		for _, want := range []string{`id="excludeEmpty"`, "Exclude zero-gas units", "const gasUsed", "statBody"} {
			if !strings.Contains(html, want) {
				t.Errorf("report is missing %q", want)
			}
		}
		// The per-unit gas array drives the filter, so it must be complete and
		// in row order.
		if !strings.Contains(html, "const gasUsed            = [0,0,5000];") {
			t.Error("per-unit gas array missing or out of order")
		}
		if !strings.Contains(html, "including 2 that used no gas") {
			t.Error("the zero-gas count must be stated up front")
		}
	})
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeStatelessOutput(t *testing.T) {
	root := strings.Repeat("ab", 32)
	full := root + "01" + "0100000000000000" + "0115"

	o := decodeStatelessOutput(full)
	if !o.OK || o.PayloadRoot != root || !o.Success {
		t.Errorf("valid output decoded wrong: %+v", o)
	}
	if o.ChainID != 1 || o.SchemaID != 0x1501 {
		t.Errorf("chain/schema = %d/%#x", o.ChainID, o.SchemaID)
	}

	if o := decodeStatelessOutput(root + "00" + "0100000000000000" + "0115"); o.Success {
		t.Error("valid byte 0x00 must decode as failure")
	}
	// A short or absent region must yield nothing, never a partial decode that
	// could read as a verdict.
	for _, bad := range []string{"", root, root + "01", "zz"} {
		if o := decodeStatelessOutput(bad); o.OK || o.Success || o.PayloadRoot != "" {
			t.Errorf("input %q should not decode: %+v", bad, o)
		}
	}
}

// The point of these columns: a run's CSV alone must reproduce the identity of
// its report's raw table, so a report never has to be rebuilt by scraping a
// previous one.
func TestWriteCSVCarriesTheGuestVerdict(t *testing.T) {
	root := strings.Repeat("cd", 32)
	rows := []BlockResult{
		{ // validated
			Kind: fixture.FormatZkevm, Label: "a::pass", Suite: "s", GasUsed: 10,
			OutputHex: root + "01" + "0100000000000000" + "0115",
		},
		{ // guest reported invalid
			Kind: fixture.FormatZkevm, Label: "b::fail", Suite: "s", GasUsed: 20,
			OutputHex: root + "00" + "0100000000000000" + "0115",
		},
		{ // no output region at all
			Kind: fixture.FormatZkevm, Label: "c::none", Suite: "s", GasUsed: 30,
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
		t.Fatal(err)
	}

	iRoot, iOK := -1, -1
	for i, h := range recs[0] {
		switch h {
		case "payload_root":
			iRoot = i
		case "success":
			iOK = i
		}
	}
	if iRoot < 0 || iOK < 0 {
		t.Fatalf("payload_root/success missing from header: %v", recs[0])
	}
	// Appended, so the original fifteen keep their positions.
	if recs[0][13] != "total" || recs[0][14] != "elapsed_ms" {
		t.Errorf("existing columns moved: %v", recs[0][:15])
	}

	want := [][2]string{{root, "1"}, {root, "0"}, {"", "0"}}
	for i, w := range want {
		if got := recs[i+1][iRoot]; got != w[0] {
			t.Errorf("row %d payload_root = %q, want %q", i, got, w[0])
		}
		if got := recs[i+1][iOK]; got != w[1] {
			t.Errorf("row %d success = %q, want %q", i, got, w[1])
		}
	}
}
