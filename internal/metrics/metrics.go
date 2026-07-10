package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Request metrics
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "haprovider_requests_total",
			Help: "Total number of requests processed",
		},
		[]string{"endpoint", "provider", "transport", "method"},
	)

	failedRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "haprovider_failed_requests_total",
			Help: "Total number of failed requests",
		},
		[]string{"endpoint", "provider", "transport", "method"},
	)

	// Connection metrics
	openConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "haprovider_open_connections",
			Help: "Number of currently open connections",
		},
		[]string{"endpoint", "provider", "transport"},
	)

	totalConnections = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "haprovider_total_connections",
			Help: "Total number of connections established",
		},
		[]string{"endpoint", "provider", "transport"},
	)

	// Provider health metrics
	providerHealth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "haprovider_provider_health",
			Help: "Provider health status (1=healthy, 0=unhealthy)",
		},
		[]string{"endpoint", "provider"},
	)

	// Request duration metrics
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "haprovider_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint", "provider", "transport", "method"},
	)

	// In-flight requests currently being processed
	inflightRequests = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "haprovider_inflight_requests",
			Help: "Number of requests currently being processed",
		},
		[]string{"endpoint", "transport"},
	)

	// Requests served from a coalesced (deduplicated) upstream call rather than
	// their own upstream request.
	coalescedRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "haprovider_coalesced_requests_total",
			Help: "Total number of requests served from a coalesced upstream call",
		},
		[]string{"endpoint", "method"},
	)
)

// MetricsHandler returns an HTTP handler for the Prometheus metrics endpoint
func MetricsHandler() http.Handler {
	prometheus.MustRegister(requestsTotal)
	prometheus.MustRegister(failedRequestsTotal)
	prometheus.MustRegister(openConnections)
	prometheus.MustRegister(totalConnections)
	prometheus.MustRegister(providerHealth)
	prometheus.MustRegister(requestDuration)
	prometheus.MustRegister(inflightRequests)
	prometheus.MustRegister(coalescedRequests)
	return promhttp.Handler()
}

// RecordCoalescedRequest records that a request was served from a shared
// (deduplicated) upstream call instead of issuing its own.
func RecordCoalescedRequest(endpoint, method string) {
	coalescedRequests.WithLabelValues(endpoint, method).Inc()
}

// TrackInflight increments the in-flight gauge and returns a function that
// decrements it again, intended to be deferred at the start of a handler:
//
//	defer metrics.TrackInflight(endpoint, "http")()
func TrackInflight(endpoint, transport string) func() {
	inflightRequests.WithLabelValues(endpoint, transport).Inc()
	return func() {
		inflightRequests.WithLabelValues(endpoint, transport).Dec()
	}
}

// RecordRequest records metrics for a request
func RecordRequest(endpoint, provider, transport, method string, duration float64) {
	requestsTotal.WithLabelValues(endpoint, provider, transport, method).Inc()
	requestDuration.WithLabelValues(endpoint, provider, transport, method).Observe(duration)
}

func RecordFailedRequest(endpoint, provider, transport, method string) {
	failedRequestsTotal.WithLabelValues(endpoint, provider, transport, method).Inc()
}

func RecordOpenConnection(endpoint, provider string) {
	openConnections.WithLabelValues(endpoint, provider, "ws").Inc()
	totalConnections.WithLabelValues(endpoint, provider, "ws").Inc()
}

func RecordCloseConnection(endpoint, provider string) {
	openConnections.WithLabelValues(endpoint, provider, "ws").Dec()
}

func RecordProviderHealth(endpoint, provider string, online bool) {
	if online {
		providerHealth.WithLabelValues(endpoint, provider).Set(1)
	} else {
		providerHealth.WithLabelValues(endpoint, provider).Set(0)
	}
}
