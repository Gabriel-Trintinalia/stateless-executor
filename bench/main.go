// Command bench converts fixture JSON files to zesu-zkvm binary inputs and
// runs each one through ziskemu, reporting cost statistics and generating an
// HTML report with charts.
//
// Usage:
//
//	bench --fixtures <dir> --elf <path> [--ziskemu <path>] [--jobs N] [--report <path>]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"html/template"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

// CostReport holds the parsed ziskemu COST DISTRIBUTION table for one block.
type CostReport struct {
	Base       uint64
	Main       uint64
	Opcodes    uint64
	Precompiles uint64
	Memory     uint64
	Total      uint64
}

// BlockResult holds the outcome of running one fixture block.
type BlockResult struct {
	BlockNum  uint64
	Name      string
	Costs     CostReport
	Err       error
	ErrOutput string // raw ziskemu output when Err != nil
	ExecError string // non-empty when ziskemu ran but EVM execution failed (e.g. "InvalidGasUsed")
	Elapsed   time.Duration
}

var blockNumRe = regexp.MustCompile(`block_(\d+)`)

func main() {
	fixturesDir := flag.String("fixtures", "", "directory containing fixture JSON files (required)")
	elfPath := flag.String("elf", "", "path to the zesu-zkvm ELF binary (required)")
	ziskemuPath := flag.String("ziskemu", "ziskemu-0.16.1", "path to ziskemu binary")
	jobs := flag.Int("jobs", 1, "number of parallel ziskemu runs")
	reportPath := flag.String("report", "bench_report.html", "output HTML report path")
	flag.Parse()

	if *fixturesDir == "" || *elfPath == "" {
		flag.Usage()
		os.Exit(1)
	}
	if _, err := os.Stat(*elfPath); err != nil {
		log.Fatalf("ELF not found at %s: %v", *elfPath, err)
	}

	paths, err := collectJSON(*fixturesDir)
	if err != nil {
		log.Fatalf("collect fixtures: %v", err)
	}
	if len(paths) == 0 {
		log.Fatalf("no JSON fixtures found in %s", *fixturesDir)
	}
	log.Printf("found %d fixtures, running with ziskemu (%d job(s))...", len(paths), *jobs)

	results := make([]BlockResult, len(paths))
	sem := make(chan struct{}, *jobs)
	var wg sync.WaitGroup
	var done atomic.Int64

	for i, p := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, path string) {
			defer wg.Done()
			defer func() { <-sem }()

			name := strings.TrimSuffix(filepath.Base(path), ".json")
			blockNum := extractBlockNum(name)
			t := time.Now()
			costs, execErr, errOut, runErr := benchOne(path, *elfPath, *ziskemuPath)
			elapsed := time.Since(t)
			n := done.Add(1)
			if runErr != nil {
				fmt.Printf("[%3d/%d] ERROR %-40s  %v\n", n, len(paths), name, runErr)
			} else if execErr != "" {
				fmt.Printf("[%3d/%d] block %d  total=%d  EXEC FAILED: %s  (%s)\n", n, len(paths), blockNum, costs.Total, execErr, elapsed.Round(time.Millisecond))
			} else {
				fmt.Printf("[%3d/%d] block %d  total=%d  (%s)\n", n, len(paths), blockNum, costs.Total, elapsed.Round(time.Millisecond))
			}
			results[idx] = BlockResult{
				BlockNum:  blockNum,
				Name:      name,
				Costs:     costs,
				Err:       runErr,
				ErrOutput: errOut,
				ExecError: execErr,
				Elapsed:   elapsed,
			}
		}(i, p)
	}
	wg.Wait()

	// Collect successful results sorted by block number.
	var good []BlockResult
	for _, r := range results {
		if r.Err == nil {
			good = append(good, r)
		}
	}
	sort.Slice(good, func(i, j int) bool { return good[i].BlockNum < good[j].BlockNum })

	if len(good) > 0 {
		printSummary(good, len(results))
	} else {
		log.Printf("WARNING: no successful results — report will contain errors only")
	}

	if err := writeReport(*reportPath, good, results); err != nil {
		log.Fatalf("write report: %v", err)
	}
	log.Printf("report written to %s", *reportPath)
}

// benchOne returns (costs, execError, rawOutput, error). execError is non-empty
// when ziskemu ran successfully but the EVM execution itself failed (success=0).
// rawOutput is always populated from ziskemu's combined output.
func benchOne(fixturePath, elfPath, ziskemuPath string) (CostReport, string, string, error) {
	f, err := fixture.LoadFile(fixturePath)
	if err != nil {
		return CostReport{}, "", "", fmt.Errorf("load: %w", err)
	}

	input, err := fixture.ZesuInput(f)
	if err != nil {
		return CostReport{}, "", "", fmt.Errorf("encode: %w", err)
	}

	tmp, err := os.CreateTemp("", "zesu-bench-*.bin")
	if err != nil {
		return CostReport{}, "", "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(input); err != nil {
		tmp.Close()
		return CostReport{}, "", "", err
	}
	if err := tmp.Close(); err != nil {
		return CostReport{}, "", "", err
	}

	out, err := exec.Command(ziskemuPath, "-X", "-e", elfPath, "-i", tmp.Name()).
		CombinedOutput()
	rawOut := strings.TrimSpace(string(out))
	if err != nil {
		return CostReport{}, "", rawOut, fmt.Errorf("ziskemu: %w", err)
	}

	costs, ok := parseCostReport(rawOut)
	if !ok {
		return CostReport{}, "", rawOut, fmt.Errorf("no COST DISTRIBUTION in ziskemu output")
	}
	execErr := parseExecError(rawOut)
	return costs, execErr, rawOut, nil
}

var execFailedRe = regexp.MustCompile(`error: execution failed: (\S+)`)

// parseExecError returns the failure reason from a line like
// "error: execution failed: InvalidGasUsed", or "" if not found.
func parseExecError(output string) string {
	m := execFailedRe.FindStringSubmatch(output)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// parseCostReport parses the ziskemu COST DISTRIBUTION table:
//
//	BASE                         293,601,280  99.89%
//	MAIN                             247,452   0.08%
//	OPCODES                           11,069   0.00%
//	PRECOMPILES                            0   0.00%
//	MEMORY                            58,854   0.02%
//	TOTAL                        293,918,655 100.00%
func parseCostReport(output string) (CostReport, bool) {
	var r CostReport
	sc := bufio.NewScanner(strings.NewReader(output))
	found := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := parseCostLine(line, "BASE"); ok {
			r.Base = v
			found = true
		} else if v, ok := parseCostLine(line, "MAIN"); ok {
			r.Main = v
		} else if v, ok := parseCostLine(line, "OPCODES"); ok {
			r.Opcodes = v
		} else if v, ok := parseCostLine(line, "PRECOMPILES"); ok {
			r.Precompiles = v
		} else if v, ok := parseCostLine(line, "MEMORY"); ok {
			r.Memory = v
		} else if v, ok := parseCostLine(line, "TOTAL"); ok {
			r.Total = v
		}
	}
	return r, found
}

// parseCostLine extracts the numeric cost from a line like "BASE   293,601,280  99.89%".
func parseCostLine(line, label string) (uint64, bool) {
	rest, ok := strings.CutPrefix(line, label)
	if !ok {
		return 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, false
	}
	clean := strings.ReplaceAll(fields[0], ",", "")
	n, err := strconv.ParseUint(clean, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func extractBlockNum(name string) uint64 {
	m := blockNumRe.FindStringSubmatch(name)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	return n
}

// costStats computes min/p50/max/avg for a slice of uint64 values (must be sorted).
type costStats struct {
	Min, P50, Max, Avg uint64
}

func computeStats(vals []uint64) costStats {
	if len(vals) == 0 {
		return costStats{}
	}
	sorted := make([]uint64, len(vals))
	copy(sorted, vals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum uint64
	for _, v := range sorted {
		sum += v
	}
	return costStats{
		Min: sorted[0],
		P50: sorted[len(sorted)/2],
		Max: sorted[len(sorted)-1],
		Avg: sum / uint64(len(sorted)),
	}
}

func printSummary(good []BlockResult, total int) {
	extract := func(fn func(CostReport) uint64) []uint64 {
		vs := make([]uint64, len(good))
		for i, r := range good {
			vs[i] = fn(r.Costs)
		}
		return vs
	}
	fmt.Printf("\n=== Results (%d/%d blocks) ===\n", len(good), total)
	fmt.Printf("%-14s %18s %18s %18s %18s\n", "COMPONENT", "MIN", "P50", "MAX", "AVG")
	fmt.Printf("%s\n", strings.Repeat("-", 92))
	for _, row := range []struct {
		label string
		fn    func(CostReport) uint64
	}{
		{"BASE", func(c CostReport) uint64 { return c.Base }},
		{"MAIN", func(c CostReport) uint64 { return c.Main }},
		{"OPCODES", func(c CostReport) uint64 { return c.Opcodes }},
		{"PRECOMPILES", func(c CostReport) uint64 { return c.Precompiles }},
		{"MEMORY", func(c CostReport) uint64 { return c.Memory }},
		{"TOTAL", func(c CostReport) uint64 { return c.Total }},
	} {
		s := computeStats(extract(row.fn))
		fmt.Printf("%-14s %18d %18d %18d %18d\n", row.label, s.Min, s.P50, s.Max, s.Avg)
	}
}

// ── HTML report ───────────────────────────────────────────────────────────────

type reportData struct {
	Generated   string
	Total       int
	Good        int
	Failed      int
	StatRows    []statRow
	Labels      template.JS // JSON array of block numbers
	TotalCosts  template.JS
	BaseCosts   template.JS
	MainCosts   template.JS
	OpCosts     template.JS
	PreCosts    template.JS
	MemCosts    template.JS
	ExecFailed  []execFailedRow
	Errors      []errorRow
}

type execFailedRow struct {
	BlockNum uint64
	Name     string
	Reason   string
}

type errorRow struct {
	BlockNum uint64
	Name     string
	ErrMsg   string
	Output   string
}

type statRow struct {
	Label string
	Min   uint64
	P50   uint64
	Max   uint64
	Avg   uint64
}

func writeReport(path string, good []BlockResult, all []BlockResult) error {
	total := len(all)
	extract := func(fn func(CostReport) uint64) []uint64 {
		vs := make([]uint64, len(good))
		for i, r := range good {
			vs[i] = fn(r.Costs)
		}
		return vs
	}

	toJS := func(vs []uint64) template.JS {
		var sb strings.Builder
		sb.WriteByte('[')
		for i, v := range vs {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(strconv.FormatUint(v, 10))
		}
		sb.WriteByte(']')
		return template.JS(sb.String())
	}

	blockNums := make([]uint64, len(good))
	for i, r := range good {
		blockNums[i] = r.BlockNum
	}

	var statRows []statRow
	for _, row := range []struct {
		label string
		fn    func(CostReport) uint64
	}{
		{"BASE", func(c CostReport) uint64 { return c.Base }},
		{"MAIN", func(c CostReport) uint64 { return c.Main }},
		{"OPCODES", func(c CostReport) uint64 { return c.Opcodes }},
		{"PRECOMPILES", func(c CostReport) uint64 { return c.Precompiles }},
		{"MEMORY", func(c CostReport) uint64 { return c.Memory }},
		{"TOTAL", func(c CostReport) uint64 { return c.Total }},
	} {
		s := computeStats(extract(row.fn))
		statRows = append(statRows, statRow{
			Label: row.label,
			Min:   s.Min,
			P50:   s.P50,
			Max:   s.Max,
			Avg:   s.Avg,
		})
	}

	var execFailedRows []execFailedRow
	for _, r := range all {
		if r.Err == nil && r.ExecError != "" {
			execFailedRows = append(execFailedRows, execFailedRow{
				BlockNum: r.BlockNum,
				Name:     r.Name,
				Reason:   r.ExecError,
			})
		}
	}
	sort.Slice(execFailedRows, func(i, j int) bool { return execFailedRows[i].BlockNum < execFailedRows[j].BlockNum })

	var errRows []errorRow
	for _, r := range all {
		if r.Err != nil {
			errRows = append(errRows, errorRow{
				BlockNum: r.BlockNum,
				Name:     r.Name,
				ErrMsg:   r.Err.Error(),
				Output:   r.ErrOutput,
			})
		}
	}
	sort.Slice(errRows, func(i, j int) bool { return errRows[i].BlockNum < errRows[j].BlockNum })

	data := reportData{
		Generated:  time.Now().Format(time.RFC1123),
		Total:      total,
		Good:       len(good),
		Failed:     len(execFailedRows),
		StatRows:   statRows,
		Labels:     toJS(blockNums),
		TotalCosts: toJS(extract(func(c CostReport) uint64 { return c.Total })),
		BaseCosts:  toJS(extract(func(c CostReport) uint64 { return c.Base })),
		MainCosts:  toJS(extract(func(c CostReport) uint64 { return c.Main })),
		OpCosts:    toJS(extract(func(c CostReport) uint64 { return c.Opcodes })),
		PreCosts:   toJS(extract(func(c CostReport) uint64 { return c.Precompiles })),
		MemCosts:   toJS(extract(func(c CostReport) uint64 { return c.Memory })),
		ExecFailed: execFailedRows,
		Errors:     errRows,
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return reportTmpl.Execute(f, data)
}

var reportTmpl = template.Must(template.New("report").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>zesu-zkvm Benchmark Report</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4/dist/chart.umd.min.js"></script>
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem; background: #f8f9fa; color: #212529; }
  h1 { font-size: 1.6rem; }
  .meta { color: #6c757d; font-size: 0.9rem; margin-bottom: 1.5rem; }
  table { border-collapse: collapse; width: 100%; max-width: 700px; margin-bottom: 2rem; }
  th, td { border: 1px solid #dee2e6; padding: 0.4rem 0.8rem; text-align: right; }
  th:first-child, td:first-child { text-align: left; }
  thead th { background: #343a40; color: #fff; }
  tbody tr:nth-child(even) { background: #e9ecef; }
  tbody tr:last-child { font-weight: bold; background: #dee2e6; }
  .chart-wrap { background: #fff; border-radius: 8px; padding: 1rem; margin-bottom: 2rem;
                box-shadow: 0 1px 4px rgba(0,0,0,.1); max-width: 1100px; }
  canvas { max-height: 400px; }
  #execFailTable { max-width: 600px; }
  #execFailTable td:first-child { width: 10rem; }
  #errTable { max-width: 1100px; }
  #errTable td:first-child { font-family: monospace; width: 10rem; }
</style>
</head>
<body>
<h1>zesu-zkvm Benchmark Report</h1>
<p class="meta">Generated: {{.Generated}} &nbsp;|&nbsp; Blocks: {{.Good}}/{{.Total}} succeeded{{if .Failed}} &nbsp;|&nbsp; <span style="color:#dc3545">{{.Failed}} execution failure(s)</span>{{end}}</p>

<h2>Cost Summary</h2>
<table>
  <thead><tr><th>Component</th><th>Min</th><th>P50</th><th>Max</th><th>Avg</th></tr></thead>
  <tbody>
  {{range .StatRows}}
  <tr>
    <td>{{.Label}}</td>
    <td>{{.Min}}</td>
    <td>{{.P50}}</td>
    <td>{{.Max}}</td>
    <td>{{.Avg}}</td>
  </tr>
  {{end}}
  </tbody>
</table>

<h2>Total Cost by Block</h2>
<div class="chart-wrap"><canvas id="totalChart"></canvas></div>

<h2>Cost Breakdown by Block</h2>
<div class="chart-wrap"><canvas id="stackedChart"></canvas></div>

{{if .ExecFailed}}
<h2>Execution Failures ({{len .ExecFailed}} blocks)</h2>
<table id="execFailTable">
  <thead><tr><th>Block</th><th>Reason</th></tr></thead>
  <tbody>
  {{range .ExecFailed}}
  <tr>
    <td style="white-space:nowrap;font-family:monospace">{{.BlockNum}}</td>
    <td style="font-family:monospace;color:#dc3545">{{.Reason}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
{{end}}

{{if .Errors}}
<h2>Errors ({{len .Errors}} blocks)</h2>
<table id="errTable">
  <thead><tr><th>Block</th><th>Error</th></tr></thead>
  <tbody>
  {{range .Errors}}
  <tr>
    <td style="white-space:nowrap">{{.BlockNum}}</td>
    <td>
      <details>
        <summary style="cursor:pointer;font-family:monospace">{{.ErrMsg}}</summary>
        <pre style="margin:.5rem 0;padding:.5rem;background:#f1f3f5;border-radius:4px;overflow-x:auto;font-size:.8rem">{{.Output}}</pre>
      </details>
    </td>
  </tr>
  {{end}}
  </tbody>
</table>
{{end}}

<script>
const labels = {{.Labels}};
const totalCosts = {{.TotalCosts}};
const baseCosts  = {{.BaseCosts}};
const mainCosts  = {{.MainCosts}};
const opCosts    = {{.OpCosts}};
const preCosts   = {{.PreCosts}};
const memCosts   = {{.MemCosts}};

new Chart(document.getElementById('totalChart'), {
  type: 'line',
  data: {
    labels,
    datasets: [{
      label: 'TOTAL COST',
      data: totalCosts,
      borderColor: '#0d6efd',
      backgroundColor: 'rgba(13,110,253,0.08)',
      borderWidth: 1.5,
      pointRadius: 2,
      fill: true,
      tension: 0.2,
    }]
  },
  options: {
    responsive: true,
    plugins: { legend: { display: false } },
    scales: {
      x: { title: { display: true, text: 'Block Number' } },
      y: { title: { display: true, text: 'Cost' }, beginAtZero: true }
    }
  }
});

new Chart(document.getElementById('stackedChart'), {
  type: 'bar',
  data: {
    labels,
    datasets: [
      { label: 'BASE',        data: baseCosts,  backgroundColor: '#0d6efd' },
      { label: 'MAIN',        data: mainCosts,  backgroundColor: '#6610f2' },
      { label: 'OPCODES',     data: opCosts,    backgroundColor: '#198754' },
      { label: 'PRECOMPILES', data: preCosts,   backgroundColor: '#fd7e14' },
      { label: 'MEMORY',      data: memCosts,   backgroundColor: '#dc3545' },
    ]
  },
  options: {
    responsive: true,
    plugins: { legend: { position: 'top' } },
    scales: {
      x: { stacked: true, title: { display: true, text: 'Block Number' } },
      y: { stacked: true, title: { display: true, text: 'Cost' }, beginAtZero: true }
    }
  }
});
</script>
</body>
</html>
`))

func collectJSON(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			paths = append(paths, filepath.Join(path, e.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

