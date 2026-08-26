// Package metrics owns the controller's Prometheus registry, metric
// families, and the store decorator / DB collector that feed them.
package metrics

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
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
	agentAuthEvents *prometheus.CounterVec
	collectorErrors prometheus.Counter
	buildInfo       *prometheus.GaugeVec

	bgRuns     *prometheus.CounterVec
	bgDuration *prometheus.HistogramVec
	bgItems    *prometheus.CounterVec
	logLines   *prometheus.CounterVec
	logBytes   prometheus.Counter
	queueWait  prometheus.Histogram
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
		agentAuthEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_agent_auth_events_total",
			Help: "Agent authentication and credential issuance events with bounded labels.",
		}, []string{"provider", "result", "reason"}),
		collectorErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "unifiedcd_scrape_collector_errors_total",
			Help: "Errors while collecting DB-backed gauges at scrape time.",
		}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "unifiedcd_build_info",
			Help: "Controller build information; always 1, the version is carried in the label.",
		}, []string{"version"}),
		bgRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_background_task_runs_total",
			Help: "Background worker passes, by task and outcome (success, error).",
		}, []string{"task", "outcome"}),
		bgDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "unifiedcd_background_task_duration_seconds",
			Help: "Background worker pass duration in seconds, by task.",
			// A pass that outlives its own tick interval is the failure these
			// buckets exist to show, so they run well past the fastest
			// interval any worker uses.
			Buckets: []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900},
		}, []string{"task"}),
		bgItems: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_background_task_items_total",
			Help: "Items a background worker acted on (runs archived, rows trimmed, runs reaped), by task and per-item result.",
		}, []string{"task", "result"}),
		logLines: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "unifiedcd_log_lines_ingested_total",
			Help: "Log lines received from agents, by result (accepted, dropped).",
		}, []string{"result"}),
		logBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "unifiedcd_log_bytes_ingested_total",
			Help: "Bytes of log line content received from agents, accepted or not.",
		}),
		queueWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "unifiedcd_run_time_to_claim_seconds",
			Help: "Time from run creation to an agent claiming it (spans both Pending and Queued).",
			// unifiedcd_runs_current{status="queued"} says how many runs are
			// waiting; this says how long they waited, which is the number a
			// CI user actually experiences.
			//
			// Measured from CreatedAt, so it spans the Pending phase (git
			// template resolution, concurrency gating) as well as Queued.
			// There is no queued_at column to measure the narrower window
			// from, and the wider one is the number a user cares about
			// anyway; the name says which it is so nobody reads it as pure
			// queue time.
			Buckets: []float64{1, 5, 15, 30, 60, 300, 900, 1800, 3600},
		}),
	}
	m.reg.MustRegister(m.runsCreated, m.runsFinished, m.stepsCompleted,
		m.stepDuration, m.webhookEvents, m.httpRequests, m.httpDuration,
		m.agentAuthEvents, m.collectorErrors, m.buildInfo,
		m.bgRuns, m.bgDuration, m.bgItems, m.logLines, m.logBytes, m.queueWait)

	// The Go and process collectors have to be registered explicitly here.
	// prometheus.DefaultRegisterer installs them for you; a private registry
	// starts completely empty, so choosing one above (for the coexistence
	// property it buys) silently dropped every go_* and process_* series.
	// Without them a goroutine leak, a memory climb toward an OOMKill, and GC
	// pressure behind a latency regression are all invisible to a dashboard.
	//
	// Registering them on the private registry keeps both properties: two
	// Metrics instances in one process get their own copies rather than
	// colliding, which is what a duplicate registration against the global
	// registry would do.
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// SetBuildInfo publishes the controller's build version as the label of a
// constant-1 gauge (the standard Prometheus build-info idiom). Calling it
// again replaces the series, so exactly one version is ever exported per
// process. Nothing consumes this to make a decision — it exists so an
// operator scraping /metrics can see which controller version is running
// next to the agent versions in GET /api/v1/agents.
func (m *Metrics) SetBuildInfo(version string) {
	m.buildInfo.Reset()
	m.buildInfo.WithLabelValues(version).Set(1)
}

// AgentAuthEvent records a credential event using only bounded labels.
func (m *Metrics) AgentAuthEvent(provider, result, reason string) {
	m.agentAuthEvents.WithLabelValues(agentAuthProvider(provider), agentAuthResult(result), agentAuthReason(reason)).Inc()
}

func agentAuthProvider(provider string) string {
	switch provider {
	case "uce", "one-time-token":
		return "one-time-token"
	case "kubernetes":
		return "kubernetes"
	case "uca", "access":
		return "access"
	case "ucr", "refresh":
		return "refresh"
	default:
		return "other"
	}
}

func agentAuthResult(result string) string {
	if result == "success" {
		return result
	}
	return "failure"
}

func agentAuthReason(reason string) string {
	switch reason {
	case "ok", "invalid", "expired", "disabled", "policy", "replay", "rate_limited", "unavailable":
		return reason
	default:
		return "other"
	}
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
	method = httpMethodLabel(method)
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

// httpMethodLabel folds the request method into a bounded label set. Only
// the standard HTTP methods pass through unchanged; anything else (typos,
// probes, or garbage tokens sent at an internet-facing endpoint) folds into
// "other" so it cannot mint unbounded label series.
func httpMethodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "other"
	}
}

// BackgroundTask records one pass of a background worker: how long it took,
// whether it failed, and how many items it acted on.
//
// One family for every worker rather than a family per worker, because the
// question an operator asks is the same for all of them — "is this still
// running, and is it keeping up?" — and a shared family means a new worker
// appears on the dashboard by passing its name here rather than by editing a
// query. task must be a fixed string, never derived from input: it is a
// Prometheus label, so an unbounded set of values is a cardinality leak.
//
// ok and failed are counted separately because several of these workers
// iterate a batch and swallow per-item failures to keep going — the archiver
// logs a run it could not archive and moves to the next one, returning nil.
// Pass-level outcome alone therefore reports SUCCESS for a worker whose every
// single item failed, which is precisely the silent breakage an operator needs
// to see. rate(...{result="error"}) is the query that catches it.
//
// A worker that fails is the case this exists for. Every one of these runs on
// a ticker with no external caller, so a silent failure has nothing else to
// surface it — the archiver in particular can stop archiving indefinitely
// while every other signal looks healthy.
func (m *Metrics) BackgroundTask(task string, ok, failed int, seconds float64, err error) {
	if m == nil {
		return
	}
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.bgRuns.WithLabelValues(task, outcome).Inc()
	m.bgDuration.WithLabelValues(task).Observe(seconds)
	if ok > 0 {
		m.bgItems.WithLabelValues(task, "ok").Add(float64(ok))
	}
	if failed > 0 {
		m.bgItems.WithLabelValues(task, "error").Add(float64(failed))
	}
}

// LogsIngested records a batch of agent log lines. bytes counts the line
// content received regardless of whether it was stored, so a run whose logs
// are being dropped still shows the ingress cost it is imposing.
func (m *Metrics) LogsIngested(accepted, dropped, bytes int) {
	if m == nil {
		return
	}
	if accepted > 0 {
		m.logLines.WithLabelValues("accepted").Add(float64(accepted))
	}
	if dropped > 0 {
		m.logLines.WithLabelValues("dropped").Add(float64(dropped))
	}
	if bytes > 0 {
		m.logBytes.Add(float64(bytes))
	}
}

// RunTimeToClaim records how long a run waited between being created and being
// claimed by an agent.
// Negative durations (a clock stepping backwards between the two timestamps)
// are dropped rather than clamped to zero, which would otherwise pile a
// fictional instant-claim into the first bucket.
func (m *Metrics) RunTimeToClaim(seconds float64) {
	if m == nil {
		return
	}
	if seconds < 0 {
		return
	}
	m.queueWait.Observe(seconds)
}
