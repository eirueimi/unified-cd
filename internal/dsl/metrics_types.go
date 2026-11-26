package dsl

import "strings"

// MetricNamePrefix is prepended to every job-defined metric name when it is
// exposed on the controller's /metrics endpoint.
const MetricNamePrefix = "unifiedcd_job_"

// MetricDef is one job-defined Prometheus metric. It is declared in a
// metrics: block on spec (evaluated when the run finishes) or on a step
// (evaluated when the step finishes). Exactly one of counter, gauge or
// histogram must be set.
type MetricDef struct {
	// Name is the metric name without the unifiedcd_job_ prefix.
	Name string `yaml:"name"`
	// Help is the Prometheus help text.
	Help string `yaml:"help"`
	// Labels are the label key/value templates attached to every sample.
	Labels []MetricLabel `yaml:"labels,omitempty"`
	// When is an optional condition; the metric is emitted only when it is true.
	When      string           `yaml:"when,omitempty"`
	Counter   *CounterMetric   `yaml:"counter,omitempty"`
	Gauge     *GaugeMetric     `yaml:"gauge,omitempty"`
	Histogram *HistogramMetric `yaml:"histogram,omitempty"`
}

// MetricLabel is one label of a metric. Value is a template.
type MetricLabel struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"`
}

// CounterMetric increments a counter by Value (empty means "1").
type CounterMetric struct {
	Value string `yaml:"value,omitempty"`
}

// GaugeMetric sets, adds to or subtracts from a gauge.
type GaugeMetric struct {
	Value string `yaml:"value"`
	// Realtime updates the gauge while the run (or step) is still running.
	// Only valid when value is exactly {{ .Run.Duration }} (run level) or
	// {{ .Step.Duration }} (step level).
	Realtime bool `yaml:"realtime,omitempty"`
	// Operation is one of Set, Add or Sub (empty means Set).
	Operation string `yaml:"operation,omitempty"`
}

// HistogramMetric observes Value into a histogram. Buckets are optional
// (default Prometheus buckets) and must be strictly increasing when given.
type HistogramMetric struct {
	Value   string    `yaml:"value"`
	Buckets []float64 `yaml:"buckets,omitempty"`
}

// Kind returns "counter", "gauge" or "histogram" ("" when none is set).
func (m MetricDef) Kind() string {
	switch {
	case m.Counter != nil:
		return "counter"
	case m.Gauge != nil:
		return "gauge"
	case m.Histogram != nil:
		return "histogram"
	}
	return ""
}

// ExposedName is the exposed metric family name. Counters get a _total
// suffix unless the name already ends in it.
func (m MetricDef) ExposedName() string {
	n := MetricNamePrefix + m.Name
	if m.Counter != nil && !strings.HasSuffix(n, "_total") {
		n += "_total"
	}
	return n
}

// Families returns every exposed family name (a histogram also exposes
// _bucket, _sum and _count).
func (m MetricDef) Families() []string {
	n := m.ExposedName()
	if m.Histogram != nil {
		return []string{n, n + "_bucket", n + "_sum", n + "_count"}
	}
	return []string{n}
}
