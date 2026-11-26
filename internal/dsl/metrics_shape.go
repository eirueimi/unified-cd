package dsl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

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
