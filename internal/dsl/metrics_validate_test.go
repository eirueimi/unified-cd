package dsl

import (
	"fmt"
	"strings"
	"testing"
)

func metricsJob(specMetrics, stepBody string) string {
	return fmt.Sprintf(`apiVersion: unified-cd/v1
kind: Job
metadata:
  name: m
spec:
%s  steps:
    - name: a
%s
`, specMetrics, stepBody)
}

func counterYAML(name string) string {
	return "- name: " + name + "\n  help: h\n  counter: {}\n"
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestValidateMetrics(t *testing.T) {
	many := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(counterYAML(fmt.Sprintf("m%d", i)))
		}
		return b.String()
	}
	labels := func(n int) string {
		var b strings.Builder
		b.WriteString("- name: x\n  help: h\n  counter: {}\n  labels:\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "    - {key: k%d, value: v}\n", i)
		}
		return b.String()
	}
	run := func(block string) string { return metricsJob("  metrics:\n"+indent(block, "    "), "      run: echo") }
	step := func(block string) string {
		return metricsJob("", "      run: echo\n      metrics:\n"+indent(block, "        "))
	}

	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"valid counter", run("- name: c\n  help: h\n  counter: {}\n"), ""},
		{"valid gauge", run("- name: g\n  help: h\n  gauge: {value: '1', operation: Add}\n"), ""},
		{"valid histogram", run("- name: h\n  help: h\n  histogram: {value: '1', buckets: [1, 2, 3]}\n"), ""},
		{"valid run realtime", run("- name: g\n  help: h\n  gauge: {value: '{{ .Run.Duration }}', realtime: true}\n"), ""},
		{"valid step realtime", step("- name: g\n  help: h\n  gauge: {value: '{{ .Step.Duration }}', realtime: true}\n"), ""},
		{"bad name", run("- name: 1bad\n  help: h\n  counter: {}\n"), "must match"},
		{"reserved label", run("- name: x\n  help: h\n  counter: {}\n  labels: [{key: job, value: v}]\n"), "reserved"},
		{"dunder label key", run("- name: x\n  help: h\n  counter: {}\n  labels: [{key: __x, value: v}]\n"), "Prometheus-internal"},
		{"nan bucket", run("- name: x\n  help: h\n  histogram: {value: '1', buckets: [1, .nan]}\n"), "finite"},
		{"inf bucket", run("- name: x\n  help: h\n  histogram: {value: '1', buckets: [1, .inf]}\n"), "finite"},
		{"empty help", run("- name: x\n  counter: {}\n"), "help is required"},
		{"51 metrics", run(many(51)), "at most 50"},
		{"11 labels", run(labels(11)), "at most 10 labels"},
		{"zero types", run("- name: x\n  help: h\n"), "exactly one of"},
		{"two types", run("- name: x\n  help: h\n  counter: {}\n  gauge: {value: '1'}\n"), "exactly one of"},
		{"non-increasing buckets", run("- name: x\n  help: h\n  histogram: {value: '1', buckets: [1, 1]}\n"), "strictly increasing"},
		{"realtime on counter", run("- name: x\n  help: h\n  counter: {realtime: true}\n"), "realtime"},
		{"operation on counter", run("- name: x\n  help: h\n  counter: {operation: Add}\n"), "operation"},
		{"realtime bad value", run("- name: x\n  help: h\n  gauge: {value: '{{ .Outputs.x }}', realtime: true}\n"), "realtime requires value"},
		{"run realtime with step duration", run("- name: x\n  help: h\n  gauge: {value: '{{ .Step.Duration }}', realtime: true}\n"), "realtime requires value"},
		{"realtime label uses status", run("- name: x\n  help: h\n  gauge: {value: '{{ .Run.Duration }}', realtime: true}\n  labels: [{key: s, value: '{{ .Run.Status }}'}]\n"), "unknown while a realtime"},
		{"bad operation", run("- name: x\n  help: h\n  gauge: {value: '1', operation: Mul}\n"), "operation"},
		{"same name different labels in one block", run("- name: x\n  help: h\n  counter: {}\n  labels: [{key: a, value: v}]\n- name: x\n  help: h\n  counter: {}\n  labels: [{key: b, value: v}]\n"), "different labels"},
		{"family collision", run("- name: x\n  help: h\n  counter: {}\n- name: x_total\n  help: h\n  gauge: {value: '1'}\n"), "collides"},
		{"call step metrics", metricsJob("", "      call: {job: other}\n      metrics:\n"+indent(counterYAML("c"), "        ")), "not supported on call"},
		{"same name different labels across blocks", metricsJob(
			"  metrics:\n"+indent("- name: x\n  help: h\n  counter: {}\n  labels: [{key: a, value: v}]\n", "    "),
			"      run: echo\n      metrics:\n"+indent("- name: x\n  help: h\n  counter: {}\n  labels: [{key: b, value: v}]\n", "        ")), "different labels"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.yaml))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestMetricDef_ExposedNameAndFamilies(t *testing.T) {
	c := MetricDef{Name: "x", Counter: &CounterMetric{}}
	if got := c.ExposedName(); got != "unifiedcd_job_x_total" {
		t.Errorf("counter x = %q", got)
	}
	c2 := MetricDef{Name: "x_total", Counter: &CounterMetric{}}
	if got := c2.ExposedName(); got != "unifiedcd_job_x_total" {
		t.Errorf("counter x_total = %q", got)
	}
	h := MetricDef{Name: "h", Histogram: &HistogramMetric{}}
	want := []string{"unifiedcd_job_h", "unifiedcd_job_h_bucket", "unifiedcd_job_h_sum", "unifiedcd_job_h_count"}
	got := h.Families()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("histogram families = %v, want %v", got, want)
	}
}

func TestMetricShape_DiffAndHash(t *testing.T) {
	a := MetricDef{Name: "x", Help: "h", Labels: []MetricLabel{{Key: "b"}, {Key: "a"}}, Counter: &CounterMetric{}}.Shape()
	b := MetricDef{Name: "x", Help: "h", Labels: []MetricLabel{{Key: "a"}, {Key: "b"}}, Counter: &CounterMetric{}}.Shape()
	if a.Diff(b) != "" || a.Hash() != b.Hash() {
		t.Errorf("label order must not matter")
	}
	g := MetricDef{Name: "x", Help: "h", Gauge: &GaugeMetric{}}.Shape()
	if a.Diff(g) != "type" || a.Hash() == g.Hash() {
		t.Errorf("type diff = %q", a.Diff(g))
	}
	hh := MetricDef{Name: "x", Help: "h2", Counter: &CounterMetric{}}.Shape()
	if got := a.Diff(hh); got != "labels" {
		t.Errorf("diff = %q, want labels (checked before help)", got)
	}
}
