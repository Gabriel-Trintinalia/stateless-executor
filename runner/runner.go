// Package runner executes a guest binary against SSZ block input and
// parses the binary SszStatelessValidationResult written to stdout.
//
// Guest contract:
//   - stdin:  raw SSZ SszStatelessInput (no framing)
//   - stdout: binary SszStatelessValidationResult
//     [0..32] new_payload_request_root, [32] successful_validation
//   - stderr: informational (logged but not parsed)
package runner

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gabriel-Trintinalia/stateless-executor/metrics"
	"github.com/Gabriel-Trintinalia/stateless-executor/store"
)

// GuestSpec identifies a guest binary by name and filesystem path.
type GuestSpec struct {
	Name string
	Path string
}

// ParseGuestSpecs parses the GUEST_BINARIES env var value.
// Format: "name:/path/to/binary[,name2:/path/to/binary2]"
func ParseGuestSpecs(s string) ([]GuestSpec, error) {
	var specs []GuestSpec
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		idx := strings.Index(entry, ":")
		if idx < 0 {
			return nil, fmt.Errorf("invalid GUEST_BINARIES entry %q: expected name:/path", entry)
		}
		specs = append(specs, GuestSpec{
			Name: entry[:idx],
			Path: entry[idx+1:],
		})
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("GUEST_BINARIES is empty")
	}
	return specs, nil
}

// Run executes the guest binary at spec.Path, feeding input via stdin, and
// returns a store.Result ready to be added to the ring buffer.
// blockNum is set on the result since the guest output does not include it.
func Run(ctx context.Context, spec GuestSpec, input []byte, blockNum uint64) (store.Result, error) {
	// Ensure the binary is executable (Kurtosis file artifacts may drop the bit).
	if err := os.Chmod(spec.Path, 0755); err != nil {
		log.Printf("runner [%s]: chmod %s: %v (continuing)", spec.Name, spec.Path, err)
	}

	start := time.Now()

	cmd := exec.CommandContext(ctx, spec.Path)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Dir(spec.Path))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	durationMs := time.Since(start).Milliseconds()

	logOutput := strings.TrimSpace(stderr.String())
	if logOutput != "" {
		log.Printf("runner [%s]: %s", spec.Name, logOutput)
	}

	if err != nil {
		metrics.BlockVerifiedTotal.WithLabelValues(spec.Name, "error").Inc()
		metrics.VerificationDurationMs.WithLabelValues(spec.Name).Observe(float64(durationMs))
		return store.Result{Log: logOutput}, fmt.Errorf("runner [%s]: %w", spec.Name, err)
	}

	// SszStatelessValidationResult layout:
	//   [0..32]  new_payload_request_root (Bytes32)
	//   [32]     successful_validation (boolean: 0x00 or 0x01)
	//   [33..37] offset to chain_config (uint32 LE)
	//   [37..]   chain_config SSZ bytes
	out := stdout.Bytes()
	if len(out) < 33 {
		metrics.BlockVerifiedTotal.WithLabelValues(spec.Name, "error").Inc()
		metrics.VerificationDurationMs.WithLabelValues(spec.Name).Observe(float64(durationMs))
		return store.Result{Log: logOutput}, fmt.Errorf("runner [%s]: output too short (%d bytes)", spec.Name, len(out))
	}
	valid := out[32] == 0x01

	result := "ok"
	if !valid {
		result = "fail"
	}
	metrics.BlockVerifiedTotal.WithLabelValues(spec.Name, result).Inc()
	metrics.VerificationDurationMs.WithLabelValues(spec.Name).Observe(float64(durationMs))

	return store.Result{
		Block:      blockNum,
		Guest:      spec.Name,
		Valid:      valid,
		Log:        logOutput,
		DurationMs: durationMs,
	}, nil
}
