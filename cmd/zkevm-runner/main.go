// Command zkevm-runner runs zkevm blockchain test fixtures through zesu-zkvm,
// reporting pass/fail for each test case.
//
// Usage:
//
//	zkevm-runner --fixtures <dir> --elf <path> [--ziskemu <path>] [--jobs N]
package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

// TestResult holds the outcome of running one zkevm test block.
type TestResult struct {
	Name              string
	Network           string
	ExpectedSuccess   bool
	GotSuccess        bool
	ExpectedOutputHex string
	GotOutputHex      string
	OutputMatch       bool
	ValidationOK      bool
	Err               error
	ErrOutput         string
	Elapsed           time.Duration
}

func main() {
	fixturesDir := flag.String("fixtures", "", "directory containing zkevm blockchain test JSON files (required)")
	elfPath := flag.String("elf", "", "path to the zesu-zkvm ELF binary (required unless -dump-dir is set)")
	ziskemuPath := flag.String("ziskemu", "ziskemu", "path to ziskemu binary (zisk-0.17+)")
	jobs := flag.Int("jobs", 1, "number of parallel ziskemu runs")
	reportPath := flag.String("report", "", "output HTML report path (omit to skip)")
	dumpDir := flag.String("dump-dir", "", "if set, write encoded .bin input files here instead of running ziskemu")
	flag.Parse()

	if *fixturesDir == "" {
		flag.Usage()
		os.Exit(1)
	}
	if *dumpDir == "" && *elfPath == "" {
		log.Fatalf("-elf is required when -dump-dir is not set")
	}
	if *elfPath != "" {
		if _, err := os.Stat(*elfPath); err != nil {
			log.Fatalf("ELF not found at %s: %v", *elfPath, err)
		}
	}

	paths, err := collectJSON(*fixturesDir)
	if err != nil {
		log.Fatalf("collect fixtures: %v", err)
	}
	if len(paths) == 0 {
		log.Fatalf("no JSON fixtures found in %s", *fixturesDir)
	}
	if *dumpDir != "" {
		if err := os.MkdirAll(*dumpDir, 0o755); err != nil {
			log.Fatalf("create dump-dir: %v", err)
		}
		log.Printf("found %d fixture files, dumping .bin inputs to %s...", len(paths), *dumpDir)
	} else {
		log.Printf("found %d fixture files, running with ziskemu (%d job(s))...", len(paths), *jobs)
	}

	// Each JSON file may contain multiple test cases, each with multiple blocks.
	// Expand to individual (file, testcase, blockidx) work items.
	type workItem struct {
		path    string
		tc      *fixture.ZkevmTestCase
		block   *fixture.ZkevmBlock
		name    string
		network string
	}
	var items []workItem
	for _, p := range paths {
		tcs, err := fixture.LoadZkevmFile(p)
		if err != nil {
			log.Printf("SKIP %s: %v", p, err)
			continue
		}
		for _, tc := range tcs {
			for bi := range tc.Blocks {
				name := tc.Name
				if len(tc.Blocks) > 1 {
					name = fmt.Sprintf("%s/block%d", tc.Name, bi)
				}
				items = append(items, workItem{p, tc, &tc.Blocks[bi], name, tc.Network})
			}
		}
	}
	if len(items) == 0 {
		log.Fatalf("no test blocks found")
	}
	log.Printf("expanded to %d test blocks", len(items))

	var logMu sync.Mutex

	results := make([]TestResult, len(items))
	sem := make(chan struct{}, *jobs)
	var wg sync.WaitGroup
	var done atomic.Int64

	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, it workItem) {
			defer wg.Done()
			defer func() { <-sem }()

			t := time.Now()
			var gotSuccess bool
			var gotOutputHex string
			var rawOut string
			var runErr error
			if *dumpDir != "" {
				runErr = dumpOne(it.tc, it.block, idx, *dumpDir)
			} else {
				gotSuccess, gotOutputHex, rawOut, runErr = runOne(it.tc, it.block, *elfPath, *ziskemuPath)
			}
			elapsed := time.Since(t)
			expectedOutputHex := strings.ToLower(strings.TrimPrefix(it.block.StatelessOutputBytes, "0x"))
			// ziskemu's -o writes the full output region (zero-padded), so trim got to expected's length.
			gotOutputCmp := gotOutputHex
			if len(expectedOutputHex) > 0 && len(gotOutputCmp) > len(expectedOutputHex) {
				gotOutputCmp = gotOutputCmp[:len(expectedOutputHex)]
			}
			// bal-devnet-7 / zkevm@v0.4.1: expectedSuccess comes from the fixture's
			// SszStatelessValidationResult byte 32 (successful_validation), NOT from
			// the block-level expectException field. Reason: expectException asserts
			// the block is invalid at some layer, but the t8n pipeline's stateless
			// serializer still produces successful_validation=01 for several
			// BlockException classes (e.g. INVALID_BLOCK_ACCESS_LIST, INVALID_REQUESTS)
			// where t8n built the block successfully even though the block is
			// invalid. Trusting the fixture's serialized byte aligns the runner with
			// what a correct stateless verifier (zesu) should emit.
			expectedSuccess := len(expectedOutputHex) < 66 || expectedOutputHex[64:66] != "00"
			outputMatch := expectedOutputHex == "" || gotOutputCmp == expectedOutputHex
			// SSZ-output match is the authoritative pass/fail signal. We do NOT
			// consult ExpectException: for fixtures built with Block.rlp_modifier
			// the corruption only exists in the RLP block, while statelessInputBytes
			// (and statelessOutputBytes) still describe the canonical pre-modifier
			// block — so a spec-compliant guest correctly returns successful_validation
			// for those inputs even though ExpectException is set. See the EELS
			// fixture-format issue for the full story.
			validationOK := *dumpDir != "" || (runErr == nil && outputMatch)

			n := done.Add(1)
			status := "OK"
			if isSkipped(runErr) {
				status = fmt.Sprintf("SKIP: %v", runErr)
			} else if runErr != nil {
				status = fmt.Sprintf("ERROR: %v", runErr)
			} else if !validationOK {
				status = "VALIDATION FAILED (output mismatch)"
			}
			fmt.Printf("[%3d/%d] %s  [%s]  %s  (%s)\n",
				n, len(items), truncateName(it.name), it.network, status, elapsed.Round(time.Millisecond))
			if expectedOutputHex != "" && !outputMatch {
				fmt.Printf("         expected: %s\n", expectedOutputHex)
			}
			if rawOut != "" {
				logMu.Lock()
				fmt.Printf("%s\n", filterUARTLog(rawOut))
				logMu.Unlock()
			}

			results[idx] = TestResult{
				Name:              it.name,
				Network:           it.network,
				ExpectedSuccess:   expectedSuccess,
				GotSuccess:        gotSuccess,
				ExpectedOutputHex: expectedOutputHex,
				GotOutputHex:      gotOutputHex,
				OutputMatch:       outputMatch,
				ValidationOK:      validationOK,
				Err:               runErr,
				ErrOutput:         rawOut,
				Elapsed:           elapsed,
			}
		}(i, item)
	}
	wg.Wait()

	printSummary(results)

	if *reportPath != "" {
		if err := writeReport(*reportPath, results); err != nil {
			log.Fatalf("write report: %v", err)
		}
		log.Printf("report written to %s", *reportPath)
	}
}

// dumpOne encodes one zkevm block and writes it to dir/<sanitized_name>_<idx>.bin.
func dumpOne(tc *fixture.ZkevmTestCase, block *fixture.ZkevmBlock, idx int, dir string) error {
	input, _, err := fixture.ZesuInputFromZkevmBlock(tc, block)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	// Sanitize test name to a safe filename using the last path component.
	base := tc.Name
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	safe := strings.NewReplacer(":", "_", "[", "_", "]", "", " ", "_").Replace(base)
	if len(safe) > 80 {
		safe = safe[:80]
	}
	dest := filepath.Join(dir, fmt.Sprintf("%s_%d.bin", safe, idx))
	return os.WriteFile(dest, input, 0o644)
}

// runOne encodes and executes one zkevm block. Returns (gotSuccess, outputHex, rawOutput, error).
func runOne(tc *fixture.ZkevmTestCase, block *fixture.ZkevmBlock, elfPath, ziskemuPath string) (bool, string, string, error) {
	input, _, err := fixture.ZesuInputFromZkevmBlock(tc, block)
	if err != nil {
		return false, "", "", fmt.Errorf("encode: %w", err)
	}

	in, err := os.CreateTemp("", "zkevm-runner-in-*.bin")
	if err != nil {
		return false, "", "", err
	}
	defer os.Remove(in.Name())
	if _, err := in.Write(input); err != nil {
		in.Close()
		return false, "", "", err
	}
	if err := in.Close(); err != nil {
		return false, "", "", err
	}

	out, err := os.CreateTemp("", "zkevm-runner-out-*.bin")
	if err != nil {
		return false, "", "", err
	}
	defer os.Remove(out.Name())
	out.Close()

	cmd, err := exec.Command(ziskemuPath, "-X", "-e", elfPath, "-i", in.Name(), "-o", out.Name()).
		CombinedOutput()
	rawOut := strings.TrimSpace(string(cmd))
	if err != nil {
		return false, "", rawOut, fmt.Errorf("zkvm: %w", err)
	}

	// success=1 in the zesu UART log means EVM execution succeeded.
	// success=0 means the block was invalid (ziskemu still exits 0).
	gotSuccess := strings.Contains(rawOut, "success=1") && !strings.Contains(rawOut, "success=0")

	// Read the 41-byte SszStatelessValidationResult that the guest wrote to the output region.
	outputBytes, err := os.ReadFile(out.Name())
	if err != nil {
		return gotSuccess, "", rawOut, nil
	}
	return gotSuccess, hex.EncodeToString(outputBytes), rawOut, nil
}

// filterUARTLog keeps only the key info lines from ziskemu UART output.
func filterUARTLog(rawOut string) string {
	var kept []string
	for _, line := range strings.Split(rawOut, "\n") {
		t := strings.TrimSpace(line)
		if strings.Contains(t, "input_len=") ||
			strings.Contains(t, "block=") ||
			strings.Contains(t, "root:") ||
			strings.Contains(t, "output:") ||
			t == "info: ok" {
			kept = append(kept, t)
		}
	}
	return strings.Join(kept, "\n")
}

func printSummary(results []TestResult) {
	var passed, failed, errored, skipped int
	var failures []TestResult
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	for _, r := range results {
		if isSkipped(r.Err) {
			skipped++
		} else if r.Err != nil {
			errored++
		} else if r.ValidationOK {
			passed++
		} else {
			failed++
			failures = append(failures, r)
		}
	}
	total := len(results)
	fmt.Printf("\n=== SUMMARY: %d/%d passed, %d validation failures, %d errors, %d skipped ===\n",
		passed, total, failed, errored, skipped)
	if len(failures) > 0 {
		fmt.Println("\nValidation failures:")
		for _, r := range failures {
			fmt.Printf("  %-50s  expected=%v got=%v\n", r.Name, r.ExpectedSuccess, r.GotSuccess)
		}
	}
}

// ── HTML report ───────────────────────────────────────────────────────────────

type reportData struct {
	Generated string
	Total     int
	Passed    int
	Failed    int
	Errored   int
	Skipped   int
	Failures  []failureRow
	Errors    []errorRow
	Skips     []errorRow
}

type failureRow struct {
	Name              string
	Network           string
	ExpectedSuccess   bool
	GotSuccess        bool
	ExpectedOutputHex string
	GotOutputHex      string
	OutputMatch       bool
	Elapsed           string
	ErrLine           string
}

type errorRow struct {
	Name    string
	Network string
	ErrMsg  string
	Output  string
}

func writeReport(path string, results []TestResult) error {
	var failures []failureRow
	var errors []errorRow
	var skips []errorRow
	passed, errored, skipped := 0, 0, 0

	for _, r := range results {
		if isSkipped(r.Err) {
			skipped++
			skips = append(skips, errorRow{
				Name:    r.Name,
				Network: r.Network,
				ErrMsg:  r.Err.Error(),
				Output:  r.ErrOutput,
			})
		} else if r.Err != nil {
			errored++
			errors = append(errors, errorRow{
				Name:    r.Name,
				Network: r.Network,
				ErrMsg:  r.Err.Error(),
				Output:  r.ErrOutput,
			})
		} else if !r.ValidationOK {
			// Extract the specific error line from ziskemu output (e.g. "error: execution failed: Foo").
			errLine := ""
			for _, line := range strings.Split(r.ErrOutput, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "error:") {
					errLine = strings.TrimSpace(line)
					break
				}
			}
			failures = append(failures, failureRow{
				Name:              r.Name,
				Network:           r.Network,
				ExpectedSuccess:   r.ExpectedSuccess,
				GotSuccess:        r.GotSuccess,
				ExpectedOutputHex: r.ExpectedOutputHex,
				GotOutputHex:      r.GotOutputHex,
				OutputMatch:       r.OutputMatch,
				Elapsed:           r.Elapsed.Round(time.Millisecond).String(),
				ErrLine:           errLine,
			})
		} else {
			passed++
		}
	}

	sort.Slice(failures, func(i, j int) bool { return failures[i].Name < failures[j].Name })
	sort.Slice(errors, func(i, j int) bool { return errors[i].Name < errors[j].Name })
	sort.Slice(skips, func(i, j int) bool { return skips[i].Name < skips[j].Name })

	data := reportData{
		Generated: time.Now().Format(time.RFC1123),
		Total:     len(results),
		Passed:    passed,
		Failed:    len(failures),
		Errored:   errored,
		Skipped:   skipped,
		Failures:  failures,
		Errors:    errors,
		Skips:     skips,
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
<title>zkevm-runner Report</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 1rem 1.5rem; background: #f8f9fa; color: #212529; font-size: .9rem; }
  h1 { font-size: 1.3rem; margin-bottom: .25rem; }
  .meta { color: #6c757d; font-size: .8rem; margin-bottom: .75rem; }
  .pill { display: inline-block; padding: .15rem .5rem; border-radius: 1rem; font-size: .78rem; font-weight: 600; margin-right: .3rem; }
  .pill-green { background: #d1e7dd; color: #0a3622; }
  .pill-red   { background: #f8d7da; color: #58151c; }
  .pill-orange{ background: #fff3cd; color: #664d03; }
  .pill-grey  { background: #e2e3e5; color: #41464b; }
  table { border-collapse: collapse; width: 100%; margin-bottom: 1rem; }
  th, td { border: 1px solid #dee2e6; padding: .2rem .5rem; text-align: left; vertical-align: top; font-size: .8rem; }
  thead th { background: #343a40; color: #fff; font-size: .78rem; }
  tbody tr:nth-child(even) { background: #f1f3f5; }
  .mono { font-family: monospace; font-size: .78rem; }
  .tag-network { display:inline-block; padding:.05rem .3rem; border-radius:.3rem; background:#cfe2ff; color:#084298; font-size:.72rem; }
  .tag-pass  { color: #0a3622; }
  .tag-fail  { color: #58151c; }
  pre { margin:.2rem 0; padding:.3rem .5rem; background:#f1f3f5; border-radius:4px; overflow-x:auto; font-size:.75rem; white-space:pre-wrap; word-break:break-all; }
  details summary { cursor: pointer; }
  .tabs { display: flex; gap: 0; border-bottom: 2px solid #dee2e6; margin-top: 1rem; }
  .tab-btn { padding: .35rem .9rem; border: 1px solid transparent; border-bottom: none;
             background: none; cursor: pointer; font-size: .85rem; font-weight: 600;
             color: #6c757d; border-radius: .3rem .3rem 0 0; }
  .tab-btn:hover { background: #e9ecef; color: #212529; }
  .tab-btn.active { background: #fff; border-color: #dee2e6; color: #212529; margin-bottom: -2px; }
  .tab-panel { display: none; padding-top: .6rem; }
  .tab-panel.active { display: block; }
</style>
</head>
<body>
<h1>zkevm-runner Report</h1>
<p class="meta">Generated: {{.Generated}}</p>

<p>
  <span class="pill pill-grey">{{.Total}} total</span>
  <span class="pill pill-green">{{.Passed}} passed</span>
  {{if .Failed}}<span class="pill pill-red">{{.Failed}} validation failures</span>{{end}}
  {{if .Errored}}<span class="pill pill-orange">{{.Errored}} errors</span>{{end}}
  {{if .Skipped}}<span class="pill pill-grey">{{.Skipped}} skipped</span>{{end}}
</p>

{{if and (eq .Failed 0) (eq .Errored 0) (eq .Skipped 0)}}
<p style="color:#0a3622;font-weight:600">All {{.Total}} tests passed.</p>
{{else}}
<div class="tabs">
  {{if .Failures}}<button class="tab-btn{{if .Failures}} active{{end}}" onclick="showTab('failures')">Validation Failures ({{.Failed}})</button>{{end}}
  {{if .Errors}}<button class="tab-btn{{if not .Failures}} active{{end}}" onclick="showTab('errors')">Errors ({{.Errored}})</button>{{end}}
  {{if .Skips}}<button class="tab-btn" onclick="showTab('skipped')">Skipped ({{.Skipped}})</button>{{end}}
</div>

{{if .Failures}}
<div id="tab-failures" class="tab-panel active">
<table>
  <thead><tr><th>Test</th><th>Network</th><th>Expected</th><th>Got</th><th>Output</th><th>Error</th><th>Time</th></tr></thead>
  <tbody>
  {{range .Failures}}
  <tr>
    <td class="mono">{{.Name}}</td>
    <td><span class="tag-network">{{.Network}}</span></td>
    <td>{{if .ExpectedSuccess}}<span class="tag-pass">✓ success</span>{{else}}<span class="tag-fail">✗ failure</span>{{end}}</td>
    <td>{{if .GotSuccess}}<span class="tag-pass">✓ success</span>{{else}}<span class="tag-fail">✗ failure</span>{{end}}</td>
    <td>{{if .OutputMatch}}<span class="tag-pass">✓ match</span>{{else}}<details><summary class="tag-fail mono">✗ mismatch</summary><pre>expected: {{.ExpectedOutputHex}}
got:      {{.GotOutputHex}}</pre></details>{{end}}</td>
    <td class="mono">{{.ErrLine}}</td>
    <td style="white-space:nowrap">{{.Elapsed}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
</div>
{{end}}

{{if .Errors}}
<div id="tab-errors" class="tab-panel{{if not .Failures}} active{{end}}">
<table>
  <thead><tr><th>Test</th><th>Network</th><th>Error</th></tr></thead>
  <tbody>
  {{range .Errors}}
  <tr>
    <td class="mono">{{.Name}}</td>
    <td><span class="tag-network">{{.Network}}</span></td>
    <td>
      <details>
        <summary class="mono">{{.ErrMsg}}</summary>
        <pre>{{.Output}}</pre>
      </details>
    </td>
  </tr>
  {{end}}
  </tbody>
</table>
</div>
{{end}}

{{if .Skips}}
<div id="tab-skipped" class="tab-panel">
<table>
  <thead><tr><th>Test</th><th>Network</th><th>Reason</th></tr></thead>
  <tbody>
  {{range .Skips}}
  <tr>
    <td class="mono">{{.Name}}</td>
    <td><span class="tag-network">{{.Network}}</span></td>
    <td class="mono">{{.ErrMsg}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
</div>
{{end}}
{{end}}

<script>
function showTab(id) {
  document.querySelectorAll('.tab-panel').forEach(p => p.classList.remove('active'));
  document.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
  document.getElementById('tab-' + id).classList.add('active');
  document.querySelectorAll('.tab-btn').forEach(b => {
    if (b.getAttribute('onclick') === "showTab('" + id + "')") b.classList.add('active');
  });
}
</script>
</body>
</html>
`))

func isSkipped(err error) bool {
	return errors.Is(err, fixture.ErrMissingStatelessInputBytes)
}

func truncateName(s string) string {
	const half = 40
	if len(s) <= half*2+3 {
		return s
	}
	return s[:half] + "..." + s[len(s)-half:]
}

func collectJSON(dir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".json") {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}
