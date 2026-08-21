package main

import (
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
		v.Reason = fmt.Sprintf("expected success=%v, got success=%v", enc.ExpectedSuccess, gotSuccess)
		return v
	}
	v.Kind = verdictPass
	return v
}
