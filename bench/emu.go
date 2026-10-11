package main

import (
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

// runOpenVM is the OpenVM equivalent of runEmu; see emu.RunOpenVM.
func runOpenVM(o emuOpts, input []byte) (emuResult, error) { return emu.RunOpenVM(o, input) }
