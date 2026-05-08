// Command bench converts fixture JSON files to zesu-zkvm binary inputs and
// runs each one through a zkVM emulator, reporting statistics and generating
// an HTML report.
//
// Usage:
//
//	bench --fixtures <dir> --elf <path> [--target zisk|openvm] [--ziskemu <path>] [--runner <path>] [--jobs N] [--report <path>]
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
	Base        uint64
	Main        uint64
	Opcodes     uint64
	Precompiles uint64
	Memory      uint64
	Total       uint64
}

// BlockResult holds the outcome of running one fixture block.
type BlockResult struct {
	BlockNum        uint64
	Name            string
	Target          string // "zisk" or "openvm"
	Costs           CostReport
	Err             error
	ErrOutput       string
	ExecError       string
	Elapsed         time.Duration
	ExpectedSuccess bool
	ValidationOK    bool
}

var blockNumRe = regexp.MustCompile(`block_(\d+)`)

func main() {
	fixturesDir := flag.String("fixtures", "", "directory containing fixture JSON files (required)")
	elfPath := flag.String("elf", "", "path to the zesu-zkvm ELF binary (required)")
	targetFlag := flag.String("target", "zisk", "zkVM target: zisk or openvm")
	ziskemuPath := flag.String("ziskemu", "ziskemu", "path to ziskemu binary (zisk-0.17+)")
	runnerPath := flag.String("runner", "zesu-openvm-runner", "path to openvm runner binary")
	jobs := flag.Int("jobs", 1, "number of parallel emulator runs")
	reportPath := flag.String("report", "bench_report.html", "output HTML report path")
	flag.Parse()

	if *fixturesDir == "" || *elfPath == "" {
		flag.Usage()
		os.Exit(1)
	}
	if *targetFlag != "zisk" && *targetFlag != "openvm" {
		log.Fatalf("unknown target %q: must be zisk or openvm", *targetFlag)
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
	log.Printf("found %d fixtures, running with %s (%d job(s))...", len(paths), *targetFlag, *jobs)

	var runBench func(fixturePath string) (CostReport, string, string, error, bool)
	if *targetFlag == "openvm" {
		ep, rp := *elfPath, *runnerPath
		runBench = func(p string) (CostReport, string, string, error, bool) {
			return benchOneOpenVM(p, ep, rp)
		}
	} else {
		ep, zp := *elfPath, *ziskemuPath
		runBench = func(p string) (CostReport, string, string, error, bool) {
			return benchOne(p, ep, zp)
		}
	}

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
			costs, execErr, errOut, runErr, expectedSuccess := runBench(path)
			elapsed := time.Since(t)
			gotSuccess := runErr == nil && execErr == ""
			validationOK := runErr == nil && gotSuccess == expectedSuccess
			n := done.Add(1)
			if runErr != nil {
				fmt.Printf("[%3d/%d] ERROR %-40s  %v\n", n, len(paths), name, runErr)
			} else if !validationOK {
				if *targetFlag == "openvm" {
					fmt.Printf("[%3d/%d] block %d  VALIDATION FAILED (expected success=%v)  (%s)\n", n, len(paths), blockNum, expectedSuccess, elapsed.Round(time.Millisecond))
				} else {
					fmt.Printf("[%3d/%d] block %d  total=%d  VALIDATION FAILED (expected success=%v)  (%s)\n", n, len(paths), blockNum, costs.Total, expectedSuccess, elapsed.Round(time.Millisecond))
				}
			} else if execErr != "" {
				if *targetFlag == "openvm" {
					fmt.Printf("[%3d/%d] block %d  EXEC FAILED (expected): %s  (%s)\n", n, len(paths), blockNum, execErr, elapsed.Round(time.Millisecond))
				} else {
					fmt.Printf("[%3d/%d] block %d  total=%d  EXEC FAILED (expected): %s  (%s)\n", n, len(paths), blockNum, costs.Total, execErr, elapsed.Round(time.Millisecond))
				}
			} else {
				if *targetFlag == "openvm" {
					fmt.Printf("[%3d/%d] block %d  (%s)\n", n, len(paths), blockNum, elapsed.Round(time.Millisecond))
				} else {
					fmt.Printf("[%3d/%d] block %d  total=%d  (%s)\n", n, len(paths), blockNum, costs.Total, elapsed.Round(time.Millisecond))
				}
			}
			results[idx] = BlockResult{
				BlockNum:        blockNum,
				Name:            name,
				Target:          *targetFlag,
				Costs:           costs,
				Err:             runErr,
				ErrOutput:       errOut,
				ExecError:       execErr,
				Elapsed:         elapsed,
				ExpectedSuccess: expectedSuccess,
				ValidationOK:    validationOK,
			}
		}(i, p)
	}
	wg.Wait()

	var good []BlockResult
	var validationFailures []BlockResult
	for _, r := range results {
		if r.Err == nil {
			good = append(good, r)
			if !r.ValidationOK {
				validationFailures = append(validationFailures, r)
			}
		}
	}
	sort.Slice(good, func(i, j int) bool { return good[i].BlockNum < good[j].BlockNum })

	if len(good) > 0 {
		printSummary(good, len(results), len(validationFailures), *targetFlag)
	} else {
		log.Printf("WARNING: no successful results — report will contain errors only")
	}
	if len(validationFailures) > 0 {
		log.Printf("VALIDATION FAILURES: %d block(s) had unexpected execution outcome", len(validationFailures))
	}

	if err := writeReport(*reportPath, good, results, *targetFlag); err != nil {
		log.Fatalf("write report: %v", err)
	}
	log.Printf("report written to %s", *reportPath)
}

// benchOne returns (costs, execError, rawOutput, error, expectedSuccess) for a ZisK run.
func benchOne(fixturePath, elfPath, ziskemuPath string) (CostReport, string, string, error, bool) {
	f, err := fixture.LoadFile(fixturePath)
	if err != nil {
		return CostReport{}, "", "", fmt.Errorf("load: %w", err), false
	}

	input, err := fixture.ZesuInputSSZ(f)
	if err != nil {
		return CostReport{}, "", "", fmt.Errorf("encode: %w", err), f.Success
	}

	tmp, err := os.CreateTemp("", "zesu-bench-*.bin")
	if err != nil {
		return CostReport{}, "", "", err, f.Success
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(input); err != nil {
		tmp.Close()
		return CostReport{}, "", "", err, f.Success
	}
	if err := tmp.Close(); err != nil {
		return CostReport{}, "", "", err, f.Success
	}

	out, err := exec.Command(ziskemuPath, "-X", "-e", elfPath, "-i", tmp.Name()).
		CombinedOutput()
	rawOut := strings.TrimSpace(string(out))
	if err != nil {
		return CostReport{}, "", rawOut, fmt.Errorf("ziskemu: %w", err), f.Success
	}

	costs, ok := parseCostReport(rawOut)
	if !ok {
		return CostReport{}, "", rawOut, fmt.Errorf("no COST DISTRIBUTION in ziskemu output"), f.Success
	}
	execErr := parseExecError(rawOut)
	return costs, execErr, rawOut, nil, f.Success
}

// benchOneOpenVM returns (costs, execError, rawOutput, error, expectedSuccess) for an OpenVM run.
// costs is always zero — OpenVM emulation does not produce a circuit cost breakdown.
// execError is "ExecutionFailed" when the guest writes success=0 to public values byte[32].
func benchOneOpenVM(fixturePath, elfPath, runnerPath string) (CostReport, string, string, error, bool) {
	f, err := fixture.LoadFile(fixturePath)
	if err != nil {
		return CostReport{}, "", "", fmt.Errorf("load: %w", err), false
	}

	input, err := fixture.ZesuInputOpenVM(f)
	if err != nil {
		return CostReport{}, "", "", fmt.Errorf("encode: %w", err), f.Success
	}

	tmpIn, err := os.CreateTemp("", "zesu-bench-in-*.bin")
	if err != nil {
		return CostReport{}, "", "", err, f.Success
	}
	defer os.Remove(tmpIn.Name())
	if _, err := tmpIn.Write(input); err != nil {
		tmpIn.Close()
		return CostReport{}, "", "", err, f.Success
	}
	if err := tmpIn.Close(); err != nil {
		return CostReport{}, "", "", err, f.Success
	}

	tmpOut, err := os.CreateTemp("", "zesu-bench-out-*.bin")
	if err != nil {
		return CostReport{}, "", "", err, f.Success
	}
	tmpOutPath := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpOutPath)

	out, err := exec.Command(runnerPath, elfPath, tmpIn.Name(), "-o", tmpOutPath).
		CombinedOutput()
	rawOut := strings.TrimSpace(string(out))
	if err != nil {
		return CostReport{}, "", rawOut, fmt.Errorf("runner: %w", err), f.Success
	}

	outBytes, err := os.ReadFile(tmpOutPath)
	if err != nil {
		return CostReport{}, "", rawOut, fmt.Errorf("read output: %w", err), f.Success
	}
	if len(outBytes) < 41 {
		return CostReport{}, "", rawOut, fmt.Errorf("output too short: %d bytes (expected 41)", len(outBytes)), f.Success
	}

	execErr := ""
	if outBytes[32] == 0 {
		execErr = "ExecutionFailed"
	}
	return CostReport{}, execErr, rawOut, nil, f.Success
}

var execFailedRe = regexp.MustCompile(`error: execution failed: (\S+)`)

func parseExecError(output string) string {
	m := execFailedRe.FindStringSubmatch(output)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// parseCostReport parses the ziskemu COST DISTRIBUTION table.
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

func printSummary(good []BlockResult, total, validationFailures int, target string) {
	validated := len(good) - validationFailures
	fmt.Printf("\n=== Results (%d/%d blocks, %d/%d validated) ===\n", len(good), total, validated, len(good))

	if target == "openvm" {
		elapsedMs := make([]uint64, len(good))
		for i, r := range good {
			elapsedMs[i] = uint64(r.Elapsed.Milliseconds())
		}
		s := computeStats(elapsedMs)
		fmt.Printf("%-14s %18s %18s %18s %18s\n", "ELAPSED (ms)", "MIN", "P50", "MAX", "AVG")
		fmt.Printf("%s\n", strings.Repeat("-", 92))
		fmt.Printf("%-14s %18d %18d %18d %18d\n", "ELAPSED", s.Min, s.P50, s.Max, s.Avg)
		return
	}

	extract := func(fn func(CostReport) uint64) []uint64 {
		vs := make([]uint64, len(good))
		for i, r := range good {
			vs[i] = fn(r.Costs)
		}
		return vs
	}
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
	Generated        string
	Target           string
	Total            int
	Good             int
	Failed           int
	ValidationFailed int
	StatRows         []statRow
	Labels           template.JS
	ElapsedMs        template.JS // ms per block (both targets)
	TotalCosts       template.JS // ZisK only
	BaseCosts        template.JS
	MainCosts        template.JS
	OpCosts          template.JS
	PreCosts         template.JS
	MemCosts         template.JS
	ExecFailed       []execFailedRow
	ValidationFails  []validationFailRow
	Errors           []errorRow
}

type execFailedRow struct {
	BlockNum uint64
	Name     string
	Reason   string
}

type validationFailRow struct {
	BlockNum        uint64
	Name            string
	ExpectedSuccess bool
	ExecError       string
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

func writeReport(path string, good []BlockResult, all []BlockResult, target string) error {
	total := len(all)

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
	elapsedMs := make([]uint64, len(good))
	for i, r := range good {
		blockNums[i] = r.BlockNum
		elapsedMs[i] = uint64(r.Elapsed.Milliseconds())
	}

	extract := func(fn func(CostReport) uint64) []uint64 {
		vs := make([]uint64, len(good))
		for i, r := range good {
			vs[i] = fn(r.Costs)
		}
		return vs
	}

	var statRows []statRow
	if target == "openvm" {
		s := computeStats(elapsedMs)
		statRows = []statRow{{Label: "ELAPSED (ms)", Min: s.Min, P50: s.P50, Max: s.Max, Avg: s.Avg}}
	} else {
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
	}

	var execFailedRows []execFailedRow
	var validationFailRows []validationFailRow
	for _, r := range all {
		if r.Err == nil && r.ExecError != "" && r.ValidationOK {
			execFailedRows = append(execFailedRows, execFailedRow{
				BlockNum: r.BlockNum,
				Name:     r.Name,
				Reason:   r.ExecError,
			})
		}
		if r.Err == nil && !r.ValidationOK {
			validationFailRows = append(validationFailRows, validationFailRow{
				BlockNum:        r.BlockNum,
				Name:            r.Name,
				ExpectedSuccess: r.ExpectedSuccess,
				ExecError:       r.ExecError,
			})
		}
	}
	sort.Slice(execFailedRows, func(i, j int) bool { return execFailedRows[i].BlockNum < execFailedRows[j].BlockNum })
	sort.Slice(validationFailRows, func(i, j int) bool { return validationFailRows[i].BlockNum < validationFailRows[j].BlockNum })

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
		Generated:        time.Now().Format(time.RFC1123),
		Target:           target,
		Total:            total,
		Good:             len(good),
		Failed:           len(execFailedRows),
		ValidationFailed: len(validationFailRows),
		StatRows:         statRows,
		Labels:           toJS(blockNums),
		ElapsedMs:        toJS(elapsedMs),
		TotalCosts:       toJS(extract(func(c CostReport) uint64 { return c.Total })),
		BaseCosts:        toJS(extract(func(c CostReport) uint64 { return c.Base })),
		MainCosts:        toJS(extract(func(c CostReport) uint64 { return c.Main })),
		OpCosts:          toJS(extract(func(c CostReport) uint64 { return c.Opcodes })),
		PreCosts:         toJS(extract(func(c CostReport) uint64 { return c.Precompiles })),
		MemCosts:         toJS(extract(func(c CostReport) uint64 { return c.Memory })),
		ExecFailed:       execFailedRows,
		ValidationFails:  validationFailRows,
		Errors:           errRows,
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
  .target-badge { display: inline-block; padding: .1rem .5rem; border-radius: 4px; font-size: .8rem; font-weight: 600;
                  background: #0d6efd; color: #fff; margin-left: .5rem; vertical-align: middle; }
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
<h1>zesu-zkvm Benchmark Report <span class="target-badge">{{.Target}}</span></h1>
<p class="meta">Generated: {{.Generated}} &nbsp;|&nbsp; Blocks: {{.Good}}/{{.Total}} succeeded{{if .ValidationFailed}} &nbsp;|&nbsp; <span style="color:#dc3545">{{.ValidationFailed}} validation failure(s)</span>{{end}}{{if .Failed}} &nbsp;|&nbsp; {{.Failed}} expected failure(s){{end}}</p>

<h2>Summary</h2>
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

<h2>Elapsed Time by Block (ms)</h2>
<div class="chart-wrap"><canvas id="elapsedChart"></canvas></div>

{{if eq .Target "zisk"}}
<h2>Total Cost by Block</h2>
<div class="chart-wrap"><canvas id="totalChart"></canvas></div>

<h2>Cost Breakdown by Block</h2>
<div class="chart-wrap"><canvas id="stackedChart"></canvas></div>
{{end}}

{{if .ValidationFails}}
<h2>Validation Failures ({{len .ValidationFails}} blocks)</h2>
<table id="validationFailTable">
  <thead><tr><th>Block</th><th>Expected</th><th>Got</th></tr></thead>
  <tbody>
  {{range .ValidationFails}}
  <tr>
    <td style="white-space:nowrap;font-family:monospace">{{.BlockNum}}</td>
    <td>{{if .ExpectedSuccess}}success{{else}}failure{{end}}</td>
    <td style="color:#dc3545">{{if .ExecError}}failed: {{.ExecError}}{{else}}success{{end}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
{{end}}

{{if .ExecFailed}}
<h2>Expected Failures ({{len .ExecFailed}} blocks)</h2>
<table id="execFailTable">
  <thead><tr><th>Block</th><th>Reason</th></tr></thead>
  <tbody>
  {{range .ExecFailed}}
  <tr>
    <td style="white-space:nowrap;font-family:monospace">{{.BlockNum}}</td>
    <td style="font-family:monospace;color:#6c757d">{{.Reason}}</td>
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
const labels     = {{.Labels}};
const elapsedMs  = {{.ElapsedMs}};
const totalCosts = {{.TotalCosts}};
const baseCosts  = {{.BaseCosts}};
const mainCosts  = {{.MainCosts}};
const opCosts    = {{.OpCosts}};
const preCosts   = {{.PreCosts}};
const memCosts   = {{.MemCosts}};

new Chart(document.getElementById('elapsedChart'), {
  type: 'line',
  data: {
    labels,
    datasets: [{
      label: 'Elapsed (ms)',
      data: elapsedMs,
      borderColor: '#20c997',
      backgroundColor: 'rgba(32,201,151,0.08)',
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
      y: { title: { display: true, text: 'ms' }, beginAtZero: true }
    }
  }
});

{{if eq .Target "zisk"}}
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
{{end}}
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
