package dsl

import "testing"

func TestMetricValue(t *testing.T) {
	d := MetricTemplateData{Step: MetricStepData{Duration: 2.5}}
	tests := []struct {
		name    string
		tpl     string
		want    float64
		wantErr bool
	}{
		{"literal", "1", 1, false},
		{"padded", " 3.5\n", 3.5, false},
		{"step duration", "{{ .Step.Duration }}", 2.5, false},
		{"non-numeric", "abc", 0, true},
		{"nan", "NaN", 0, true},
		{"pos inf", "+Inf", 0, true},
		{"neg inf", "-Inf", 0, true},
		{"empty", "", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MetricValue(tc.tpl, d)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMetricWhen(t *testing.T) {
	d := MetricTemplateData{Step: MetricStepData{Status: "Succeeded"}}
	tests := []struct {
		name string
		tpl  string
		data MetricTemplateData
		want bool
	}{
		{"empty is true", "", d, true},
		{"status match", `{{ eq .Step.Status "Succeeded" }}`, d, true},
		{"status mismatch", `{{ eq .Step.Status "Failed" }}`, d, false},
		{"whitespace trimmed", " true\n", d, true},
		{"other text is false", "yes", d, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MetricWhen(tc.tpl, tc.data)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := MetricWhen("{{ bad", d); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestExpandMetricTemplate_Fields(t *testing.T) {
	d := MetricTemplateData{
		Run:                RunData{ID: "run-1", Duration: 7},
		RunDurationSeconds: 12.5,
		Job:                JobData{Name: "build", Labels: map[string]string{"team": "core"}},
		Params:             map[string]string{"env": "prod"},
		Outputs:            map[string]string{"coverage": "87"},
		Step:               MetricStepData{Name: "test", MatrixKey: "linux", ExitCode: 3},
	}
	tests := map[string]string{
		"{{ .Job.Labels.team }}":        "core",
		"{{ .Job.Name }}":               "build",
		"{{ .Params.env }}":             "prod",
		"{{ .Outputs.coverage }}":       "87",
		"{{ .Step.Name }}":              "test",
		"{{ .Step.MatrixKey }}":         "linux",
		"{{ .Step.ExitCode }}":          "3",
		"{{ .Run.ID }}":                 "run-1",
		"{{ .Run.Duration }}":           "12.5",
		"{{ .Params.missing }}":         "",
		"{{ .Outputs.missing }}":        "",
		"{{ .Job.Labels.missing }}":     "",
		`{{ "a,b" | split "," | len }}`: "2",
	}
	for tpl, want := range tests {
		got, err := ExpandMetricTemplate(tpl, d)
		if err != nil {
			t.Fatalf("%q: %v", tpl, err)
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", tpl, got, want)
		}
	}
}
