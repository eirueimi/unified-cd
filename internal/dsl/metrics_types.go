package dsl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

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

// MetricShape is what must agree between two definitions of the same metric
// name within one job.
type MetricShape struct {
	Kind      string
	Help      string
	LabelKeys []string // sorted
	Buckets   []float64
	Realtime  bool
}

// Shape returns the definition's shape.
func (m MetricDef) Shape() MetricShape {
	keys := make([]string, 0, len(m.Labels))
	for _, l := range m.Labels {
		keys = append(keys, l.Key)
	}
	sort.Strings(keys)
	s := MetricShape{Kind: m.Kind(), Help: m.Help, LabelKeys: keys}
	if m.Histogram != nil {
		s.Buckets = m.Histogram.Buckets
	}
	if m.Gauge != nil {
		s.Realtime = m.Gauge.Realtime
	}
	return s
}

// Diff returns "" when the shapes are equal, otherwise the first differing
// aspect: "type", "labels", "help", "buckets" or "realtime".
func (a MetricShape) Diff(b MetricShape) string {
	switch {
	case a.Kind != b.Kind:
		return "type"
	case strings.Join(a.LabelKeys, "\x00") != strings.Join(b.LabelKeys, "\x00") || len(a.LabelKeys) != len(b.LabelKeys):
		return "labels"
	case a.Help != b.Help:
		return "help"
	case !floatsEqual(a.Buckets, b.Buckets):
		return "buckets"
	case a.Realtime != b.Realtime:
		return "realtime"
	}
	return ""
}

func floatsEqual(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Hash returns a stable digest of the shape.
func (s MetricShape) Hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%d", s.Kind, s.Help, len(s.LabelKeys))
	for _, k := range s.LabelKeys {
		fmt.Fprintf(h, "\x00%s", k)
	}
	fmt.Fprintf(h, "\x00%d", len(s.Buckets))
	for _, b := range s.Buckets {
		fmt.Fprintf(h, "\x00%g", b)
	}
	fmt.Fprintf(h, "\x00%t", s.Realtime)
	return hex.EncodeToString(h.Sum(nil))
}
