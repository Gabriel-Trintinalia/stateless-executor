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
	"encoding/hex"
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

// CostReport holds the parsed COST DISTRIBUTION table for one block.
// ZisK populates Base/Main/Opcodes/Precompiles/Memory/Total (circuit trace cells).
// OpenVM populates Instructions/Total (retired instruction count).
type CostReport struct {
	Base         uint64
	Main         uint64
	Opcodes      uint64
	Precompiles  uint64
	Memory       uint64
	Total        uint64
	Instructions uint64 // OpenVM: retired instruction count (deterministic)
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
	// Block characteristics for correlation analysis.
	TxCount     int
	GasUsed     uint64
	LegacyTxs   int
	Eip1559Txs  int
	Eip2930Txs  int
	Eip4844Txs  int
	Eip7702Txs  int
	OutputHex string
}

var blockNumRe = regexp.MustCompile(`block_(\d+)`)

func main() {
	fixturesDir := flag.String("fixtures", "", "directory containing fixture JSON files (required)")
	elfPath := flag.String("elf", "", "path to the zesu-zkvm ELF binary (required)")
	targetFlag := flag.String("target", "zisk", "zkVM target: zisk or openvm")
	zkvmPath := flag.String("zkvmPath", "", "path to zkVM emulator binary (ziskemu for ZisK, zesu-openvm-runner for OpenVM)")
	jobs := flag.Int("jobs", 1, "number of parallel emulator runs")
	reportPath := flag.String("report", "bench_report.html", "output HTML report path")
	csvPath := flag.String("csv", "", "optional path to write per-block CSV (block_num,tx_count,gas_used,legacy,eip1559,eip2930,eip4844,eip7702,base,main,opcodes,precompiles,memory,total,elapsed_ms)")
	flag.Parse()

	if *fixturesDir == "" || *elfPath == "" {
		flag.Usage()
		os.Exit(1)
	}
	if *targetFlag != "zisk" && *targetFlag != "openvm" {
		log.Fatalf("unknown target %q: must be zisk or openvm", *targetFlag)
	}
	if *zkvmPath == "" {
		if *targetFlag == "openvm" {
			*zkvmPath = "zesu-openvm-runner"
		} else {
			*zkvmPath = "ziskemu"
		}
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
	log.Printf("found %d fixtures, running with %s/%s (%d job(s))...", len(paths), *targetFlag, *zkvmPath, *jobs)

	var runBench func(fixturePath string) (CostReport, string, string, error, bool, blockInfo)
	if *targetFlag == "openvm" {
		ep, zp := *elfPath, *zkvmPath
		runBench = func(p string) (CostReport, string, string, error, bool, blockInfo) {
			costs, execErr, out, err, ok := benchOneOpenVM(p, ep, zp)
			return costs, execErr, out, err, ok, blockInfo{}
		}
	} else {
		ep, zp := *elfPath, *zkvmPath
		runBench = func(p string) (CostReport, string, string, error, bool, blockInfo) {
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
			costs, execErr, errOut, runErr, expectedSuccess, bi := runBench(path)
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
				TxCount:         bi.TxCount,
				GasUsed:         bi.GasUsed,
				LegacyTxs:       bi.LegacyTxs,
				Eip1559Txs:      bi.Eip1559Txs,
				Eip2930Txs:      bi.Eip2930Txs,
				Eip4844Txs:      bi.Eip4844Txs,
				Eip7702Txs:      bi.Eip7702Txs,
				OutputHex:       bi.OutputHex,
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

	if *csvPath != "" {
		if err := writeCSV(*csvPath, good); err != nil {
			log.Fatalf("write csv: %v", err)
		}
		log.Printf("csv written to %s", *csvPath)
	}
}

// blockInfo carries per-block characteristics extracted from the fixture.
type blockInfo struct {
	TxCount    int
	GasUsed    uint64
	LegacyTxs  int
	Eip1559Txs int
	Eip2930Txs int
	Eip4844Txs int
	Eip7702Txs int
	OutputHex  string
}

func writeCSV(path string, results []BlockResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(f, "block_num,tx_count,gas_used,legacy,eip1559,eip2930,eip4844,eip7702,base,main,opcodes,precompiles,memory,total,elapsed_ms")
	for _, r := range results {
		fmt.Fprintf(f, "%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n",
			r.BlockNum,
			r.TxCount,
			r.GasUsed,
			r.LegacyTxs,
			r.Eip1559Txs,
			r.Eip2930Txs,
			r.Eip4844Txs,
			r.Eip7702Txs,
			r.Costs.Base,
			r.Costs.Main,
			r.Costs.Opcodes,
			r.Costs.Precompiles,
			r.Costs.Memory,
			r.Costs.Total,
			r.Elapsed.Milliseconds(),
		)
	}
	return nil
}

// benchOne returns (costs, execError, rawOutput, error, expectedSuccess, blockInfo) for a ZisK run.
func benchOne(fixturePath, elfPath, zkvmPath string) (CostReport, string, string, error, bool, blockInfo) {
	f, err := fixture.LoadFile(fixturePath)
	if err != nil {
		return CostReport{}, "", "", fmt.Errorf("load: %w", err), false, blockInfo{}
	}

	bi := extractBlockInfo(f)

	input, err := fixture.ZesuInputSSZ(f)
	if err != nil {
		return CostReport{}, "", "", fmt.Errorf("encode: %w", err), f.Success, bi
	}

	tmp, err := os.CreateTemp("", "zesu-bench-*.bin")
	if err != nil {
		return CostReport{}, "", "", err, f.Success, bi
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(input); err != nil {
		tmp.Close()
		return CostReport{}, "", "", err, f.Success, bi
	}
	if err := tmp.Close(); err != nil {
		return CostReport{}, "", "", err, f.Success, bi
	}

	outFile, err := os.CreateTemp("", "zesu-bench-out-*.bin")
	if err != nil {
		return CostReport{}, "", "", err, f.Success, bi
	}
	outPath := outFile.Name()
	outFile.Close()
	defer os.Remove(outPath)

	out, err := exec.Command(zkvmPath, "-X", "-e", elfPath, "-i", tmp.Name(), "-o", outPath).
		CombinedOutput()
	rawOut := strings.TrimSpace(string(out))
	if err != nil {
		return CostReport{}, "", rawOut, fmt.Errorf("zkvm: %w", err), f.Success, bi
	}

	costs, ok := parseCostReport(rawOut)
	if !ok {
		return CostReport{}, "", rawOut, fmt.Errorf("no COST DISTRIBUTION in ziskemu output"), f.Success, bi
	}
	execErr := parseExecError(rawOut)

	if outBytes, err2 := os.ReadFile(outPath); err2 == nil {
		bi.OutputHex = hex.EncodeToString(outBytes)
	}

	return costs, execErr, rawOut, nil, f.Success, bi
}

func extractBlockInfo(f *fixture.FixtureFile) blockInfo {
	bi := blockInfo{}
	txs := f.StatelessInput.Block.Body.Transactions
	bi.TxCount = len(txs)
	for _, tx := range txs {
		switch {
		case tx.Transaction["eip1559"] != nil:
			bi.Eip1559Txs++
		case tx.Transaction["eip4844"] != nil:
			bi.Eip4844Txs++
		case tx.Transaction["eip2930"] != nil:
			bi.Eip2930Txs++
		case tx.Transaction["eip7702"] != nil:
			bi.Eip7702Txs++
		default:
			bi.LegacyTxs++
		}
	}
	bi.GasUsed = f.StatelessInput.Block.Header.GasUsed
	return bi
}

// benchOneOpenVM returns (costs, execError, rawOutput, error, expectedSuccess) for an OpenVM run.
// costs is always zero — OpenVM emulation does not produce a circuit cost breakdown.
// execError is "ExecutionFailed" when the guest writes success=0 to public values byte[32].
func benchOneOpenVM(fixturePath, elfPath, zkvmPath string) (CostReport, string, string, error, bool) {
	f, err := fixture.LoadFile(fixturePath)
	if err != nil {
		return CostReport{}, "", "", fmt.Errorf("load: %w", err), false
	}

	input, err := fixture.ZesuInputSSZ(f)
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

	out, err := exec.Command(zkvmPath, "-X", "-e", elfPath, "-i", tmpIn.Name(), "-o", tmpOutPath).
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
	costs, _ := parseCostReport(rawOut)
	return costs, execErr, rawOut, nil, f.Success
}

var execFailedRe = regexp.MustCompile(`error: execution failed: (\S+)`)

func parseExecError(output string) string {
	m := execFailedRe.FindStringSubmatch(output)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// parseCostReport parses a COST DISTRIBUTION table from combined runner output.
// Handles both ZisK (BASE/MAIN/OPCODES/PRECOMPILES/MEMORY/TOTAL) and
// OpenVM (ELAPSED_MS/TOTAL) table formats.
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
		} else if v, ok := parseCostLine(line, "INSTRUCTIONS"); ok {
			r.Instructions = v
			found = true
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
		insns := make([]uint64, len(good))
		for i, r := range good {
			insns[i] = r.Costs.Instructions
		}
		s := computeStats(insns)
		fmt.Printf("%-14s %18s %18s %18s %18s\n", "INSTRUCTIONS", "MIN", "P50", "MAX", "AVG")
		fmt.Printf("%s\n", strings.Repeat("-", 92))
		fmt.Printf("%-14s %18d %18d %18d %18d\n", "INSTRUCTIONS", s.Min, s.P50, s.Max, s.Avg)
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
	Generated         string
	Target            string
	Total             int
	Good              int
	Failed            int
	ValidationFailed  int
	StatRows          []statRow
	Labels            template.JS
	ElapsedMs         template.JS // outer wall-clock ms per block (both targets)
	InstructionCounts template.JS // retired instruction count per block (OpenVM only)
	TotalCosts        template.JS // ZisK only
	BaseCosts         template.JS
	MainCosts         template.JS
	OpCosts           template.JS
	PreCosts          template.JS
	MemCosts          template.JS
	ExecFailed        []execFailedRow
	ValidationFails   []validationFailRow
	Errors            []errorRow
	RawBlocks         []rawBlockRow
}

type rawBlockRow struct {
	BlockNum       uint64
	TxCount        int
	GasUsed        uint64
	Base           uint64
	Main           uint64
	Opcodes        uint64
	Precompiles    uint64
	Memory         uint64
	Total          uint64
	PayloadRoot    string // hex of out[0:32]: new_payload_request_root
	Success        bool   // out[32]: 0x01 = valid
	ChainConfigHex string // hex of out[33:105]: SszChainConfig
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
	instructionCounts := make([]uint64, len(good))
	for i, r := range good {
		blockNums[i] = r.BlockNum
		elapsedMs[i] = uint64(r.Elapsed.Milliseconds())
		instructionCounts[i] = r.Costs.Instructions
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
		insns := make([]uint64, len(good))
		for i, r := range good {
			insns[i] = r.Costs.Instructions
		}
		s := computeStats(insns)
		statRows = []statRow{{Label: "INSTRUCTIONS", Min: s.Min, P50: s.P50, Max: s.Max, Avg: s.Avg}}
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

	rawBlocks := make([]rawBlockRow, len(good))
	for i, r := range good {
		row := rawBlockRow{
			BlockNum:    r.BlockNum,
			TxCount:     r.TxCount,
			GasUsed:     r.GasUsed,
			Base:        r.Costs.Base,
			Main:        r.Costs.Main,
			Opcodes:     r.Costs.Opcodes,
			Precompiles: r.Costs.Precompiles,
			Memory:      r.Costs.Memory,
			Total:       r.Costs.Total,
		}
		if b, err := hex.DecodeString(r.OutputHex); err == nil && len(b) >= 105 {
			row.PayloadRoot = hex.EncodeToString(b[0:32])
			row.Success = b[32] == 0x01
			row.ChainConfigHex = hex.EncodeToString(b[33:105])
		}
		rawBlocks[i] = row
	}

	data := reportData{
		Generated:         time.Now().Format(time.RFC1123),
		Target:            target,
		Total:             total,
		Good:              len(good),
		Failed:            len(execFailedRows),
		ValidationFailed:  len(validationFailRows),
		StatRows:          statRows,
		Labels:            toJS(blockNums),
		ElapsedMs:         toJS(elapsedMs),
		InstructionCounts: toJS(instructionCounts),
		TotalCosts:        toJS(extract(func(c CostReport) uint64 { return c.Total })),
		BaseCosts:         toJS(extract(func(c CostReport) uint64 { return c.Base })),
		MainCosts:         toJS(extract(func(c CostReport) uint64 { return c.Main })),
		OpCosts:           toJS(extract(func(c CostReport) uint64 { return c.Opcodes })),
		PreCosts:          toJS(extract(func(c CostReport) uint64 { return c.Precompiles })),
		MemCosts:          toJS(extract(func(c CostReport) uint64 { return c.Memory })),
		ExecFailed:        execFailedRows,
		ValidationFails:   validationFailRows,
		Errors:            errRows,
		RawBlocks:         rawBlocks,
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
  tr.row-fail { background: #fff0f0 !important; border-left: 3px solid #dc3545; }
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

{{if eq .Target "openvm"}}
<h2>Instruction Count by Block</h2>
<div class="chart-wrap"><canvas id="instructionChart"></canvas></div>
{{end}}

<h2>Wall-Clock Time by Block (ms)</h2>
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

{{if .RawBlocks}}
<h2>Raw Block Data</h2>
<input id="rawSearch" type="text" placeholder="Filter by block number..." style="margin-bottom:.5rem;padding:.3rem .6rem;font-size:.9rem;border:1px solid #dee2e6;border-radius:4px;width:220px">
<label style="margin-left:.75rem;font-size:.9rem;cursor:pointer"><input type="checkbox" id="failOnly" style="margin-right:.3rem">Failures only</label>
<div style="overflow-x:auto;max-width:100%">
<table id="rawTable" style="font-size:.82rem;min-width:900px">
  <thead>
  <tr>
    <th onclick="sortTable(0)" style="cursor:pointer;white-space:nowrap">Block ↕</th>
    <th onclick="sortTable(1)" style="cursor:pointer">TxCount ↕</th>
    <th onclick="sortTable(2)" style="cursor:pointer">GasUsed ↕</th>
    <th onclick="sortTable(3)" style="cursor:pointer">Base ↕</th>
    <th onclick="sortTable(4)" style="cursor:pointer">Main ↕</th>
    <th onclick="sortTable(5)" style="cursor:pointer">Opcodes ↕</th>
    <th onclick="sortTable(6)" style="cursor:pointer">Precompiles ↕</th>
    <th onclick="sortTable(7)" style="cursor:pointer">Memory ↕</th>
    <th onclick="sortTable(8)" style="cursor:pointer">Total ↕</th>
    <th>Success</th>
    <th>PayloadRoot</th>
  </tr>
  </thead>
  <tbody id="rawBody">
  {{range .RawBlocks}}
  <tr{{if not .Success}} class="row-fail"{{end}}>
    <td style="font-family:monospace">{{.BlockNum}}</td>
    <td>{{.TxCount}}</td>
    <td>{{.GasUsed}}</td>
    <td>{{.Base}}</td>
    <td>{{.Main}}</td>
    <td>{{.Opcodes}}</td>
    <td>{{.Precompiles}}</td>
    <td>{{.Memory}}</td>
    <td>{{.Total}}</td>
    <td style="text-align:center">{{if .Success}}<span style="color:#198754">✓</span>{{else}}<span style="color:#dc3545">✗</span>{{end}}</td>
    <td style="font-family:monospace;font-size:.72rem;white-space:nowrap" title="{{.PayloadRoot}}">0x{{.PayloadRoot}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
</div>
{{end}}

<script>
const labels             = {{.Labels}};
const elapsedMs          = {{.ElapsedMs}};
const instructionCounts  = {{.InstructionCounts}};
const totalCosts         = {{.TotalCosts}};
const baseCosts  = {{.BaseCosts}};
const mainCosts  = {{.MainCosts}};
const opCosts    = {{.OpCosts}};
const preCosts   = {{.PreCosts}};
const memCosts   = {{.MemCosts}};

{{if eq .Target "openvm"}}
new Chart(document.getElementById('instructionChart'), {
  type: 'line',
  data: {
    labels,
    datasets: [{
      label: 'Instructions',
      data: instructionCounts,
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
      y: { title: { display: true, text: 'Retired Instructions' }, beginAtZero: true }
    }
  }
});
{{end}}

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

let sortDir = {};
function sortTable(col) {
  const tbody = document.getElementById('rawBody');
  if (!tbody) return;
  const rows = Array.from(tbody.rows);
  const asc = !sortDir[col];
  sortDir = {};
  sortDir[col] = asc;
  rows.sort((a, b) => {
    const av = a.cells[col].textContent.trim();
    const bv = b.cells[col].textContent.trim();
    const an = parseFloat(av.replace(/,/g,'')), bn = parseFloat(bv.replace(/,/g,''));
    if (!isNaN(an) && !isNaN(bn)) return asc ? an - bn : bn - an;
    return asc ? av.localeCompare(bv) : bv.localeCompare(av);
  });
  rows.forEach(r => tbody.appendChild(r));
}
const rawSearch = document.getElementById('rawSearch');
const failOnly = document.getElementById('failOnly');
function applyRawFilters() {
  const q = rawSearch ? rawSearch.value.trim() : '';
  const fo = failOnly ? failOnly.checked : false;
  Array.from(document.getElementById('rawBody').rows).forEach(r => {
    const blockMatch = r.cells[0].textContent.includes(q);
    const failMatch = !fo || r.classList.contains('row-fail');
    r.style.display = (blockMatch && failMatch) ? '' : 'none';
  });
}
if (rawSearch) rawSearch.addEventListener('input', applyRawFilters);
if (failOnly) failOnly.addEventListener('change', applyRawFilters);

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
