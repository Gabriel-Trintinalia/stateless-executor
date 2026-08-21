// Command bench converts fixture JSON files to zesu-zkvm binary inputs and
// runs each one through a zkVM emulator, reporting statistics and generating
// an HTML report.
//
// Usage:
//
//	bench --fixtures <dir|file> --elf <path> [--target zisk|openvm] [--zkvmPath <path>]
//	      [--jobs N] [--report <path>] [--csv <path>] [--dry-run]
package main

import (
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

// BlockResult holds the outcome of running one fixture block.
type BlockResult struct {
	BlockNum        uint64
	Name            string
	Label           string // unit label: corpus file stem, or EEST test-case name[/blockN]
	Suite           string // fixture dir relative to the fixtures root
	Network         string // EEST only; "" for corpus
	Kind            fixture.Format
	Verdict         verdict
	Target          string // "zisk" or "openvm"
	Costs           CostReport
	Err             error
	ErrOutput       string
	ExecError       string
	Elapsed         time.Duration
	ExpectedSuccess bool
	ValidationOK    bool
	// Block characteristics for correlation analysis.
	TxCount    int
	GasUsed    uint64
	LegacyTxs  int
	Eip1559Txs int
	Eip2930Txs int
	Eip4844Txs int
	Eip7702Txs int
	OutputHex  string
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
	dryRun := flag.Bool("dry-run", false, "discover fixtures, print the per-format census, and exit without running the emulator")
	maxSteps := flag.Uint64("maxSteps", 0, "emulator step cap, passed as -n; 0 leaves the emulator on its default (68719476735)")
	flag.Parse()

	if *fixturesDir == "" {
		flag.Usage()
		os.Exit(1)
	}
	// --dry-run touches no emulator, so it does not need an ELF.
	if *elfPath == "" && !*dryRun {
		flag.Usage()
		os.Exit(1)
	}
	if *targetFlag != "zisk" && *targetFlag != "openvm" {
		log.Fatalf("unknown target %q: must be zisk or openvm", *targetFlag)
	}
	if *jobs < 1 {
		log.Fatalf("--jobs must be at least 1, got %d", *jobs)
	}
	if *zkvmPath == "" {
		if *targetFlag == "openvm" {
			*zkvmPath = "zesu-openvm-runner"
		} else {
			*zkvmPath = "ziskemu"
		}
	}
	if !*dryRun {
		if _, err := os.Stat(*elfPath); err != nil {
			log.Fatalf("ELF not found at %s: %v", *elfPath, err)
		}
	}

	jobsFound, skipped, err := discover(*fixturesDir)
	if err != nil {
		log.Fatalf("collect fixtures: %v", err)
	}
	if *dryRun {
		printCensus(census{Jobs: jobsFound, Skipped: skipped})
		return
	}
	for _, s := range skipped {
		log.Printf("SKIP %s: %s", s.Path, s.Reason)
	}
	if len(jobsFound) == 0 {
		log.Fatalf("no runnable JSON fixtures found in %s (%d skipped)", *fixturesDir, len(skipped))
	}

	// OpenVM's verdict comes from a fixed-offset read of the output region and
	// has no EEST equivalent, so refuse the combination up front rather than
	// mis-reporting thousands of units. The formats are known from discovery.
	if *targetFlag == "openvm" {
		for _, j := range jobsFound {
			if j.Kind != fixture.FormatCorpus {
				log.Fatalf("%s: --target openvm does not support %s fixtures", j.Path, j.Kind)
			}
		}
	}
	// A mixed run is well-defined but its summary statistics span two
	// incomparable populations, so warn rather than refuse.
	if hasKind(jobsFound, fixture.FormatCorpus) && hasKind(jobsFound, fixture.FormatZkevm) {
		log.Printf("WARNING: mixed corpus and zkevm fixtures — summary statistics span both and are not meaningful")
	}

	log.Printf("found %d fixtures, running with %s/%s (%d job(s))...", len(jobsFound), *targetFlag, *zkvmPath, *jobs)

	o := emuOpts{ELF: *elfPath, Bin: *zkvmPath, MaxSteps: *maxSteps}
	var run emuRunner
	if *targetFlag == "openvm" {
		run = func(input []byte) (emuResult, error) { return runOpenVM(o, input) }
	} else {
		run = func(input []byte) (emuResult, error) { return runEmu(o, input) }
	}

	results := runAll(jobsFound, run, *targetFlag, *jobs)
	if len(results) == 0 {
		log.Fatalf("no runnable units across %d file(s)", len(jobsFound))
	}

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
	sortResults(good)

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

// sortResults orders rows for the CSV and the report.
//
// Corpus rows sort by block number — not by label, which sorts wrong across the
// 8-to-9-digit boundary. EEST block numbers are all 1 or 2 within a fixture and
// so cannot order anything; those sort by (suite, label, block number).
//
// The sort is stable in both cases. With an unstable sort, EEST rows would come
// out in a different order on every run, destroying the A/B diffing the tool
// exists for.
func sortResults(rs []BlockResult) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Kind == fixture.FormatZkevm || b.Kind == fixture.FormatZkevm {
			if a.Suite != b.Suite {
				return a.Suite < b.Suite
			}
			if a.Label != b.Label {
				return a.Label < b.Label
			}
		}
		return a.BlockNum < b.BlockNum
	})
}

// csvHeader is the column order. New columns are appended rather than
// interleaved, so the first fifteen fields are unchanged and positional
// accessors into the archived runs ($14 total, $15 elapsed_ms) keep working.
var csvHeader = []string{
	"block_num", "tx_count", "gas_used",
	"legacy", "eip1559", "eip2930", "eip4844", "eip7702",
	"base", "main", "opcodes", "precompiles", "memory", "total", "elapsed_ms",
	"steps", "suite", "label",
}

// writeCSV uses encoding/csv because EEST labels contain commas, brackets and
// colons and have to be quoted. Numeric fields are never quoted, so corpus rows
// come out byte-identical to the hand-rolled Fprintf this replaces.
func writeCSV(path string, results []BlockResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write(csvHeader); err != nil {
		return err
	}
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	for _, r := range results {
		if err := w.Write([]string{
			u(r.BlockNum),
			strconv.Itoa(r.TxCount),
			u(r.GasUsed),
			strconv.Itoa(r.LegacyTxs),
			strconv.Itoa(r.Eip1559Txs),
			strconv.Itoa(r.Eip2930Txs),
			strconv.Itoa(r.Eip4844Txs),
			strconv.Itoa(r.Eip7702Txs),
			u(r.Costs.Base),
			u(r.Costs.Main),
			u(r.Costs.Opcodes),
			u(r.Costs.Precompiles),
			u(r.Costs.Memory),
			u(r.Costs.Total),
			strconv.FormatInt(r.Elapsed.Milliseconds(), 10),
			u(r.Costs.Steps),
			r.Suite,
			r.Label,
		}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
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
	Skipped           int
	Unverified        int
	IsEEST            bool   // the run contains zkevm fixtures
	UnitColumn        string // header for the first Raw Data column
	UnitAxis          string // chart x-axis title
	Scaled            bool   // too many units for per-unit charts
	ScaleNote         string
	RankCosts         template.JS // sorted total costs, rank on x
	SuiteLabels       template.JS
	SuiteMedians      template.JS
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
	Skips             []skipRow
	Unverifieds       []skipRow
	RawBlocks         []rawBlockRow
}

// skipRow backs both the Skipped and the Unverified tables: a unit that is
// neither a pass nor a failure, plus why.
type skipRow struct {
	Unit    string
	Suite   string
	Network string
	Reason  string
}

type rawBlockRow struct {
	// Unit is the first column: the EEST test label, or the corpus block
	// number rendered as-is so corpus reports are unchanged.
	Unit        string
	Suite       string
	Network     string
	BlockNum    uint64
	TxCount     int
	GasUsed     uint64
	Base        uint64
	Main        uint64
	Opcodes     uint64
	Precompiles uint64
	Memory      uint64
	Total       uint64
	// zkevm@v0.8.0: SszStatelessValidationResult is a flat 43 bytes —
	// root(32) ‖ valid(1) ‖ chain_id(8, LE) ‖ schema_id(2, LE).
	PayloadRoot string // hex of out[0:32]: new_payload_request_root
	Success     bool   // out[32]: 0x01 = valid
	ChainID     uint64 // out[33:41]
	SchemaID    uint16 // out[41:43]
}

type execFailedRow struct {
	BlockNum uint64
	Name     string
	Unit     string
	Reason   string
}

type validationFailRow struct {
	BlockNum        uint64
	Name            string
	Unit            string
	ExpectedSuccess bool
	ExecError       string
	Reason          string
}

type errorRow struct {
	BlockNum uint64
	Name     string
	Unit     string
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

// maxChartUnits is where per-unit charts stop being useful. A stacked bar with
// five datasets over 7,390 units hangs the browser, so above this the report
// switches to aggregate views.
const maxChartUnits = 1500

// toJS marshals chart data. Going through encoding/json rather than string
// concatenation is what makes EEST labels safe: they contain brackets, colons,
// commas and quotes.
func toJS(v any) template.JS {
	b, err := json.Marshal(v)
	// A nil slice marshals to "null", which would break every array method the
	// charts call on it.
	if err != nil || string(b) == "null" {
		return template.JS("[]")
	}
	return template.JS(b)
}

// unitCell is the first-column identity of a row: the EEST test label, or the
// corpus block number rendered exactly as it was before labels existed.
func unitCell(r BlockResult) string {
	if r.Kind == fixture.FormatZkevm {
		return r.Label
	}
	return strconv.FormatUint(r.BlockNum, 10)
}

// suiteMedians returns each suite and its median total cost, ordered by suite.
// For the benchmark fixtures that is the comparison that matters: the same
// workload at 10M, 30M and 60M gas.
func suiteMedians(good []BlockResult) ([]string, []uint64) {
	bySuite := map[string][]uint64{}
	for _, r := range good {
		bySuite[r.Suite] = append(bySuite[r.Suite], r.Costs.Total)
	}
	names := make([]string, 0, len(bySuite))
	for s := range bySuite {
		names = append(names, s)
	}
	sort.Strings(names)
	medians := make([]uint64, len(names))
	for i, s := range names {
		medians[i] = computeStats(bySuite[s]).P50
	}
	return names, medians
}

func writeReport(path string, good []BlockResult, all []BlockResult, target string) error {
	total := len(all)

	isEEST := false
	for _, r := range all {
		if r.Kind == fixture.FormatZkevm {
			isEEST = true
			break
		}
	}

	blockNums := make([]uint64, len(good))
	unitLabels := make([]string, len(good))
	elapsedMs := make([]uint64, len(good))
	instructionCounts := make([]uint64, len(good))
	for i, r := range good {
		blockNums[i] = r.BlockNum
		unitLabels[i] = r.Label
		elapsedMs[i] = uint64(r.Elapsed.Milliseconds())
		instructionCounts[i] = r.Costs.Instructions
	}

	// Chart labels: corpus keeps its numeric axis (marshalling []uint64 gives
	// exactly the array the old string concatenation produced), EEST gets
	// strings. Concatenating EEST names by hand would break the report outright,
	// since they contain brackets, colons, commas and quotes.
	var chartLabels template.JS
	if isEEST {
		chartLabels = toJS(unitLabels)
	} else {
		chartLabels = toJS(blockNums)
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
				Unit:     unitCell(r),
				Reason:   r.ExecError,
			})
		}
		if r.Err == nil && r.Verdict.Kind == verdictFail {
			validationFailRows = append(validationFailRows, validationFailRow{
				BlockNum:        r.BlockNum,
				Name:            r.Name,
				Unit:            unitCell(r),
				ExpectedSuccess: r.ExpectedSuccess,
				ExecError:       r.ExecError,
				Reason:          r.Verdict.Reason,
			})
		}
	}
	sort.SliceStable(execFailedRows, func(i, j int) bool { return execFailedRows[i].Unit < execFailedRows[j].Unit })
	sort.SliceStable(validationFailRows, func(i, j int) bool { return validationFailRows[i].Unit < validationFailRows[j].Unit })

	var errRows []errorRow
	var skipRows, unverifiedRows []skipRow
	for _, r := range all {
		if r.Err != nil {
			errRows = append(errRows, errorRow{
				BlockNum: r.BlockNum,
				Name:     r.Name,
				Unit:     unitCell(r),
				ErrMsg:   r.Err.Error(),
				Output:   r.ErrOutput,
			})
		}
		// Skipped and unverified units get their own tables rather than joining
		// the Errors table, so a mixed-fork tree does not render thousands of
		// red rows.
		switch r.Verdict.Kind {
		case verdictSkip:
			skipRows = append(skipRows, skipRow{unitCell(r), r.Suite, r.Network, r.Verdict.Reason})
		case verdictUnverified:
			unverifiedRows = append(unverifiedRows, skipRow{unitCell(r), r.Suite, r.Network, r.Verdict.Reason})
		}
	}
	sort.SliceStable(errRows, func(i, j int) bool { return errRows[i].Unit < errRows[j].Unit })
	sort.SliceStable(skipRows, func(i, j int) bool { return skipRows[i].Unit < skipRows[j].Unit })
	sort.SliceStable(unverifiedRows, func(i, j int) bool { return unverifiedRows[i].Unit < unverifiedRows[j].Unit })

	rawBlocks := make([]rawBlockRow, len(good))
	for i, r := range good {
		row := rawBlockRow{
			Unit:        unitCell(r),
			Suite:       r.Suite,
			Network:     r.Network,
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
		if b, err := hex.DecodeString(r.OutputHex); err == nil && len(b) >= 43 {
			row.PayloadRoot = hex.EncodeToString(b[0:32])
			row.Success = b[32] == 0x01
			row.ChainID = binary.LittleEndian.Uint64(b[33:41])
			row.SchemaID = binary.LittleEndian.Uint16(b[41:43])
		}
		rawBlocks[i] = row
	}

	unitColumn, unitAxis := "Block", "Block Number"
	if isEEST {
		unitColumn, unitAxis = "Test", "Test"
	}

	// Above maxChartUnits, per-unit charts are replaced by a rank-ordered cost
	// curve and a per-suite median bar. The swap is stated in the report rather
	// than left for the reader to infer from a missing chart.
	scaled := len(good) > maxChartUnits
	var (
		scaleNote  string
		rankCosts  []uint64
		suiteNames []string
		suiteMeds  []uint64
	)
	if scaled {
		scaleNote = fmt.Sprintf(
			"%d units exceeds the %d-unit per-unit chart limit; showing the cost distribution and per-suite medians instead.",
			len(good), maxChartUnits)
		rankCosts = extract(func(c CostReport) uint64 { return c.Total })
		sort.Slice(rankCosts, func(i, j int) bool { return rankCosts[i] < rankCosts[j] })
		suiteNames, suiteMeds = suiteMedians(good)
	}

	data := reportData{
		Generated:         time.Now().Format(time.RFC1123),
		Target:            target,
		Total:             total,
		Good:              len(good),
		Failed:            len(execFailedRows),
		ValidationFailed:  len(validationFailRows),
		StatRows:          statRows,
		Skipped:           len(skipRows),
		Unverified:        len(unverifiedRows),
		IsEEST:            isEEST,
		UnitColumn:        unitColumn,
		UnitAxis:          unitAxis,
		Scaled:            scaled,
		ScaleNote:         scaleNote,
		RankCosts:         toJS(rankCosts),
		SuiteLabels:       toJS(suiteNames),
		SuiteMedians:      toJS(suiteMeds),
		Labels:            chartLabels,
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
		Skips:             skipRows,
		Unverifieds:       unverifiedRows,
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
<p class="meta">Generated: {{.Generated}} &nbsp;|&nbsp; Blocks: {{.Good}}/{{.Total}} succeeded{{if .ValidationFailed}} &nbsp;|&nbsp; <span style="color:#dc3545">{{.ValidationFailed}} validation failure(s)</span>{{end}}{{if .Failed}} &nbsp;|&nbsp; {{.Failed}} expected failure(s){{end}}{{if .Unverified}} &nbsp;|&nbsp; {{.Unverified}} unverified{{end}}{{if .Skipped}} &nbsp;|&nbsp; {{.Skipped}} skipped{{end}}</p>

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

{{if .Scaled}}
<p class="meta">{{.ScaleNote}}</p>

<h2>Total Cost Distribution (sorted by rank)</h2>
<div class="chart-wrap"><canvas id="rankChart"></canvas></div>

<h2>Median Total Cost by Suite</h2>
<div class="chart-wrap"><canvas id="suiteChart"></canvas></div>
{{else}}
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
{{end}}

{{if .ValidationFails}}
<h2>Validation Failures ({{len .ValidationFails}} blocks)</h2>
<table id="validationFailTable">
  <thead><tr><th>{{.UnitColumn}}</th><th>Expected</th><th>Got</th></tr></thead>
  <tbody>
  {{range .ValidationFails}}
  <tr>
    <td style="white-space:nowrap;font-family:monospace">{{.Unit}}</td>
    <td>{{if .ExpectedSuccess}}success{{else}}failure{{end}}</td>
    <td style="color:#dc3545">{{if .Reason}}{{.Reason}}{{else}}{{if .ExecError}}failed: {{.ExecError}}{{else}}success{{end}}{{end}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
{{end}}

{{if .ExecFailed}}
<h2>Expected Failures ({{len .ExecFailed}} blocks)</h2>
<table id="execFailTable">
  <thead><tr><th>{{.UnitColumn}}</th><th>Reason</th></tr></thead>
  <tbody>
  {{range .ExecFailed}}
  <tr>
    <td style="white-space:nowrap;font-family:monospace">{{.Unit}}</td>
    <td style="font-family:monospace;color:#6c757d">{{.Reason}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
{{end}}

{{if .Errors}}
<h2>Errors ({{len .Errors}} blocks)</h2>
<table id="errTable">
  <thead><tr><th>{{.UnitColumn}}</th><th>Error</th></tr></thead>
  <tbody>
  {{range .Errors}}
  <tr>
    <td style="white-space:nowrap">{{.Unit}}</td>
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

{{if .Unverifieds}}
<h2>Unverified ({{len .Unverifieds}} units)</h2>
<p class="meta">Ran, but the fixture carries no expected output to compare against — neither a pass nor a failure.</p>
<table id="unverifiedTable" style="max-width:1100px">
  <thead><tr><th>{{.UnitColumn}}</th><th>Network</th><th>Reason</th></tr></thead>
  <tbody>
  {{range .Unverifieds}}
  <tr>
    <td style="font-family:monospace">{{.Unit}}</td>
    <td>{{.Network}}</td>
    <td style="color:#6c757d">{{.Reason}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
{{end}}

{{if .Skips}}
<h2>Skipped ({{len .Skips}} units)</h2>
<p class="meta">Not runnable by this tool — no stateless input in the fixture.</p>
<table id="skipTable" style="max-width:1100px">
  <thead><tr><th>{{.UnitColumn}}</th><th>Network</th><th>Reason</th></tr></thead>
  <tbody>
  {{range .Skips}}
  <tr>
    <td style="font-family:monospace">{{.Unit}}</td>
    <td>{{.Network}}</td>
    <td style="color:#6c757d">{{.Reason}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
{{end}}

{{if .RawBlocks}}
<h2>Raw Block Data</h2>
<input id="rawSearch" type="text" placeholder="Filter by {{if .IsEEST}}test name{{else}}block number{{end}}..." style="margin-bottom:.5rem;padding:.3rem .6rem;font-size:.9rem;border:1px solid #dee2e6;border-radius:4px;width:220px">
<label style="margin-left:.75rem;font-size:.9rem;cursor:pointer"><input type="checkbox" id="failOnly" style="margin-right:.3rem">Failures only</label>
<div style="overflow-x:auto;max-width:100%">
<table id="rawTable" style="font-size:.82rem;min-width:900px">
  <thead>
  <tr>
    <th onclick="sortTable(0)" style="cursor:pointer;white-space:nowrap">{{.UnitColumn}} ↕</th>
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
    <td style="font-family:monospace">{{.Unit}}</td>
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
const rankCosts          = {{.RankCosts}};
const suiteLabels        = {{.SuiteLabels}};
const suiteMedians       = {{.SuiteMedians}};
const elapsedMs          = {{.ElapsedMs}};
const instructionCounts  = {{.InstructionCounts}};
const totalCosts         = {{.TotalCosts}};
const baseCosts  = {{.BaseCosts}};
const mainCosts  = {{.MainCosts}};
const opCosts    = {{.OpCosts}};
const preCosts   = {{.PreCosts}};
const memCosts   = {{.MemCosts}};

{{if .Scaled}}
new Chart(document.getElementById('rankChart'), {
  type: 'line',
  data: {
    labels: rankCosts.map((_, i) => i + 1),
    datasets: [{
      label: 'TOTAL COST',
      data: rankCosts,
      borderColor: '#0d6efd',
      backgroundColor: 'rgba(13,110,253,0.08)',
      borderWidth: 1.5,
      pointRadius: 0,
      fill: true,
    }]
  },
  options: {
    responsive: true,
    plugins: { legend: { display: false } },
    scales: {
      x: { title: { display: true, text: 'Unit rank (cheapest to most expensive)' } },
      y: { title: { display: true, text: 'Cost' }, beginAtZero: true }
    }
  }
});

new Chart(document.getElementById('suiteChart'), {
  type: 'bar',
  data: {
    labels: suiteLabels,
    datasets: [{ label: 'Median TOTAL COST', data: suiteMedians, backgroundColor: '#198754' }]
  },
  options: {
    responsive: true,
    indexAxis: 'y',
    plugins: { legend: { display: false } },
    scales: { x: { title: { display: true, text: 'Cost' }, beginAtZero: true } }
  }
});
{{end}}

{{if and (not .Scaled) (eq .Target "openvm")}}
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

{{if not .Scaled}}
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
      x: { title: { display: true, text: '{{.UnitAxis}}' } },
      y: { title: { display: true, text: 'ms' }, beginAtZero: true }
    }
  }
});
{{end}}

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

{{if and (not .Scaled) (eq .Target "zisk")}}
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
