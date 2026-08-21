package main

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// emuRunner executes one already-encoded input. The zisk and openvm targets
// differ only here.
type emuRunner func(input []byte) (emuResult, error)

// progress tracks how far a run has got. The denominator starts at one per file
// and grows as multi-block files expand, so a corpus run — where one file is
// always one unit — prints exactly the counts it always did.
type progress struct {
	done  atomic.Int64
	total atomic.Int64
}

func (p *progress) addUnits(n int) { p.total.Add(int64(n) - 1) }

// runAll expands every file into units and runs them, returning results in
// discovery order.
//
// The work item is the FILE, not the unit. That bounds peak memory to
// jobs × one file, parses a multi-case file once rather than once per block, and
// leaves the corpus path exactly as it was, since there one file is one unit.
// Units within a file run sequentially, in the order expand emitted them.
func runAll(jobs []fileJob, run emuRunner, target string, parallelism int) []BlockResult {
	perFile := make([][]BlockResult, len(jobs))

	var p progress
	p.total.Store(int64(len(jobs)))

	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup

	for i, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, job fileJob) {
			defer wg.Done()
			defer func() { <-sem }()

			units, err := expand(job)
			if err != nil {
				log.Printf("SKIP %s: %v", job.Path, err)
				p.done.Add(1)
				return
			}
			p.addUnits(len(units))

			out := make([]BlockResult, 0, len(units))
			for _, u := range units {
				out = append(out, runUnit(u, run, target, &p))
			}
			perFile[idx] = out
		}(i, j)
	}
	wg.Wait()

	var results []BlockResult
	for _, rs := range perFile {
		results = append(results, rs...)
	}
	return results
}

// runUnit loads, encodes, runs and verifies one unit. Load and encode are
// inside the timed span, as they have always been.
func runUnit(u unit, run emuRunner, target string, p *progress) BlockResult {
	t := time.Now()
	enc, encErr := u.Encode()
	var (
		r      emuResult
		runErr = encErr
	)
	if encErr == nil {
		r, runErr = run(enc.Input)
	}
	elapsed := time.Since(t)

	v := u.Verify(enc, r, runErr)

	info := u.Meta.Info
	if enc.Info != (blockInfo{}) {
		info = enc.Info
	}

	res := BlockResult{
		BlockNum:        u.Meta.BlockNum,
		Name:            u.Meta.Label,
		Label:           u.Meta.Label,
		Suite:           u.Meta.Suite,
		Network:         u.Meta.Network,
		Target:          target,
		Elapsed:         elapsed,
		ExpectedSuccess: v.ExpectedSuccess,
		ValidationOK:    v.OK(),
		Verdict:         v,
		OutputHex:       r.OutputHex,
		TxCount:         info.TxCount,
		GasUsed:         info.GasUsed,
		LegacyTxs:       info.LegacyTxs,
		Eip1559Txs:      info.Eip1559Txs,
		Eip2930Txs:      info.Eip2930Txs,
		Eip4844Txs:      info.Eip4844Txs,
		Eip7702Txs:      info.Eip7702Txs,
	}
	// A failed run reports no costs and no exec error, so a partial cost table
	// can never reach the statistics or the charts.
	if runErr != nil {
		res.Err = runErr
		res.ErrOutput = r.RawOut
	} else {
		res.Costs = r.Costs
		res.ExecError = r.ExecErr
	}

	printProgress(res, p, target)
	return res
}

func printProgress(res BlockResult, p *progress, target string) {
	n := p.done.Add(1)
	total := p.total.Load()
	costSuffix := ""
	if target != "openvm" {
		costSuffix = fmt.Sprintf("  total=%d", res.Costs.Total)
	}

	switch {
	case res.Err != nil:
		fmt.Printf("[%3d/%d] ERROR %-40s  %v\n", n, total, res.Name, res.Err)
	case res.Verdict.Kind == verdictSkip:
		fmt.Printf("[%3d/%d] SKIP %s: %s\n", n, total, res.Label, res.Verdict.Reason)
	case res.Verdict.Kind == verdictUnverified:
		fmt.Printf("[%3d/%d] %s%s  UNVERIFIED: %s  (%s)\n", n, total, res.Label, costSuffix, res.Verdict.Reason, res.Elapsed.Round(time.Millisecond))
	case !res.ValidationOK:
		fmt.Printf("[%3d/%d] block %d%s  VALIDATION FAILED (expected success=%v)  (%s)\n",
			n, total, res.BlockNum, costSuffix, res.ExpectedSuccess, res.Elapsed.Round(time.Millisecond))
	case res.ExecError != "":
		fmt.Printf("[%3d/%d] block %d%s  EXEC FAILED (expected): %s  (%s)\n",
			n, total, res.BlockNum, costSuffix, res.ExecError, res.Elapsed.Round(time.Millisecond))
	default:
		fmt.Printf("[%3d/%d] block %d%s  (%s)\n", n, total, res.BlockNum, costSuffix, res.Elapsed.Round(time.Millisecond))
	}
}
