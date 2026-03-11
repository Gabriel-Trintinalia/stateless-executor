// Package pool manages a set of Ethereum EL JSON-RPC endpoints.
// On startup it probes every URL with debug_executionWitness; nodes that do
// not support it are dropped. Surviving nodes are polled round-robin for new
// block heads.
package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eth-proofs/stateless-executor/metrics"
)

const (
	pollInterval = 500 * time.Millisecond
	probeTimeout = 10 * time.Second
	rpcTimeout   = 5 * time.Second
)

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Pool holds healthy EL nodes and exposes a channel of new block numbers.
type Pool struct {
	mu      sync.RWMutex
	nodes   []string // healthy RPC URLs
	robin   atomic.Uint64
	Heads   chan uint64 // emits a block number each time a new head is seen
	seen    uint64
	client  *http.Client
}

// New probes each URL and returns a Pool containing only responsive nodes.
func New(urls []string) (*Pool, error) {
	p := &Pool{
		Heads:  make(chan uint64, 32),
		client: &http.Client{Timeout: rpcTimeout},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var healthy []string

	for _, u := range urls {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			if probe(url) {
				mu.Lock()
				healthy = append(healthy, url)
				mu.Unlock()
				log.Printf("pool: %s OK", url)
			} else {
				log.Printf("pool: %s SKIP (debug_executionWitness not supported)", url)
			}
		}(u)
	}
	wg.Wait()

	if len(healthy) == 0 {
		return nil, fmt.Errorf("no EL nodes support debug_executionWitness")
	}
	p.nodes = healthy
	metrics.ELPoolSize.Set(float64(len(healthy)))
	return p, nil
}

// Run starts the polling loop; call in a goroutine.
func (p *Pool) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll()
		}
	}
}

func (p *Pool) poll() {
	url := p.next()
	if url == "" {
		return
	}
	num, err := blockNumber(p.client, url)
	if err != nil {
		log.Printf("pool: poll %s: %v", url, err)
		return
	}
	if num > p.seen {
		p.seen = num
		select {
		case p.Heads <- num:
		default:
			// consumer is behind; drop
		}
	}
}

// next returns the next URL in round-robin order.
func (p *Pool) next() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.nodes) == 0 {
		return ""
	}
	idx := p.robin.Add(1) - 1
	return p.nodes[idx%uint64(len(p.nodes))]
}

// Pick returns any healthy URL (used by pipeline to fetch block data).
func (p *Pool) Pick() string {
	return p.next()
}

// probe calls debug_executionWitness on "latest" and returns true if supported.
func probe(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	// First get the latest block number.
	client := &http.Client{Timeout: probeTimeout}
	num, err := blockNumber(client, url)
	if err != nil {
		return false
	}
	// Try debug_executionWitness on that block.
	hexNum := "0x" + strconv.FormatUint(num, 16)
	_, err = call(ctx, client, url, "debug_executionWitness", []interface{}{hexNum})
	return err == nil
}

func blockNumber(client *http.Client, url string) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	raw, err := call(ctx, client, url, "eth_blockNumber", nil)
	if err != nil {
		return 0, err
	}
	var hexStr string
	if err := json.Unmarshal(raw, &hexStr); err != nil {
		return 0, err
	}
	hexStr = strings.TrimPrefix(hexStr, "0x")
	return strconv.ParseUint(hexStr, 16, 64)
}

// call sends a JSON-RPC request and returns the raw result bytes.
func call(ctx context.Context, client *http.Client, url, method string, params []interface{}) (json.RawMessage, error) {
	if params == nil {
		params = []interface{}{}
	}
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc %s: %s", method, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// CallRaw is exposed so pipeline can reuse the shared client.
func (p *Pool) CallRaw(ctx context.Context, url, method string, params []interface{}) (json.RawMessage, error) {
	return call(ctx, p.client, url, method, params)
}
