package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	BlockVerifiedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "stateless_block_verified_total",
		Help: "Total blocks verified by guest image.",
	}, []string{"guest", "result"}) // result: ok | fail | error

	VerificationDurationMs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "stateless_verification_duration_ms",
		Help:    "Wall-clock time from docker run start to JSON result.",
		Buckets: []float64{10, 50, 100, 250, 500, 1000, 2500, 5000, 10000},
	}, []string{"guest"})
)
