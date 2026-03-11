package store

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Result is one block verification result stored in the ring buffer
// and served by GET /results.
type Result struct {
	Block         uint64 `json:"block"`
	Guest         string `json:"guest"`
	Valid         bool   `json:"valid"`
	PreStateRoot  string `json:"pre_state_root"`
	PostStateRoot string `json:"post_state_root"`
	ReceiptsRoot  string `json:"receipts_root"`
	DurationMs    int64  `json:"duration_ms"`
	Timestamp     int64  `json:"timestamp"`
}

const capacity = 1000

// RingBuffer holds the last 1000 verification results (all guests combined).
type RingBuffer struct {
	mu    sync.RWMutex
	items [capacity]Result
	head  int
	count int
}

// Add inserts a result, stamping the current Unix timestamp.
func (r *RingBuffer) Add(res Result) {
	res.Timestamp = time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[r.head] = res
	r.head = (r.head + 1) % capacity
	if r.count < capacity {
		r.count++
	}
}

// All returns results in insertion order (oldest first).
func (r *RingBuffer) All() []Result {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Result, r.count)
	for i := range r.count {
		idx := (r.head - r.count + i + capacity) % capacity
		out[i] = r.items[idx]
	}
	return out
}

// Handler returns an http.HandlerFunc for GET /results.
func (r *RingBuffer) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.All())
	}
}
