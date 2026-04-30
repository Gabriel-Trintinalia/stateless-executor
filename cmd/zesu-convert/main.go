// zesu-convert: read one fixture JSON, write zesu-zkvm binary input to stdout or a file.
//
//	zesu-convert <fixture.json> [output.bin]
//	zesu-convert <fixture.json> > output.bin
package main

import (
	"fmt"
	"os"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: zesu-convert <fixture.json> [output.bin]\n")
		os.Exit(1)
	}

	f, err := fixture.LoadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}

	data, err := fixture.ZesuInput(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}

	out := os.Stdout
	if len(os.Args) >= 3 {
		out, err = os.Create(os.Args[2])
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
