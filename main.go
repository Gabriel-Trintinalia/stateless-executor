package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/eth-proofs/stateless-executor/metrics"
	"github.com/eth-proofs/stateless-executor/pipeline"
	"github.com/eth-proofs/stateless-executor/pool"
	"github.com/eth-proofs/stateless-executor/runner"
	"github.com/eth-proofs/stateless-executor/store"
)

func main() {
	// ── Config from environment ────────────────────────────────────────────────
	elURLs := splitEnv("EL_RPC_URLS")
	guestImages := splitEnv("GUEST_IMAGES")
	forkName := os.Getenv("FORK_NAME") // optional, e.g. "cancun"
	listenAddr := envOr("LISTEN_ADDR", ":8080")

	if len(elURLs) == 0 {
		log.Fatal("EL_RPC_URLS is required (comma-separated list of RPC endpoints)")
	}
	if len(guestImages) == 0 {
		log.Fatal("GUEST_IMAGES is required (comma-separated list of docker images)")
	}

	log.Printf("EL nodes:     %v", elURLs)
	log.Printf("Guest images: %v", guestImages)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ── EL pool ────────────────────────────────────────────────────────────────
	p, err := pool.New(elURLs)
	if err != nil {
		log.Fatalf("pool init: %v", err)
	}
	go p.Run(ctx)

	// ── Ring buffer ────────────────────────────────────────────────────────────
	buf := &store.RingBuffer{}

	// ── HTTP server ────────────────────────────────────────────────────────────
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

	// ── Block pipeline ─────────────────────────────────────────────────────────
	log.Println("Waiting for new block heads...")
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			_ = srv.Shutdown(shutdownCtx)
			return

		case blockNum := <-p.Heads:
			log.Printf("block #%d: fetching", blockNum)
			metrics.BlockHeight.Set(float64(blockNum))

			fetchCtx, fetchCancel := context.WithTimeout(ctx, 30*time.Second)
			input, err := pipeline.Fetch(fetchCtx, p, blockNum)
			fetchCancel()
			if err != nil {
				log.Printf("block #%d: fetch error: %v", blockNum, err)
				continue
			}
			log.Printf("block #%d: encoded %d bytes, fanning out to %d guest(s)", blockNum, len(input), len(guestImages))

			// Fan out to all guest images in parallel.
			var wg sync.WaitGroup
			for _, image := range guestImages {
				wg.Add(1)
				go func(img string) {
					defer wg.Done()
					runCtx, runCancel := context.WithTimeout(ctx, 5*time.Minute)
					defer runCancel()

					result, err := runner.Run(runCtx, img, input, forkName)
					if err != nil {
						log.Printf("block #%d [%s]: runner error: %v", blockNum, img, err)
						return
					}
					buf.Add(result)
					if result.Valid {
						log.Printf("block #%d [%s]: OK (%dms)", blockNum, img, result.DurationMs)
					} else {
						log.Printf("block #%d [%s]: FAIL (%dms)", blockNum, img, result.DurationMs)
					}
				}(image)
			}
			wg.Wait()
		}
	}
}

// splitEnv splits a comma-separated environment variable, trimming spaces.
func splitEnv(key string) []string {
	val := os.Getenv(key)
	if val == "" {
		return nil
	}
	parts := strings.Split(val, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
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
