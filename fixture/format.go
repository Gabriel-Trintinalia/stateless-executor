package fixture

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

// Format identifies which fixture schema a JSON file uses.
type Format int

const (
	// FormatUnknown means the file matched no known fixture schema.
	FormatUnknown Format = iota
	// FormatCorpus is the flat one-block-per-file schema read by LoadFile:
	// {"name":…, "stateless_input":{…}, "success":bool}.
	FormatCorpus
	// FormatZkevm is the EEST blockchain_test schema read by LoadZkevmFile:
	// a map of test-case name to a case holding "blocks", each block carrying
	// "statelessInputBytes".
	FormatZkevm
)

func (f Format) String() string {
	switch f {
	case FormatCorpus:
		return "corpus"
	case FormatZkevm:
		return "zkevm"
	default:
		return "unknown"
	}
}

// sniffLimit is how much of a file's head SniffFormat inspects. Both schemas
// reveal themselves in the first few hundred bytes; 8 KiB is generous slack for
// a long leading test-case name. Keeping this small is what makes sniffing
// cheap enough to run over a multi-gigabyte fixture tree.
const sniffLimit = 8 << 10

// Format markers, most specific first. Each is a quoted JSON key that appears
// near the head of its schema and in no other.
var formatMarkers = []struct {
	marker []byte
	format Format
}{
	{[]byte(`"stateless_input"`), FormatCorpus},
	{[]byte(`"genesisBlockHeader"`), FormatZkevm},
	{[]byte(`"statelessInputBytes"`), FormatZkevm},
	{[]byte(`"blocks"`), FormatZkevm},
}

// SniffFormat reports which fixture schema path uses, reading at most sniffLimit
// bytes and never parsing the file. An unrecognised file is not an error: it
// returns FormatUnknown with a nil error so callers can record a skip. Only I/O
// problems are reported as errors.
func SniffFormat(path string) (Format, error) {
	f, err := os.Open(path)
	if err != nil {
		return FormatUnknown, err
	}
	defer f.Close()

	head := make([]byte, sniffLimit)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return FormatUnknown, fmt.Errorf("read %s: %w", path, err)
	}
	head = head[:n]

	for _, m := range formatMarkers {
		if bytes.Contains(head, m.marker) {
			return m.format, nil
		}
	}
	return FormatUnknown, nil
}
