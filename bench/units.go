package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

// A unit is one runnable block: the smallest thing that gets its own emulator
// invocation and its own report row. A corpus file holds exactly one; an EEST
// file holds one per block across all its test cases.
//
// Encode is a closure rather than pre-loaded bytes on purpose. It is called
// inside the timed region, so elapsed_ms keeps including load and encode as it
// always has, and peak memory stays bounded by the files actually in flight
// rather than by every unit discovered.
type unit struct {
	Meta   unitMeta
	Encode func() (encoded, error)
	Verify func(enc encoded, r emuResult, runErr error) verdict
}

// unitMeta is what is known about a unit before it is loaded.
type unitMeta struct {
	Label    string // corpus: the file stem; EEST: test-case name[/blockN]
	BlockNum uint64 // corpus: the mainnet block number; EEST: the header number
	Suite    string
	Network  string
	Kind     fixture.Format
	Info     blockInfo // pre-filled where the format allows it; else from Encode
}

// encoded is the result of loading and encoding one unit: the guest input plus
// everything about the fixture that only becomes known once it is parsed.
type encoded struct {
	Input             []byte
	Info              blockInfo
	ExpectedSuccess   bool
	ExpectedOutputHex string
}

type verdictKind int

const (
	verdictPass verdictKind = iota
	verdictFail
	verdictSkip
	verdictUnverified
	verdictError
)

func (k verdictKind) String() string {
	switch k {
	case verdictPass:
		return "pass"
	case verdictFail:
		return "fail"
	case verdictSkip:
		return "skip"
	case verdictUnverified:
		return "unverified"
	default:
		return "error"
	}
}

// verdict is the unified record of a unit's outcome. The record is shared
// across formats; the rule that produces it is not. Computing validation in the
// driver is exactly what would let one format's rule leak onto the other, so
// each loader supplies its own Verify.
type verdict struct {
	Kind   verdictKind
	Mode   string // "success-flag" | "output-bytes"
	Reason string

	ExpectedSuccess bool
	GotSuccess      bool

	ExpectedOutputHex string
	GotOutputHex      string
	OutputMatch       bool
}

// OK reports whether the unit validated. Only an outright pass counts;
// unverified and skipped units are neither passes nor failures.
func (v verdict) OK() bool { return v.Kind == verdictPass }

// expand turns one discovered file into its runnable units. It is called from
// the worker, not from discovery, so a file is parsed once and only while it is
// being worked on.
func expand(j fileJob) ([]unit, error) {
	switch j.Kind {
	case fixture.FormatCorpus:
		return expandCorpus(j)
	case fixture.FormatZkevm:
		return expandEEST(j)
	default:
		return nil, fmt.Errorf("%s: unsupported fixture format %s", j.Path, j.Kind)
	}
}

// expandCorpus returns the single unit for a flat one-block fixture.
//
// It deliberately does NOT open the file: LoadFile happens inside Encode so the
// measured span is unchanged from the pre-unit implementation, and blockInfo
// arrives from Encode rather than from Meta.
func expandCorpus(j fileJob) ([]unit, error) {
	name := strings.TrimSuffix(filepath.Base(j.Path), ".json")
	path := j.Path

	return []unit{{
		Meta: unitMeta{
			Label:    name,
			BlockNum: extractBlockNum(name),
			Suite:    j.Suite,
			Kind:     j.Kind,
		},
		Encode: func() (encoded, error) {
			f, err := fixture.LoadFile(path)
			if err != nil {
				return encoded{}, fmt.Errorf("load: %w", err)
			}
			info := extractBlockInfo(f)
			input, err := fixture.ZesuInputSSZ(f)
			if err != nil {
				return encoded{Info: info, ExpectedSuccess: f.Success}, fmt.Errorf("encode: %w", err)
			}
			return encoded{
				Input:           input,
				Info:            info,
				ExpectedSuccess: f.Success,
			}, nil
		},
		Verify: verifyCorpus,
	}}, nil
}

// expandEEST parses one EEST blockchain-test file and returns a unit per block
// across all its test cases.
//
// The file is parsed once here, not once per block. Metadata needs no SSZ
// decoding: the JSON carries blockHeader.number, blockHeader.gasUsed and
// transactions[].type directly, so Meta is fully populated up front and Encode
// is a trivial re-encode of already-in-memory hex.
func expandEEST(j fileJob) ([]unit, error) {
	tcs, err := fixture.LoadZkevmFile(j.Path)
	if err != nil {
		return nil, err
	}

	var units []unit
	for _, tc := range tcs {
		for bi := range tc.Blocks {
			block := &tc.Blocks[bi]

			// Label must match cmd/zkevm-runner's exactly, so the two tools'
			// pass/fail sets can be cross-checked by label.
			label := tc.Name
			if len(tc.Blocks) > 1 {
				label = fmt.Sprintf("%s/block%d", tc.Name, bi)
			}

			units = append(units, unit{
				Meta: unitMeta{
					Label:    label,
					BlockNum: block.Number(),
					Suite:    j.Suite,
					Network:  tc.Network,
					Kind:     j.Kind,
					Info:     eestBlockInfo(block),
				},
				Encode: func() (encoded, error) {
					// The second return of ZesuInputFromZkevmBlock is
					// ExpectException == "", which is NOT the expectation this
					// tool validates against. Discard it deliberately.
					input, _, err := fixture.ZesuInputFromZkevmBlock(tc, block)
					enc := encoded{
						Input:             input,
						Info:              eestBlockInfo(block),
						ExpectedOutputHex: normaliseHex(block.StatelessOutputBytes),
					}
					// The authoritative expectation is byte 32 of the fixture's
					// own SszStatelessValidationResult, not expectException.
					enc.ExpectedSuccess = expectedSuccessFromOutput(enc.ExpectedOutputHex)
					if err != nil {
						return enc, err
					}
					return enc, nil
				},
				Verify: verifyEEST,
			})
		}
	}
	return units, nil
}

func eestBlockInfo(b *fixture.ZkevmBlock) blockInfo {
	info := blockInfo{
		TxCount: len(b.Transactions),
		GasUsed: b.GasUsed(),
	}
	for _, tx := range b.Transactions {
		switch strings.ToLower(strings.TrimPrefix(tx.Type, "0x")) {
		case "01":
			info.Eip2930Txs++
		case "02":
			info.Eip1559Txs++
		case "03":
			info.Eip4844Txs++
		case "04":
			info.Eip7702Txs++
		default: // "00", "" or absent
			info.LegacyTxs++
		}
	}
	return info
}

func normaliseHex(s string) string {
	return strings.ToLower(strings.TrimPrefix(s, "0x"))
}

// expectedSuccessFromOutput reads the expectation out of the fixture's own
// expected output: byte 32 of SszStatelessValidationResult is
// successful_validation.
func expectedSuccessFromOutput(expectedOutputHex string) bool {
	return len(expectedOutputHex) < 66 || expectedOutputHex[64:66] != "00"
}

// verifyEEST is the EEST rule: the SSZ output bytes are the authoritative
// pass/fail signal.
//
// It does NOT consult ExpectException. expectedSuccess comes from the fixture's
// SszStatelessValidationResult byte 32 (successful_validation), not from the
// block-level expectException field: a block can carry an expectException such
// as INVALID_BLOCK_ACCESS_LIST or INVALID_REQUESTS and still have a fixture
// output that says the stateless validation itself succeeded. Comparing the
// output bytes is what keeps those cases honest.
//
// execErr is display-only here. An expected-invalid block legitimately prints
// "error: execution failed: X", exits 0, and matches its expected output;
// letting execErr into the verdict would manufacture thousands of false
// failures.
func verifyEEST(enc encoded, r emuResult, runErr error) verdict {
	v := verdict{
		Mode:              "output-bytes",
		ExpectedSuccess:   enc.ExpectedSuccess,
		ExpectedOutputHex: enc.ExpectedOutputHex,
		GotOutputHex:      r.OutputHex,
	}

	if errors.Is(runErr, fixture.ErrMissingStatelessInputBytes) {
		v.Kind = verdictSkip
		v.Reason = "no statelessInputBytes"
		return v
	}
	if runErr != nil {
		v.Kind = verdictError
		v.Reason = runErr.Error()
		return v
	}

	// ziskemu's -o writes the full output region, zero-padded, so trim got to
	// expected's length before comparing.
	got := normaliseHex(r.OutputHex)
	if len(v.ExpectedOutputHex) > 0 && len(got) > len(v.ExpectedOutputHex) {
		got = got[:len(v.ExpectedOutputHex)]
	}
	v.GotSuccess = expectedSuccessFromOutput(got)

	// A fixture with no expected output cannot be validated. Upstream this
	// short-circuited to an unconditional pass, which silently hid the gap.
	if v.ExpectedOutputHex == "" {
		v.Kind = verdictUnverified
		v.Reason = "fixture has no statelessOutputBytes"
		return v
	}

	v.OutputMatch = got == v.ExpectedOutputHex
	if !v.OutputMatch {
		v.Kind = verdictFail
		v.Reason = "output mismatch"
		return v
	}
	v.Kind = verdictPass
	return v
}

// verifyCorpus is the corpus rule, unchanged: the guest's success is compared
// against the fixture's `success` flag.
func verifyCorpus(enc encoded, r emuResult, runErr error) verdict {
	gotSuccess := runErr == nil && r.ExecErr == ""
	v := verdict{
		Mode:            "success-flag",
		ExpectedSuccess: enc.ExpectedSuccess,
		GotSuccess:      gotSuccess,
		GotOutputHex:    r.OutputHex,
	}
	if runErr != nil {
		v.Kind = verdictError
		v.Reason = runErr.Error()
		return v
	}
	if gotSuccess != enc.ExpectedSuccess {
		v.Kind = verdictFail
		// Phrased to keep the progress line byte-identical to the pre-unit output.
		v.Reason = fmt.Sprintf("expected success=%v", enc.ExpectedSuccess)
		return v
	}
	v.Kind = verdictPass
	return v
}
