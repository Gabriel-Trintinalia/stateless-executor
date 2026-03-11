// Package runner executes a guest docker image against binary block input and
// parses the JSON result line written to stdout.
//
// Guest contract:
//   - stdin:  binary StatelessInput (pipeline.Fetch output)
//   - stdout: one JSON line: {"block":N,"valid":true,"pre_state_root":"0x...","post_state_root":"0x...","receipts_root":"0x..."}
//   - stderr: informational (logged but not parsed)
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/eth-proofs/stateless-executor/metrics"
	"github.com/eth-proofs/stateless-executor/store"
)

// guestResult is the JSON line emitted by the guest to stdout.
type guestResult struct {
	Block         uint64 `json:"block"`
	Valid         bool   `json:"valid"`
	PreStateRoot  string `json:"pre_state_root"`
	PostStateRoot string `json:"post_state_root"`
	ReceiptsRoot  string `json:"receipts_root"`
}

// Run executes the guest image, feeding input via stdin, and returns a
// store.Result ready to be added to the ring buffer.
//
// forkName is optional (e.g. "cancun"); if non-empty it is passed as --fork.
func Run(ctx context.Context, image string, input []byte, forkName string) (store.Result, error) {
	start := time.Now()

	args := []string{"run", "--rm", "-i", image}
	if forkName != "" {
		args = append(args, "--fork", forkName)
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = bytes.NewReader(input)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	durationMs := time.Since(start).Milliseconds()

	// Always log stderr so operator can see guest progress.
	if s := strings.TrimSpace(stderr.String()); s != "" {
		log.Printf("runner [%s]: %s", image, s)
	}

	shortName := shortImage(image)

	if err != nil {
		metrics.BlockVerifiedTotal.WithLabelValues(shortName, "error").Inc()
		metrics.VerificationDurationMs.WithLabelValues(shortName).Observe(float64(durationMs))
		return store.Result{}, fmt.Errorf("docker run %s: %w", image, err)
	}

	// Parse the last non-empty line of stdout as the JSON result.
	line, parseErr := lastJSONLine(stdout.Bytes())
	if parseErr != nil {
		metrics.BlockVerifiedTotal.WithLabelValues(shortName, "error").Inc()
		metrics.VerificationDurationMs.WithLabelValues(shortName).Observe(float64(durationMs))
		return store.Result{}, fmt.Errorf("runner [%s]: parsing output: %w (stdout=%q)", image, parseErr, stdout.String())
	}

	result := "ok"
	if !line.Valid {
		result = "fail"
	}
	metrics.BlockVerifiedTotal.WithLabelValues(shortName, result).Inc()
	metrics.VerificationDurationMs.WithLabelValues(shortName).Observe(float64(durationMs))

	return store.Result{
		Block:         line.Block,
		Guest:         shortName,
		Valid:         line.Valid,
		PreStateRoot:  line.PreStateRoot,
		PostStateRoot: line.PostStateRoot,
		ReceiptsRoot:  line.ReceiptsRoot,
		DurationMs:    durationMs,
	}, nil
}

// lastJSONLine finds the last non-empty line in buf and parses it as guestResult.
func lastJSONLine(buf []byte) (guestResult, error) {
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var r guestResult
		if err := json.Unmarshal(line, &r); err != nil {
			return guestResult{}, fmt.Errorf("invalid JSON %q: %w", line, err)
		}
		return r, nil
	}
	return guestResult{}, io.EOF
}

// shortImage strips registry/tag noise for use as a label value.
// "ghcr.io/consensys/zevm-stateless:latest" → "zevm-stateless"
func shortImage(image string) string {
	// Drop registry prefix (everything before last slash before colon).
	if idx := strings.LastIndex(image, "/"); idx >= 0 {
		image = image[idx+1:]
	}
	// Drop tag.
	if idx := strings.Index(image, ":"); idx >= 0 {
		image = image[:idx]
	}
	return image
}
