package main

import (
	"os"
	"strings"
	"testing"
)

// The golden transcript is real captured `ziskemu -X` output from a block that
// hit the step cap. It exercises every line shape the parser has to survive:
// the REPORT/STEPS header, the cost table, VARIABLE, FROPS, a RAM USAGE
// percentage line, per-opcode tables, and a "TOP STEP FUNCTIONS (STEPS, ...)"
// section header.
func TestParseCostReportGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/ziskemu_stepcapped.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parseCostReport(string(b))
	if !ok {
		t.Fatal("parseCostReport reported no cost table")
	}

	want := CostReport{
		Base:        293601280,
		Main:        4672924417980,
		Opcodes:     1462672139088,
		Precompiles: 3006480838,
		Memory:      845771242904,
		Total:       6984667882090,
		Steps:       68719476735,
	}
	if got != want {
		t.Errorf("parseCostReport =\n  %+v\nwant\n  %+v", got, want)
	}
}

// STEPS must not satisfy the "we found a cost table" flag. STEPS sits in the
// REPORT header above the table, so if it counted, a run that produced no cost
// distribution at all would be reported as a clean success.
func TestParseCostReportStepsDoesNotSatisfyFound(t *testing.T) {
	out := "input_len=1234\n\nREPORT\n------\nSTEPS   1,000,000\n"
	got, ok := parseCostReport(out)
	if ok {
		t.Error("STEPS alone satisfied the found flag; it must not")
	}
	if got.Steps != 1000000 {
		t.Errorf("Steps = %d, want 1000000", got.Steps)
	}
}

// "STEPS PROFILE TAGS" and "TOP STEP FUNCTIONS (STEPS, ...)" are section
// headers, not data. The numeric parse is what rejects them.
func TestParseCostReportIgnoresStepsHeaders(t *testing.T) {
	out := strings.Join([]string{
		"BASE      100",
		"STEPS PROFILE TAGS",
		"TOP STEP FUNCTIONS (STEPS, % STEPS, CALLS, STEPS/CALL, FUNCTION)",
		"RAM USAGE      38,888,448   7.27%",
		"FROPS          909,092,661,212  13.02%",
	}, "\n")
	got, ok := parseCostReport(out)
	if !ok {
		t.Fatal("expected BASE to satisfy the found flag")
	}
	if got.Steps != 0 {
		t.Errorf("a STEPS section header was parsed as a value: Steps = %d", got.Steps)
	}
	if got.Base != 100 {
		t.Errorf("Base = %d, want 100", got.Base)
	}
	// RAM USAGE is a percentage line and FROPS is unparsed; neither may leak
	// into a cost field.
	if got.Memory != 0 || got.Total != 0 {
		t.Errorf("unparsed lines corrupted the report: %+v", got)
	}
}

func TestEffectiveMaxSteps(t *testing.T) {
	if got := (emuOpts{}).effectiveMaxSteps(); got != defaultZiskMaxSteps {
		t.Errorf("MaxSteps 0: effective = %d, want the emulator default %d", got, defaultZiskMaxSteps)
	}
	if got := (emuOpts{MaxSteps: 500}).effectiveMaxSteps(); got != 500 {
		t.Errorf("MaxSteps 500: effective = %d, want 500", got)
	}
}

// Cap detection must fire even when --maxSteps was not passed: the captured
// transcript hit ziskemu's own default cap, exactly the case that would
// otherwise be reported as a suspiciously cheap pass.
func TestStepLimitDetectedAtEmulatorDefault(t *testing.T) {
	b, err := os.ReadFile("testdata/ziskemu_stepcapped.txt")
	if err != nil {
		t.Fatal(err)
	}
	costs, _ := parseCostReport(string(b))

	if costs.Steps < (emuOpts{}).effectiveMaxSteps() {
		t.Fatalf("steps %d did not reach the default cap %d", costs.Steps, defaultZiskMaxSteps)
	}
	// And an explicit lower cap must also trip.
	if costs.Steps < (emuOpts{MaxSteps: 1000000}).effectiveMaxSteps() {
		t.Error("steps did not reach an explicit low cap")
	}
	// A run well under the cap must not trip.
	if (CostReport{Steps: 1000}).Steps >= (emuOpts{MaxSteps: 1000000}).effectiveMaxSteps() {
		t.Error("a run far below the cap was flagged as capped")
	}
}

func TestAllZero(t *testing.T) {
	if !allZero(nil) || !allZero([]byte{0, 0, 0}) {
		t.Error("allZero should hold for empty and zero-filled input")
	}
	if allZero([]byte{0, 1, 0}) {
		t.Error("allZero should be false when any byte is set")
	}
}

// A missing, short, or all-zero output region means the guest never wrote a
// verdict, which must never read as a pass.
func TestRunEmuFlagsShortOutput(t *testing.T) {
	// `true` ignores its args, writes nothing, and exits 0 — a stand-in for an
	// emulator that produced no output region.
	r, err := runEmu(emuOpts{ELF: "/dev/null", Bin: "true"}, []byte{0x01})
	if err == nil {
		t.Error("expected an error when the emulator emits no cost report")
	}
	if !r.ShortOutput {
		t.Error("ShortOutput should be set when the output region is empty")
	}
}

func TestRunEmuMissingBinary(t *testing.T) {
	if _, err := runEmu(emuOpts{ELF: "/dev/null", Bin: "definitely-not-a-real-emulator"}, []byte{0x01}); err == nil {
		t.Fatal("expected an error for a missing emulator binary")
	}
}
