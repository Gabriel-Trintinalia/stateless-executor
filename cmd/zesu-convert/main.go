// zesu-convert: read one fixture JSON, write zesu-zkvm binary input to stdout or a file.
//
//	zesu-convert [--ziskinput | --openvm-input] <fixture.json> [output.bin]
//	zesu-convert <fixture.json> > output.bin
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

func main() {
	ziskInput := flag.Bool("ziskinput", false, "wrap output with 8-byte length header and alignment padding (zisk format)")
	openVMInput := flag.Bool("openvm-input", false, "wrap output for openvm runner (identical format to --ziskinput)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: zesu-convert [--ziskinput | --openvm-input] <fixture.json> [output.bin]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		flag.Usage()
		os.Exit(1)
	}

	f, err := fixture.LoadFile(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}

	var data []byte
	switch {
	case *ziskInput:
		data, err = fixture.ZesuInputSSZ(f)
	case *openVMInput:
		data, err = fixture.ZesuInputOpenVM(f)
	default:
		data, err = fixture.ZesuInputSSZPlain(f)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}

	out := os.Stdout
	if len(args) >= 2 {
		out, err = os.Create(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "create output: %v\n", err)
			os.Exit(1)
		}
		defer out.Close()
	}

	if _, err := out.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "ok: %d bytes, block=%d txns=%d\n",
		len(data), f.StatelessInput.Block.Header.Number, len(f.StatelessInput.Block.Body.Transactions))
}
