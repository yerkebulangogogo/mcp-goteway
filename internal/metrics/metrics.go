package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus metrics for the gateway.
type Metrics struct {
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	circuitOpen     *prometheus.GaugeVec
}

// New registers and returns gateway metrics using the given registerer.
func New(reg prometheus.Registerer) *Metrics {
	factory := promauto.With(reg)
	return &Metrics{
		requestsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_gateway_requests_total",
			Help: "Total number of MCP requests proxied, partitioned by server, method, and result.",
		}, []string{"server", "method", "result"}),

		requestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mcp_gateway_request_duration_seconds",
			Help:    "Latency of MCP requests proxied to downstream servers.",
			Buckets: []float64{.005, .025, .1, .5, 2, 10},
		}, []string{"server", "method"}),

		circuitOpen: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mcp_gateway_circuit_breaker_open",
			Help: "1 if the circuit breaker for a server is open (requests blocked), 0 otherwise.",
		}, []string{"server"}),
	}
}

// Record increments the request counter and observes the latency.
func (m *Metrics) Record(server, method, result string, durationSec float64) {
	if m == nil {
		return
	}
	m.requestsTotal.WithLabelValues(server, method, result).Inc()
	m.requestDuration.WithLabelValues(server, method).Observe(durationSec)
}

// SetCircuitOpen sets the circuit breaker gauge for server: 1=open, 0=closed/half-open.
func (m *Metrics) SetCircuitOpen(server string, open bool) {
	if m == nil {
		return
	}
	v := 0.0
	if open {
		v = 1.0
	}
	m.circuitOpen.WithLabelValues(server).Set(v)
}
