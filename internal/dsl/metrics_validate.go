package dsl

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	maxMetricsPerBlock = 50
	maxLabelsPerMetric = 10
)

var metricNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

var reservedMetricLabels = map[string]bool{"job": true, "instance": true, "le": true, "quantile": true}

var (
	runDurationRe  = regexp.MustCompile(`^\{\{\s*\.Run\.Duration\s*\}\}$`)
	stepDurationRe = regexp.MustCompile(`^\{\{\s*\.Step\.Duration\s*\}\}$`)
)

// realtimeForbiddenLabelRefs are template fields whose value is unknown while
// the run or step is still running, so a realtime metric's labels may not use
// them.
var realtimeForbiddenLabelRefs = []string{
	".Run.Status", ".Run.Duration", ".Outputs",
	".Step.Status", ".Step.ExitCode", ".Step.Duration", ".Step.Attempts",
}

// ValidateMetrics validates one metrics: block. level is "run" (spec.metrics)
// or "step" (a step's metrics). Cross-block rules (same name, same shape;
// no metrics on call: steps) are enforced by the job-wide validator.
func ValidateMetrics(defs []MetricDef, level string) error {
	if len(defs) > maxMetricsPerBlock {
		return fmt.Errorf("metrics: at most %d metrics per block (got %d)", maxMetricsPerBlock, len(defs))
	}
	for i, m := range defs {
		if err := validateMetric(m, level); err != nil {
			return fmt.Errorf("metrics[%d] (%s): %w", i, m.Name, err)
		}
	}
	return nil
}

func validateMetric(m MetricDef, level string) error {
	if !metricNameRe.MatchString(m.Name) {
		return fmt.Errorf("name %q must match %s", m.Name, metricNameRe.String())
	}
	if strings.TrimSpace(m.Help) == "" {
		return fmt.Errorf("help is required")
	}
	if len(m.Labels) > maxLabelsPerMetric {
		return fmt.Errorf("at most %d labels per metric (got %d)", maxLabelsPerMetric, len(m.Labels))
	}
	seen := map[string]bool{}
	for j, l := range m.Labels {
		if !metricNameRe.MatchString(l.Key) {
			return fmt.Errorf("labels[%d].key %q must match %s", j, l.Key, metricNameRe.String())
		}
		if reservedMetricLabels[l.Key] {
			return fmt.Errorf("labels[%d].key %q is reserved (job, instance, le and quantile cannot be used)", j, l.Key)
		}
		if seen[l.Key] {
			return fmt.Errorf("labels[%d].key %q is duplicated", j, l.Key)
		}
		seen[l.Key] = true
	}
	n := 0
	for _, set := range []bool{m.Counter != nil, m.Gauge != nil, m.Histogram != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("exactly one of counter, gauge or histogram must be set (got %d)", n)
	}
	if m.Histogram != nil {
		b := m.Histogram.Buckets
		for i := 1; i < len(b); i++ {
			if b[i] <= b[i-1] {
				return fmt.Errorf("histogram.buckets must be strictly increasing (%g after %g)", b[i], b[i-1])
			}
		}
	}
	if m.Gauge != nil {
		g := m.Gauge
		switch g.Operation {
		case "", "Set", "Add", "Sub":
		default:
			return fmt.Errorf("gauge.operation %q is invalid (want Set, Add or Sub)", g.Operation)
		}
		if g.Realtime {
			if err := validateRealtime(m, level); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRealtime(m MetricDef, level string) error {
	want, re := "{{ .Run.Duration }}", runDurationRe
	if level == "step" {
		want, re = "{{ .Step.Duration }}", stepDurationRe
	}
	if !re.MatchString(strings.TrimSpace(m.Gauge.Value)) {
		return fmt.Errorf("gauge.realtime requires value to be exactly %s at %s level (got %q)", want, level, m.Gauge.Value)
	}
	for _, l := range m.Labels {
		for _, ref := range realtimeForbiddenLabelRefs {
			if strings.Contains(l.Value, ref) {
				return fmt.Errorf("label %q references %s, which is unknown while a realtime metric is updated", l.Key, ref)
			}
		}
	}
	return nil
}

// validateJobMetrics validates every metrics: block of a spec and the
// cross-block rules: metrics are not allowed on call: steps, a name must keep
// one shape across the job, and no two metric names may expose the same
// family.
func validateJobMetrics(spec Spec) error {
	type seenMetric struct {
		shape MetricShape
		where string
	}
	byName := map[string]seenMetric{}
	byFamily := map[string]string{} // family -> metric name

	add := func(defs []MetricDef, level, where string) error {
		if err := ValidateMetrics(defs, level); err != nil {
			return fmt.Errorf("%s.%w", where, err)
		}
		for _, m := range defs {
			sh := m.Shape()
			if prev, ok := byName[m.Name]; ok {
				if d := prev.shape.Diff(sh); d != "" {
					return fmt.Errorf("%s: metric %q is defined with a different %s than at %s; the same name must have the same shape within a job", where, m.Name, d, prev.where)
				}
			} else {
				byName[m.Name] = seenMetric{sh, where}
			}
			for _, f := range m.Families() {
				if other, ok := byFamily[f]; ok && other != m.Name {
					return fmt.Errorf("%s: metric %q exposes %s, which collides with metric %q", where, m.Name, f, other)
				}
				byFamily[f] = m.Name
			}
		}
		return nil
	}

	if err := add(spec.Metrics, "run", "spec.metrics"); err != nil {
		return err
	}
	step := func(p, name string, call *CallStep, defs []MetricDef) error {
		if call != nil && len(defs) > 0 {
			return fmt.Errorf("%s (%s): metrics is not supported on call: steps", p, name)
		}
		return add(defs, "step", p)
	}
	walk := func(entries []StepEntry, prefix string) error {
		for i, e := range entries {
			if len(e.Parallel) > 0 {
				for j, st := range e.Parallel {
					p := fmt.Sprintf("%s[%d].parallel[%d]", prefix, i, j)
					if err := step(p, st.Name, st.Call, st.Metrics); err != nil {
						return err
					}
				}
				continue
			}
			if err := step(fmt.Sprintf("%s[%d]", prefix, i), e.Name, e.Call, e.Metrics); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(spec.Steps, "spec.steps"); err != nil {
		return err
	}
	return walk(spec.Finally, "spec.finally")
}
