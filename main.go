package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
	"github.com/Gabriel-Trintinalia/stateless-executor/metrics"
	"github.com/Gabriel-Trintinalia/stateless-executor/pipeline"
	"github.com/Gabriel-Trintinalia/stateless-executor/pool"
	"github.com/Gabriel-Trintinalia/stateless-executor/runner"
	"github.com/Gabriel-Trintinalia/stateless-executor/store"
)

func main() {
	// ── Config from environment ────────────────────────────────────────────────
	elURLs := splitEnv("EL_RPC_URLS")
	listenAddr := envOr("LISTEN_ADDR", ":8080")
	verbose := os.Getenv("VERBOSE") == "true"
	engineURL := os.Getenv("ENGINE_RPC_URL")
	jwtSecretFile := os.Getenv("JWT_SECRET_FILE")

	if len(elURLs) == 0 {
		log.Fatal("EL_RPC_URLS is required (comma-separated list of RPC endpoints)")
	}

	var genesis *fixture.GenesisChainConfig
	if path := os.Getenv("GENESIS_FILE"); path != "" {
		var err error
		genesis, err = fixture.ParseGenesisFile(path)
		if err != nil {
			log.Fatalf("GENESIS_FILE: %v", err)
		}
		log.Printf("genesis: chainId=%d forks=%d", genesis.ChainID, genesis.ForkCount())
	} else {
		log.Printf("GENESIS_FILE not set — using hardcoded Amsterdam mainnet chain config")
	}

	guests, err := runner.ParseGuestSpecs(os.Getenv("GUEST_BINARIES"))
	if err != nil {
		log.Fatalf("GUEST_BINARIES: %v", err)
	}

	log.Printf("EL nodes: %v", elURLs)
	for _, g := range guests {
		log.Printf("guest:    %s → %s", g.Name, g.Path)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ── EL pool ────────────────────────────────────────────────────────────────
	p, err := pool.New(elURLs)
	if err != nil {
		log.Fatalf("pool init: %v", err)
	}
	go p.Run(ctx)

	// ── One-shot mode (BLOCK_NUMBER set) ───────────────────────────────────────
	if blockStr := os.Getenv("BLOCK_NUMBER"); blockStr != "" {
		blockNum, err := strconv.ParseUint(blockStr, 10, 64)
		if err != nil {
			log.Fatalf("BLOCK_NUMBER: %v", err)
		}
		os.Exit(runBlock(ctx, p, genesis, guests, blockNum, verbose, engineURL, jwtSecretFile))
	}

	// ── Live mode ──────────────────────────────────────────────────────────────
	buf := &store.RingBuffer{}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/results", buf.Handler())

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	go func() {
		log.Printf("HTTP server listening on %s", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	log.Println("Waiting for new block heads...")
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			_ = srv.Shutdown(shutdownCtx)
			return

		case blockNum := <-p.Heads:
			metrics.BlockHeight.Set(float64(blockNum))
			log.Printf("")
		log.Printf("──────────────────────────────── block #%d ────────────────────────────────", blockNum)

			fetchCtx, fetchCancel := context.WithTimeout(ctx, 30*time.Second)
			input, elNode, meta, err := pipeline.Fetch(fetchCtx, p, blockNum, genesis, verbose, engineURL, jwtSecretFile)
			fetchCancel()
			if err != nil {
				log.Printf("block #%d: fetch error: %v", blockNum, err)
				continue
			}
			log.Printf("block #%d from %s | txs=%d gas=%d/%d base_fee=%d slot=%d bal=%d",
				blockNum, elNode, meta.TxCount, meta.GasUsed, meta.GasLimit,
				meta.BaseFee, meta.SlotNumber, meta.BALBytes)
			if verbose {
				log.Printf("block #%d: statelessInputBytes 0x%x", blockNum, input)
			}

			var wg sync.WaitGroup
			for _, g := range guests {
				wg.Add(1)
				go func(spec runner.GuestSpec) {
					defer wg.Done()
					runCtx, runCancel := context.WithTimeout(ctx, 5*time.Minute)
					defer runCancel()

					result, err := runner.Run(runCtx, spec, input, blockNum)
					result.WitnessFrom = elNode
					result.TxCount = meta.TxCount
					result.GasUsed = meta.GasUsed
					if err != nil {
						result.Block = blockNum
						result.Guest = spec.Name
						result.Error = err.Error()
						log.Printf("block #%d [%s]: runner error: %v", blockNum, spec.Name, err)
					}
					buf.Add(result)
					if result.Valid {
						log.Printf("block #%d [%s]: OK (%dms)", blockNum, spec.Name, result.DurationMs)
					} else {
						log.Printf("block #%d [%s]: FAIL (%dms)", blockNum, spec.Name, result.DurationMs)
					}
				}(g)
			}
			wg.Wait()
		}
	}
}

// runBlock fetches and verifies a single block, prints results, and returns
// an exit code (0 = all valid, 1 = any failure or error).
func runBlock(ctx context.Context, p *pool.Pool, genesis *fixture.GenesisChainConfig, guests []runner.GuestSpec, blockNum uint64, verbose bool, engineURL, jwtSecretFile string) int {
	fetchCtx, fetchCancel := context.WithTimeout(ctx, 30*time.Second)
	input, elNode, meta, err := pipeline.Fetch(fetchCtx, p, blockNum, genesis, verbose, engineURL, jwtSecretFile)
	fetchCancel()
	if err != nil {
		log.Printf("block #%d: fetch error: %v", blockNum, err)
		return 1
	}
	log.Printf("──────────────────────────────── block #%d ────────────────────────────────", blockNum)
	log.Printf("block #%d: %d bytes from %s | txs=%d gas=%d", blockNum, len(input), elNode, meta.TxCount, meta.GasUsed)
	if verbose {
		log.Printf("block #%d: statelessInputBytes 0x%x", blockNum, input)
	}

	exitCode := 0
	var wg sync.WaitGroup
	for _, g := range guests {
		wg.Add(1)
		go func(spec runner.GuestSpec) {
			defer wg.Done()
			runCtx, runCancel := context.WithTimeout(ctx, 5*time.Minute)
			defer runCancel()

			result, err := runner.Run(runCtx, spec, input, blockNum)
			result.WitnessFrom = elNode
			result.TxCount = meta.TxCount
			result.GasUsed = meta.GasUsed
			if err != nil {
				fmt.Printf("block #%d [%s]: ERROR: %v\n", blockNum, spec.Name, err)
				exitCode = 1
				return
			}
			if result.Valid {
				fmt.Printf("block #%d [%s]: OK (%dms)\n", blockNum, spec.Name, result.DurationMs)
			} else {
				fmt.Printf("block #%d [%s]: FAIL (%dms)\n", blockNum, spec.Name, result.DurationMs)
				exitCode = 1
			}
		}(g)
	}
	wg.Wait()
	return exitCode
}

func splitEnv(key string) []string {
	val := os.Getenv(key)
	if val == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(val, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
