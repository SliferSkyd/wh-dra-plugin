package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "wormhole_dra_requests_total",
		Help: "Total DRA plugin requests by operation.",
	}, []string{"driver", "operation"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "wormhole_dra_request_duration_seconds",
		Help:    "Duration of DRA plugin requests.",
		Buckets: prometheus.DefBuckets,
	}, []string{"driver", "operation"})

	requestsInFlight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "wormhole_dra_requests_inflight",
		Help: "Number of in-flight DRA plugin requests.",
	}, []string{"driver", "operation"})

	preparedDevices = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "wormhole_dra_prepared_devices",
		Help: "Number of currently prepared devices.",
	}, []string{"driver", "node"})

	nodePrepareErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "wormhole_node_prepare_errors_total",
		Help: "Total prepare errors by type.",
	}, []string{"driver", "error_type"})

	nodeUnprepareErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "wormhole_node_unprepare_errors_total",
		Help: "Total unprepare errors by type.",
	}, []string{"driver", "error_type"})
)

// TrackInFlight increments the in-flight gauge and returns a func to decrement it.
func TrackInFlight(driver, operation string) func() {
	requestsInFlight.WithLabelValues(driver, operation).Inc()
	return func() {
		requestsInFlight.WithLabelValues(driver, operation).Dec()
	}
}

// ObserveRequest records a completed request counter and histogram.
func ObserveRequest(driver, operation string, elapsed time.Duration) {
	requestsTotal.WithLabelValues(driver, operation).Inc()
	requestDuration.WithLabelValues(driver, operation).Observe(elapsed.Seconds())
}

// SetPreparedDevices updates the prepared-devices gauge.
func SetPreparedDevices(driver, node string, count int) {
	preparedDevices.WithLabelValues(driver, node).Set(float64(count))
}

// IncPrepareError records a prepare error.
func IncPrepareError(driver, errType string) {
	nodePrepareErrors.WithLabelValues(driver, errType).Inc()
}

// IncUnprepareError records an unprepare error.
func IncUnprepareError(driver, errType string) {
	nodeUnprepareErrors.WithLabelValues(driver, errType).Inc()
}
