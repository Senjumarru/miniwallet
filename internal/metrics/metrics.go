package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
	PaymentsTotal            *prometheus.CounterVec
	ProviderRequestDuration  prometheus.Histogram
	ProviderRetriesTotal     prometheus.Counter
	WebhookInvalidSignatures prometheus.Counter
	WebhookLateSuccessTotal  prometheus.Counter
	WebhookMismatchTotal     *prometheus.CounterVec
	CircuitBreakerState      prometheus.Gauge
	PaymentsPendingCount     prometheus.Gauge
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	factory := promauto.With(reg)

	return &Metrics{
		PaymentsTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "miniwallet",
				Subsystem: "payments",
				Name:      "total",
				Help:      "Total count of payments processed by status",
			},
			[]string{"status"},
		),
		ProviderRequestDuration: factory.NewHistogram(
			prometheus.HistogramOpts{
				Namespace: "miniwallet",
				Subsystem: "provider",
				Name:      "request_duration_seconds",
				Help:      "Duration of requests to the upstream payment provider",
				Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
			},
		),
		ProviderRetriesTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Namespace: "miniwallet",
				Subsystem: "provider",
				Name:      "retries_total",
				Help:      "Total count of provider call retry attempts",
			},
		),
		WebhookInvalidSignatures: factory.NewCounter(
			prometheus.CounterOpts{
				Namespace: "miniwallet",
				Subsystem: "webhook",
				Name:      "invalid_signatures_total",
				Help:      "Total count of webhooks rejected due to invalid signature",
			},
		),
		WebhookLateSuccessTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Namespace: "miniwallet",
				Subsystem: "webhook",
				Name:      "late_success_total",
				Help:      "Total count of late success webhooks requiring refund",
			},
		),
		WebhookMismatchTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "miniwallet",
				Subsystem: "webhook",
				Name:      "mismatch_total",
				Help:      "Total count of webhook attribute mismatches (amount, currency, provider_id)",
			},
			[]string{"type"},
		),
		CircuitBreakerState: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "miniwallet",
				Subsystem: "provider",
				Name:      "circuit_breaker_state",
				Help:      "Current circuit breaker state: 0=closed, 1=half-open, 2=open",
			},
		),
		PaymentsPendingCount: factory.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "miniwallet",
				Subsystem: "payments",
				Name:      "pending_count",
				Help:      "Current count of active pending payments",
			},
		),
	}
}
