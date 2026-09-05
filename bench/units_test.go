package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

// The corpus rule is unchanged from the pre-unit implementation: the guest's
// success is compared against the fixture's `success` flag, and any run error
// is an error rather than a validation failure.
func TestVerifyCorpus(t *testing.T) {
	tests := []struct {
		name            string
		expectedSuccess bool
		execErr         string
		runErr          error
		want            verdictKind
	}{
		{"expected success, got success", true, "", nil, verdictPass},
		{"expected failure, got failure", false, "InvalidBlock", nil, verdictPass},
		{"expected success, got failure", true, "InvalidBlock", nil, verdictFail},
		{"expected failure, got success", false, "", nil, verdictFail},
		{"run error outranks validation", true, "", errors.New("zkvm: exit status 1"), verdictError},
		{"run error even when expectation would match", false, "InvalidBlock", errors.New("boom"), verdictError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := verifyCorpus(
				encoded{ExpectedSuccess: tc.expectedSuccess},
				emuResult{ExecErr: tc.execErr},
				tc.runErr,
			)
			if got.Kind != tc.want {
				t.Errorf("Kind = %v, want %v (reason %q)", got.Kind, tc.want, got.Reason)
			}
			if got.Mode != "success-flag" {
				t.Errorf("Mode = %q, want success-flag", got.Mode)
			}
			if got.OK() != (tc.want == verdictPass) {
				t.Errorf("OK() = %v, want %v", got.OK(), tc.want == verdictPass)
			}
		})
	}
}

// expandCorpus must not open the file: loading belongs inside Encode so it
// stays within the timed region.
func TestExpandCorpusIsLazy(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rpc_block_24758569.json")
	if err := os.WriteFile(p, []byte(corpusJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	units, err := expandCorpus(fileJob{Path: p, Kind: fixture.FormatCorpus, Suite: "."})
	if err != nil {
		t.Fatalf("expandCorpus: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	u := units[0]
	if u.Meta.Label != "rpc_block_24758569" {
		t.Errorf("Label = %q", u.Meta.Label)
	}
	if u.Meta.BlockNum != 24758569 {
		t.Errorf("BlockNum = %d, want 24758569", u.Meta.BlockNum)
	}
	if u.Meta.Info != (blockInfo{}) {
		t.Error("expandCorpus pre-filled blockInfo; it must come from Encode so the load stays timed")
	}

	// Deleting the file after expansion but before Encode must surface as an
	// Encode error — proof that expansion never read it.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Encode(); err == nil {
		t.Error("Encode succeeded after the file was removed; expansion must not have cached it")
	}
}

func TestExpandRejectsUnsupportedFormat(t *testing.T) {
	if _, err := expand(fileJob{Path: "x.json", Kind: fixture.FormatUnknown}); err == nil {
		t.Fatal("expected an error for an unsupported format")
	}
}

// The progress denominator starts at one per file and grows as multi-block
// files expand, so a corpus run prints exactly the counts it always did.
func TestProgressDenominator(t *testing.T) {
	var p progress
	p.total.Store(3)

	p.addUnits(1) // a corpus file: one file, one unit
	if got := p.total.Load(); got != 3 {
		t.Errorf("after a 1-unit file, total = %d, want 3 (unchanged)", got)
	}

	p.addUnits(5) // a multi-block file
	if got := p.total.Load(); got != 7 {
		t.Errorf("after a 5-unit file, total = %d, want 7", got)
	}
}
