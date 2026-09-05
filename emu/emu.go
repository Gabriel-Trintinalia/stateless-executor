// Package emu runs a zesu-zkvm guest under a zkVM emulator and parses what it
// printed. It is the single place the emulator command line is built, shared by
// the bench and zkevm-runner tools.
package emu

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// DefaultZiskMaxSteps mirrors DEFAULT_MAX_STEPS_STR in zisk
// core/src/zisk_definitions.rs (2^36 - 1). ziskemu applies it whenever -n is not
// passed, so it is the cap a run is measured against when MaxSteps is 0.
const DefaultZiskMaxSteps uint64 = 68719476735

// StatelessOutputSize is the flat SszStatelessValidationResult length under
// zkevm@v0.8.0: root(32) ‖ valid(1) ‖ chain_id(8, LE) ‖ schema_id(2, LE).
const StatelessOutputSize = 43

// CostReport holds the parsed COST DISTRIBUTION table for one run.
// ZisK populates Base/Main/Opcodes/Precompiles/Memory/Total (circuit trace
// cells). OpenVM populates Instructions/Total (retired instruction count).
type CostReport struct {
	Base         uint64
	Main         uint64
	Opcodes      uint64
	Precompiles  uint64
	Memory       uint64
	Total        uint64
	Instructions uint64 // OpenVM: retired instruction count (deterministic)
	Steps        uint64 // ZisK: emulated steps; compared against the step cap
}

// Opts describes one emulator invocation.
type Opts struct {
	ELF string
	Bin string // ziskemu, or a compatible emulator
	// MaxSteps is passed as -n when non-zero. Zero means "do not pass -n",
	// leaving the emulator on its own default; cap detection then measures
	// against DefaultZiskMaxSteps.
	MaxSteps uint64
	// RequireCosts makes a run that produced no cost table an error. Callers
	// that only need the guest's verdict leave this false.
	RequireCosts bool
}

// EffectiveMaxSteps is the cap a run is actually subject to.
func (o Opts) EffectiveMaxSteps() uint64 {
	if o.MaxSteps > 0 {
		return o.MaxSteps
	}
	return DefaultZiskMaxSteps
}

// Result is the outcome of one emulator invocation.
type Result struct {
	Costs     CostReport
	OutputHex string // hex of the -o output region, "" if unreadable
	RawOut    string // combined stdout+stderr, trimmed
	ExecErr   string // guest-reported "execution failed: X", if any
	// StepLimit is true when the run reached its step cap. The emulator breaks
	// out of its loop bare on reaching max_steps, so this must be detected
	// explicitly rather than inferred from the exit status.
	StepLimit bool
	// ShortOutput is true when the output region is missing, under 43 bytes, or
	// all-zero — all of which mean the guest never wrote a verdict.
	ShortOutput bool
}

// Run writes input to a temp file, runs the emulator over it, and parses the
// cost report and output region.
//
// A non-nil error means the run itself failed. A run that completed but is
// untrustworthy (step-capped, no output region) returns a nil error with the
// corresponding flag set; deciding what that means is the caller's job.
func Run(o Opts, input []byte) (Result, error) {
	var r Result

	inFile, err := os.CreateTemp("", "zesu-emu-in-*.bin")
	if err != nil {
		return r, err
	}
	defer os.Remove(inFile.Name())
	if _, err := inFile.Write(input); err != nil {
		inFile.Close()
		return r, err
	}
	if err := inFile.Close(); err != nil {
		return r, err
	}

	// A fresh output file per run. Reusing one across runs would need an
	// explicit truncate, or a short run would read a previous run's tail.
	outFile, err := os.CreateTemp("", "zesu-emu-out-*.bin")
	if err != nil {
		return r, err
	}
	outPath := outFile.Name()
	outFile.Close()
	defer os.Remove(outPath)

	args := []string{"-X", "-e", o.ELF, "-i", inFile.Name(), "-o", outPath}
	if o.MaxSteps > 0 {
		args = append(args, "-n", strconv.FormatUint(o.MaxSteps, 10))
	}

	out, execErr := exec.Command(o.Bin, args...).CombinedOutput()
	r.RawOut = strings.TrimSpace(string(out))

	costs, foundCosts := ParseCostReport(r.RawOut)
	r.Costs = costs
	r.StepLimit = costs.Steps >= o.EffectiveMaxSteps()
	r.ExecErr = ParseExecError(r.RawOut)

	outBytes, readErr := os.ReadFile(outPath)
	if readErr == nil {
		r.OutputHex = hex.EncodeToString(outBytes)
	}
	r.ShortOutput = readErr != nil || len(outBytes) < StatelessOutputSize || allZero(outBytes)

	// Report the step cap ahead of the exit status: the emulator may exit 0 or 1
	// on reaching max_steps (it breaks out of its loop bare), so "exit status 1"
	// would be both uninformative and unreliable as the sole signal.
	if r.StepLimit {
		return r, fmt.Errorf("step limit reached: %d steps (cap %d)", r.Costs.Steps, o.EffectiveMaxSteps())
	}
	if execErr != nil {
		return r, fmt.Errorf("zkvm: %w", execErr)
	}
	if o.RequireCosts && !foundCosts {
		return r, fmt.Errorf("no cost report in emulator output")
	}
	warnIfStepsUnparsed(costs.Steps, foundCosts)
	return r, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

var stepsWarnOnce sync.Once

// warnIfStepsUnparsed guards against a silent loss of cap detection: an emulator
// build that predates the STEPS line leaves Steps at 0, which would make every
// step-limit check pass vacuously.
func warnIfStepsUnparsed(steps uint64, foundCosts bool) {
	if steps != 0 || !foundCosts {
		return
	}
	stepsWarnOnce.Do(func() {
		fmt.Println("WARNING: emulator output has no STEPS line — step-limit detection is inactive")
	})
}

var execFailedPrefix = "error: execution failed: "

// ParseExecError extracts the guest's reported execution failure, if any.
func ParseExecError(output string) string {
	i := strings.Index(output, execFailedPrefix)
	if i < 0 {
		return ""
	}
	rest := output[i+len(execFailedPrefix):]
	if f := strings.Fields(rest); len(f) > 0 {
		return f[0]
	}
	return ""
}

// ParseCostReport parses a COST DISTRIBUTION table from combined emulator
// output. Handles both the ZisK table (BASE/MAIN/OPCODES/PRECOMPILES/MEMORY/
// TOTAL) and the OpenVM one (INSTRUCTIONS/TOTAL).
//
// The bool reports whether a cost table was found at all.
func ParseCostReport(output string) (CostReport, bool) {
	var r CostReport
	sc := bufio.NewScanner(strings.NewReader(output))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	found := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := ParseCostLine(line, "BASE"); ok {
			r.Base = v
			found = true
		} else if v, ok := ParseCostLine(line, "MAIN"); ok {
			r.Main = v
		} else if v, ok := ParseCostLine(line, "OPCODES"); ok {
			r.Opcodes = v
		} else if v, ok := ParseCostLine(line, "PRECOMPILES"); ok {
			r.Precompiles = v
		} else if v, ok := ParseCostLine(line, "MEMORY"); ok {
			r.Memory = v
		} else if v, ok := ParseCostLine(line, "INSTRUCTIONS"); ok {
			r.Instructions = v
			found = true
		} else if v, ok := ParseCostLine(line, "STEPS"); ok {
			// Deliberately does not set `found`: STEPS sits in the REPORT header
			// above the cost table, so letting it satisfy `found` would mask a
			// run that produced no cost distribution at all. The numeric parse
			// is what disambiguates this from the "STEPS PROFILE TAGS" section
			// header further down the transcript.
			r.Steps = v
		} else if v, ok := ParseCostLine(line, "TOTAL"); ok {
			r.Total = v
		}
	}
	return r, found
}

// ParseCostLine reads "LABEL   1,234  12.3%" as 1234.
func ParseCostLine(line, label string) (uint64, bool) {
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
