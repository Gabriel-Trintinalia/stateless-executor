package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// defaultZiskMaxSteps mirrors DEFAULT_MAX_STEPS_STR in zisk
// core/src/zisk_definitions.rs (2^36 - 1). ziskemu applies it whenever -n is
// not passed, so it is the cap a run is measured against when --maxSteps is 0.
const defaultZiskMaxSteps uint64 = 68719476735

// statelessOutputSize is the flat SszStatelessValidationResult length under
// zkevm@v0.8.0: root(32) ‖ valid(1) ‖ chain_id(8, LE) ‖ schema_id(2, LE).
const statelessOutputSize = 43

// emuOpts describes one emulator invocation.
type emuOpts struct {
	ELF string
	Bin string // ziskemu (or a compatible emulator)
	// MaxSteps is passed as -n when non-zero. Zero means "do not pass -n",
	// leaving ziskemu on its own default; cap detection then measures against
	// defaultZiskMaxSteps.
	MaxSteps uint64
}

// effectiveMaxSteps is the cap a run is actually subject to.
func (o emuOpts) effectiveMaxSteps() uint64 {
	if o.MaxSteps > 0 {
		return o.MaxSteps
	}
	return defaultZiskMaxSteps
}

// emuResult is the outcome of one emulator invocation.
type emuResult struct {
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

// runEmu writes input to a temp file, runs the emulator over it, and parses the
// cost report and output region. It is the single place the emulator command
// line is built.
//
// A non-nil error means the run itself failed. A run that completed but is
// untrustworthy (step-capped, no output region) returns a nil error with the
// corresponding flag set; deciding what that means is the caller's job.
func runEmu(o emuOpts, input []byte) (emuResult, error) {
	var r emuResult

	inFile, err := os.CreateTemp("", "zesu-bench-in-*.bin")
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
	outFile, err := os.CreateTemp("", "zesu-bench-out-*.bin")
	if err != nil {
		return r, err
	}
	outPath := outFile.Name()
	outFile.Close()
	defer os.Remove(outPath)

	args := []string{"-X", "-e", o.ELF, "-i", inFile.Name(), "-o", outPath}
	if o.MaxSteps > 0 {
		args = append(args, "-n", fmt.Sprintf("%d", o.MaxSteps))
	}

	out, execOut := exec.Command(o.Bin, args...).CombinedOutput()
	r.RawOut = strings.TrimSpace(string(out))

	costs, ok := parseCostReport(r.RawOut)
	r.Costs = costs
	r.StepLimit = costs.Steps >= o.effectiveMaxSteps()
	r.ExecErr = parseExecError(r.RawOut)

	outBytes, readErr := os.ReadFile(outPath)
	if readErr == nil {
		r.OutputHex = hex.EncodeToString(outBytes)
	}
	r.ShortOutput = readErr != nil || len(outBytes) < statelessOutputSize || allZero(outBytes)

	// Report the step cap ahead of the exit status: the emulator may exit 0 or 1
	// on reaching max_steps (it breaks out of its loop bare), so "exit status 1"
	// would be both uninformative and unreliable as the sole signal.
	if r.StepLimit {
		return r, fmt.Errorf("step limit reached: %d steps (cap %d)", r.Costs.Steps, o.effectiveMaxSteps())
	}
	if execOut != nil {
		return r, fmt.Errorf("zkvm: %w", execOut)
	}
	if !ok {
		return r, fmt.Errorf("no cost report in emulator output")
	}
	warnIfStepsUnparsed(costs.Steps, ok)
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

// warnIfStepsUnparsed guards against a silent loss of cap detection: an
// emulator build that predates the STEPS line leaves Steps at 0, which would
// make every step-limit check pass vacuously.
func warnIfStepsUnparsed(steps uint64, foundCosts bool) {
	if steps != 0 || !foundCosts {
		return
	}
	stepsWarnOnce.Do(func() {
		fmt.Println("WARNING: emulator output has no STEPS line — step-limit detection is inactive")
	})
}
