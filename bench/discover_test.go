package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

const (
	corpusJSON = `{"name":"rpc_block_1","stateless_input":{"block":{}},"success":true}`
	zkevmJSON  = `{"a::case":{"network":"Amsterdam","genesisBlockHeader":{},"blocks":[]}}`
	metaJSON   = `{"root_hash":"0x00","test_count":1,"forks":["Amsterdam"]}`
)

func write(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The load-bearing guard: a dot-directory must be pruned, not merely tolerated.
// A real spec-tests tree hides a 166 MB .meta/index.json that would cost minutes
// to parse for nothing.
func TestDiscoverPrunesDotDirs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "rpc_block_1.json", corpusJSON)
	write(t, root, ".meta/index.json", metaJSON)
	write(t, root, ".git/objects/blob.json", metaJSON)
	write(t, root, "nested/deep/rpc_block_2.json", corpusJSON)

	jobs, skipped, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	// The dot-dir files must not appear at all — neither as jobs nor as skips,
	// since a skip record means the file was opened and sniffed.
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2: %+v", len(jobs), jobs)
	}
	for _, s := range skipped {
		t.Errorf("unexpected skip (dot-dir should be pruned before sniffing): %+v", s)
	}
	for _, j := range jobs {
		if filepath.Base(filepath.Dir(j.Path)) == ".meta" || filepath.Base(filepath.Dir(j.Path)) == ".git" {
			t.Errorf("dot-dir file was not pruned: %s", j.Path)
		}
	}
}

// Discovery must recurse: the EEST trees nest six or seven levels deep, which
// the previous os.ReadDir implementation could not reach.
func TestDiscoverRecursesAndAssignsSuites(t *testing.T) {
	root := t.TempDir()
	write(t, root, "for_amsterdam_at_0060M/compute/precompile/identity.json", zkevmJSON)
	write(t, root, "for_amsterdam_at_0010M/compute/instruction/stack/swap.json", zkevmJSON)
	write(t, root, "flat.json", corpusJSON)

	jobs, _, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs, want 3", len(jobs))
	}

	wantSuites := map[string]string{
		"identity.json": "for_amsterdam_at_0060M/compute/precompile",
		"swap.json":     "for_amsterdam_at_0010M/compute/instruction/stack",
		"flat.json":     ".",
	}
	for _, j := range jobs {
		base := filepath.Base(j.Path)
		if want := wantSuites[base]; j.Suite != want {
			t.Errorf("%s: suite = %q, want %q", base, j.Suite, want)
		}
	}
}

func TestDiscoverSniffsKindAndSortsByPath(t *testing.T) {
	root := t.TempDir()
	write(t, root, "b_zkevm.json", zkevmJSON)
	write(t, root, "a_corpus.json", corpusJSON)

	jobs, _, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	// Sorted by full path, so a_corpus precedes b_zkevm.
	if got := filepath.Base(jobs[0].Path); got != "a_corpus.json" {
		t.Errorf("jobs not sorted by path: first is %s", got)
	}
	if jobs[0].Kind != fixture.FormatCorpus {
		t.Errorf("a_corpus.json: kind = %v, want corpus", jobs[0].Kind)
	}
	if jobs[1].Kind != fixture.FormatZkevm {
		t.Errorf("b_zkevm.json: kind = %v, want zkevm", jobs[1].Kind)
	}
	if jobs[0].Bytes != int64(len(corpusJSON)) {
		t.Errorf("Bytes = %d, want %d", jobs[0].Bytes, len(corpusJSON))
	}
}

// An unrecognised file is a skip, never a failure: a mixed tree legitimately
// holds files we cannot run.
func TestDiscoverSkipsUnknownFormat(t *testing.T) {
	root := t.TempDir()
	write(t, root, "good.json", corpusJSON)
	write(t, root, "junk.json", "not json")
	write(t, root, "notjson.txt", corpusJSON)

	jobs, skipped, err := discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1: %+v", len(jobs), jobs)
	}
	if len(skipped) != 1 || filepath.Base(skipped[0].Path) != "junk.json" {
		t.Fatalf("got skips %+v, want exactly junk.json", skipped)
	}
}

// --fixtures=<single file> must keep working.
func TestDiscoverSingleFile(t *testing.T) {
	root := t.TempDir()
	p := write(t, root, "rpc_block_1.json", corpusJSON)

	jobs, skipped, err := discover(p)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("unexpected skips: %+v", skipped)
	}
	if len(jobs) != 1 || jobs[0].Path != p || jobs[0].Kind != fixture.FormatCorpus {
		t.Fatalf("got %+v, want one corpus job for %s", jobs, p)
	}
}

func TestDiscoverMissingRoot(t *testing.T) {
	if _, _, err := discover(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("expected an error for a missing root")
	}
}
