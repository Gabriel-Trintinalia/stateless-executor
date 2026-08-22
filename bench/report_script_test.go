package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

// The report's behaviour lives in a template string, so Go's compiler cannot see
// a syntax error or a const used ahead of its declaration in it. These tests
// execute the emitted script under node with the DOM and Chart.js stubbed, which
// is the only thing that catches that class of bug before a browser does.

// domStub is enough of a DOM and Chart.js for the script to reach the end of
// its top-level execution.
const domStub = `
class Chart {
  constructor(el, cfg) { this.el = el; this.cfg = cfg; this.data = cfg.data; Chart.made.push(this); }
  update() { this.updated = (this.updated || 0) + 1; }
}
Chart.made = [];
function mkEl(id) {
  return { id, checked: false, value: '', textContent: '', innerHTML: '',
           style: {}, rows: [], cells: [], classList: { contains: () => false },
           addEventListener(ev, fn) { (this.handlers ||= {})[ev] = fn; } };
}
const els = {};
const document = { getElementById(id) { return (els[id] ||= mkEl(id)); } };
`

// extractScript returns the report's inline script (the CDN tag has no body).
func extractScript(t *testing.T, html string) string {
	t.Helper()
	const open, close = "<script>", "</script>"
	i := strings.LastIndex(html, open)
	if i < 0 {
		t.Fatal("report has no inline script")
	}
	j := strings.Index(html[i:], close)
	if j < 0 {
		t.Fatal("unterminated script")
	}
	return html[i+len(open) : i+j]
}

func runNode(t *testing.T, src string) (string, error) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "report.js")
	if err := os.WriteFile(f, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("node", f).CombinedOutput()
	return string(out), err
}

func mkResults(kind fixture.Format, n int, gasEvery int) []BlockResult {
	rs := make([]BlockResult, n)
	for i := range rs {
		gas := uint64(1000 * (i + 1))
		if gasEvery > 0 && i%gasEvery == 0 {
			gas = 0 // a zero-gas unit, so the toggle renders
		}
		label := "rpc_block_" + strconv.Itoa(24758569+i)
		if kind == fixture.FormatZkevm {
			label = "tests/benchmark/compute/instruction/test_x.py::test_y[fork_Amsterdam-opcode_ADD-value_60M]"
		}
		rs[i] = BlockResult{
			Kind: kind, Label: label, BlockNum: uint64(i + 1), GasUsed: gas,
			Suite: "compute/instruction/arithmetic",
			Costs: CostReport{
				Base: 293601280, Main: uint64(i+1) * 7, Opcodes: uint64(i+1) * 3,
				Precompiles: uint64(i + 1), Memory: uint64(i+1) * 2, Total: uint64(i+1) * 13,
			},
			ValidationOK: true, Verdict: verdict{Kind: verdictPass},
		}
	}
	return rs
}

func TestReportScriptExecutes(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}

	cases := []struct {
		name   string
		kind   fixture.Format
		n      int
		zeroes int
		target string
	}{
		{"corpus per-unit charts", fixture.FormatCorpus, 12, 0, "zisk"},
		{"eest per-unit charts with toggle", fixture.FormatZkevm, 12, 3, "zisk"},
		{"eest scaled charts with toggle", fixture.FormatZkevm, maxChartUnits + 5, 3, "zisk"},
		{"openvm", fixture.FormatCorpus, 12, 0, "openvm"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := mkResults(tc.kind, tc.n, tc.zeroes)
			p := filepath.Join(t.TempDir(), "r.html")
			if err := writeReport(p, rs, rs, tc.target); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			script := extractScript(t, string(b))

			out, err := runNode(t, domStub+script+"\nconsole.log('CHARTS=' + Chart.made.length);\n")
			if err != nil {
				t.Fatalf("the report's script failed to execute:\n%s", out)
			}
			if !strings.Contains(out, "CHARTS=") {
				t.Fatalf("script did not reach the end of execution:\n%s", out)
			}
		})
	}
}

// The zero-gas toggle must actually change the numbers, and must agree with
// computeStats — the report and the console summary cannot be allowed to drift.
func TestReportToggleRecomputesStats(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}

	rs := mkResults(fixture.FormatZkevm, 40, 2) // every other unit is zero-gas
	p := filepath.Join(t.TempDir(), "r.html")
	if err := writeReport(p, rs, rs, "zisk"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	script := extractScript(t, string(b))

	driver := `
const cb = document.getElementById('excludeEmpty');
const body = document.getElementById('statBody');
function totalRow() {
  const m = body.innerHTML.match(/<tr><td>TOTAL<\/td><td>(\d+)<\/td><td>(\d+)<\/td><td>(\d+)<\/td><td>(\d+)<\/td><\/tr>/);
  return m ? m.slice(1).map(Number) : null;
}
cb.checked = false; cb.handlers.change();
const all = totalRow();
cb.checked = true; cb.handlers.change();
const gasOnly = totalRow();
console.log(JSON.stringify({all, gasOnly, scope: document.getElementById('statScope').textContent}));
`
	out, err := runNode(t, domStub+script+driver)
	if err != nil {
		t.Fatalf("toggle driver failed:\n%s", out)
	}

	// Independently compute what Go would report for each scope.
	worked, empty := partitionByGas(rs)
	if len(empty) == 0 || len(worked) == 0 {
		t.Fatal("test data must contain both zero-gas and gas-using units")
	}
	totals := func(in []BlockResult) []uint64 {
		v := make([]uint64, len(in))
		for i, r := range in {
			v[i] = r.Costs.Total
		}
		return v
	}
	wantAll := computeStats(totals(rs))
	wantGas := computeStats(totals(worked))

	for _, want := range []struct {
		key string
		s   costStats
	}{{`"all":[`, wantAll}, {`"gasOnly":[`, wantGas}} {
		frag := want.key + itoa(want.s.Min) + "," + itoa(want.s.P50) + "," +
			itoa(want.s.Max) + "," + itoa(want.s.Avg) + "]"
		if !strings.Contains(out, frag) {
			t.Errorf("report stats disagree with computeStats.\nwant substring %s\ngot %s", frag, out)
		}
	}
	if !strings.Contains(out, "zero-gas excluded") {
		t.Errorf("the toggled scope must say what it excluded: %s", out)
	}
}

func itoa(v uint64) string { return strconv.FormatUint(v, 10) }
