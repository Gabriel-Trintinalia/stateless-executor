package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

// fileJob is one discovered fixture file. Discovery deliberately does NOT open
// or parse it beyond the 8 KiB format sniff — expansion into runnable units
// happens inside the worker so peak memory is bounded by jobs × one file.
type fileJob struct {
	Path  string
	Kind  fixture.Format
	Suite string // dir of Path relative to the fixtures root; "." at the root
	Bytes int64
}

// skippedFile records a file that discovery could not turn into work. These are
// reported but never fatal: a mixed-fork tree legitimately contains files we
// cannot run.
type skippedFile struct {
	Path   string
	Reason string
}

// discover walks root and returns every runnable fixture file, sorted by path.
//
// Layered guards, none of them fatal:
//   - dot-directories are pruned, which keeps a walk out of .git and out of the
//     166 MB spec-tests .meta/index.json
//   - a file whose format cannot be sniffed is skipped, not failed
//
// A single file (not a directory) is still accepted, preserving the old
// --fixtures=<file> behaviour.
func discover(root string) ([]fileJob, []skippedFile, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}

	if !info.IsDir() {
		kind, err := fixture.SniffFormat(root)
		if err != nil {
			return nil, nil, err
		}
		if kind == fixture.FormatUnknown {
			return nil, []skippedFile{{root, "unrecognised fixture format"}}, nil
		}
		return []fileJob{{Path: root, Kind: kind, Suite: ".", Bytes: info.Size()}}, nil, nil
	}

	var jobs []fileJob
	var skipped []skippedFile

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory or file is a skip, not a failure.
			skipped = append(skipped, skippedFile{path, err.Error()})
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// Prune dot-dirs. `.meta/index.json` in a spec-tests tree is 166 MB
			// and parsing it would cost minutes and gigabytes for nothing.
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		kind, err := fixture.SniffFormat(path)
		if err != nil {
			skipped = append(skipped, skippedFile{path, err.Error()})
			return nil
		}
		if kind == fixture.FormatUnknown {
			skipped = append(skipped, skippedFile{path, "unrecognised fixture format"})
			return nil
		}

		var size int64
		if fi, err := d.Info(); err == nil {
			size = fi.Size()
		}
		jobs = append(jobs, fileJob{
			Path:  path,
			Kind:  kind,
			Suite: suiteOf(root, path),
			Bytes: size,
		})
		return nil
	})
	if walkErr != nil {
		return nil, skipped, walkErr
	}

	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Path < jobs[j].Path })
	return jobs, skipped, nil
}

// suiteOf names the group a fixture belongs to: its directory relative to the
// fixtures root. For an EEST tree that yields e.g.
// "for_amsterdam_at_0060M/compute/precompile", which is the axis worth
// comparing across.
func suiteOf(root, path string) string {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return "."
	}
	return rel
}

func hasKind(jobs []fileJob, k fixture.Format) bool {
	for _, j := range jobs {
		if j.Kind == k {
			return true
		}
	}
	return false
}

// maxSuitesShown bounds the --dry-run suite listing. A spec-tests tree has
// ~14k suites; printing them all buries the census. Truncation is always
// reported, never silent.
const maxSuitesShown = 40

// census summarises discovery for --dry-run and for the startup log line.
type census struct {
	Jobs    []fileJob
	Skipped []skippedFile
}

func (c census) countByKind() map[fixture.Format]int {
	byKind := make(map[fixture.Format]int)
	for _, j := range c.Jobs {
		byKind[j.Kind]++
	}
	return byKind
}

func (c census) totalBytes() int64 {
	var n int64
	for _, j := range c.Jobs {
		n += j.Bytes
	}
	return n
}

// printCensus reports what discovery found, per format and per suite. This is
// the cheap tier of --dry-run: it sniffs and counts files but never expands a
// file into units, so it stays fast over a multi-gigabyte tree.
func printCensus(c census) {
	byKind := c.countByKind()
	fmt.Printf("=== Discovery ===\n")
	fmt.Printf("%-10s %8s %14s\n", "FORMAT", "FILES", "BYTES")
	fmt.Printf("%s\n", strings.Repeat("-", 34))
	for _, k := range []fixture.Format{fixture.FormatCorpus, fixture.FormatZkevm} {
		if byKind[k] == 0 {
			continue
		}
		var bytes int64
		for _, j := range c.Jobs {
			if j.Kind == k {
				bytes += j.Bytes
			}
		}
		fmt.Printf("%-10s %8d %14d\n", k, byKind[k], bytes)
	}
	fmt.Printf("%-10s %8d %14d\n", "all", len(c.Jobs), c.totalBytes())

	suites := map[string]int{}
	for _, j := range c.Jobs {
		suites[j.Suite]++
	}
	names := make([]string, 0, len(suites))
	for s := range suites {
		names = append(names, s)
	}
	// Largest first, path-sorted within a tie, so the truncation below drops the
	// least interesting suites. A deep spec-tests tree has ~14k of them.
	sort.Slice(names, func(i, j int) bool {
		if suites[names[i]] != suites[names[j]] {
			return suites[names[i]] > suites[names[j]]
		}
		return names[i] < names[j]
	})
	fmt.Printf("\n=== Suites (%d) ===\n", len(names))
	shown := names
	if len(shown) > maxSuitesShown {
		shown = shown[:maxSuitesShown]
	}
	for _, s := range shown {
		fmt.Printf("%6d  %s\n", suites[s], s)
	}
	if len(shown) < len(names) {
		var rest int
		for _, s := range names[len(shown):] {
			rest += suites[s]
		}
		fmt.Printf("  … %d more suite(s) not shown, %d file(s)\n", len(names)-len(shown), rest)
	}

	if len(c.Skipped) > 0 {
		fmt.Printf("\n=== Skipped (%d) ===\n", len(c.Skipped))
		for _, s := range c.Skipped {
			log.Printf("SKIP %s: %s", s.Path, s.Reason)
		}
	}
}
