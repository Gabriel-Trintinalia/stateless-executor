package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Gabriel-Trintinalia/stateless-executor/emu"
)

// The emulator plumbing lives in package emu, shared with cmd/zkevm-runner.
// These aliases keep the local names readable.
type (
	CostReport = emu.CostReport
	emuOpts    = emu.Opts
	emuResult  = emu.Result
)

const (
	defaultZiskMaxSteps = emu.DefaultZiskMaxSteps
	statelessOutputSize = emu.StatelessOutputSize
)

func parseCostReport(output string) (CostReport, bool) { return emu.ParseCostReport(output) }
func parseCostLine(line, label string) (uint64, bool)  { return emu.ParseCostLine(line, label) }
func parseExecError(output string) string              { return emu.ParseExecError(output) }

// runEmu runs the ZisK emulator. bench needs the cost table, so a run that
// produced none is an error.
func runEmu(o emuOpts, input []byte) (emuResult, error) {
	o.RequireCosts = true
	return emu.Run(o, input)
}

// runOpenVM is the OpenVM equivalent of runEmu. OpenVM emulation produces no
// circuit cost breakdown, so a missing cost table is not an error here, and the
// guest's verdict is read from the output region rather than from an
// "execution failed" line.
func runOpenVM(o emuOpts, input []byte) (emuResult, error) {
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

	outFile, err := os.CreateTemp("", "zesu-bench-out-*.bin")
	if err != nil {
		return r, err
	}
	outPath := outFile.Name()
	outFile.Close()
	defer os.Remove(outPath)

	out, runErr := exec.Command(o.Bin, "-X", "-e", o.ELF, "-i", inFile.Name(), "-o", outPath).
		CombinedOutput()
	r.RawOut = strings.TrimSpace(string(out))
	if runErr != nil {
		return r, fmt.Errorf("runner: %w", runErr)
	}

	outBytes, err := os.ReadFile(outPath)
	if err != nil {
		return r, fmt.Errorf("read output: %w", err)
	}
	if len(outBytes) < statelessOutputSize {
		return r, fmt.Errorf("output too short: %d bytes (expected %d)", len(outBytes), statelessOutputSize)
	}
	r.OutputHex = hex.EncodeToString(outBytes)

	if outBytes[32] == 0 {
		r.ExecErr = "ExecutionFailed"
	}
	r.Costs, _ = parseCostReport(r.RawOut)
	return r, nil
}
