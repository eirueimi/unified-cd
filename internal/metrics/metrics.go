// Package metrics owns the controller's Prometheus registry, metric
// families, and the store decorator / DB collector that feed them.
package metrics

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds one registry per controller instance. Call sites go through
// typed recorder methods so handlers never touch raw Prometheus types.
type Metrics struct {
	reg *prometheus.Registry

	runsCreated     *prometheus.CounterVec
	runsFinished    *prometheus.CounterVec
	stepsCompleted  *prometheus.CounterVec
	stepDuration    *prometheus.HistogramVec
	webhookEvents   *prometheus.CounterVec
	httpRequests    *prometheus.CounterVec
	httpDuration    *prometheus.HistogramVec
	collectorErrors prometheus.Counter
}

// New builds a Metrics with its own registry (never the global default, so
// multiple Server instances can coexist in tests).
func New() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		runsCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_runs_created_total",
			Help: "Runs created, by trigger source (webhook, schedule, api).",
		}, []string{"trigger"}),
		runsFinished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_runs_finished_total",
			Help: "Runs transitioned to a terminal status.",
		}, []string{"status"}),
		stepsCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_steps_completed_total",
			Help: "Step reports received with a non-Running status.",
		}, []string{"status"}),
		stepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "unifiedcd_step_duration_seconds",
			Help:    "Step wall-clock duration reported by agents.",
			Buckets: []float64{1, 5, 15, 60, 300, 900, 1800, 3600, 7200},
		}, []string{"status"}),
		webhookEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_webhook_events_total",
			Help: "Webhook ingress events, by receiver name and outcome (accepted, rejected, filtered, error).",
		}, []string{"name", "outcome"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_http_requests_total",
			Help: "HTTP requests, by method, chi route pattern, and status code.",
		}, []string{"method", "route", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "unifiedcd_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, by method and chi route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		collectorErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "unifiedcd_scrape_collector_errors_total",
			Help: "Errors while collecting DB-backed gauges at scrape time.",
		}),
	}
	m.reg.MustRegister(m.runsCreated, m.runsFinished, m.stepsCompleted,
		m.stepDuration, m.webhookEvents, m.httpRequests, m.httpDuration,
		m.collectorErrors)
	return m
}

// Handler serves the registry in the Prometheus text exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// RunCreated records a successful run creation. triggeredBy is the raw store
// value ("webhook:<name>", "schedule:<name>", "api", or a principal name).
func (m *Metrics) RunCreated(triggeredBy string) {
	m.runsCreated.WithLabelValues(triggerLabel(triggeredBy)).Inc()
}

// RunFinished records a run's actual transition into a terminal status.
func (m *Metrics) RunFinished(status string) {
	m.runsFinished.WithLabelValues(status).Inc()
}

// StepCompleted records a step report with a non-Running status.
func (m *Metrics) StepCompleted(status string) {
	m.stepsCompleted.WithLabelValues(status).Inc()
}

// StepDuration records a completed step's wall-clock duration.
func (m *Metrics) StepDuration(status string, seconds float64) {
	m.stepDuration.WithLabelValues(status).Observe(seconds)
}

// WebhookEvent records one webhook ingress outcome.
func (m *Metrics) WebhookEvent(name, outcome string) {
	m.webhookEvents.WithLabelValues(name, outcome).Inc()
}

// WebhookEventsForTest returns the underlying counter for label assertions
// in tests. Not for production use.
func (m *Metrics) WebhookEventsForTest(name, outcome string) prometheus.Counter {
	return m.webhookEvents.WithLabelValues(name, outcome)
}

// HTTPRequest records one served HTTP request.
func (m *Metrics) HTTPRequest(method, route string, code int, seconds float64) {
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(code)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(seconds)
}

// triggerLabel folds the free-form triggeredBy store value into a bounded
// label set. Principal names (manual API triggers) fold into "api".
func triggerLabel(triggeredBy string) string {
	switch {
	case strings.HasPrefix(triggeredBy, "webhook:"):
		return "webhook"
	case strings.HasPrefix(triggeredBy, "schedule:"):
		return "schedule"
	default:
		return "api"
	}
}
