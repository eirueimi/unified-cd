package dsl

import (
	"strings"
	"testing"
)

func podTplJobYAML(containerName, volumeName string) string {
	return `apiVersion: unified-cd/v1
kind: Job
metadata: {name: x}
spec:
  podTemplate:
    spec:
      containers: [{name: "` + containerName + `", image: img}]
      volumes: [{name: "` + volumeName + `", emptyDir: {}}]
  steps:
    - {name: s, run: echo}
`
}

func TestJobValidate_PodTemplateNameShape(t *testing.T) {
	if _, err := Parse(strings.NewReader(podTplJobYAML("tools", "cache"))); err != nil {
		t.Fatalf("valid names must pass: %v", err)
	}
	for _, bad := range []struct{ c, v, want string }{
		{"My_Tools", "cache", "My_Tools"},
		{"tools", "Cache Vol", "Cache Vol"},
	} {
		_, err := Parse(strings.NewReader(podTplJobYAML(bad.c, bad.v)))
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("names %q/%q: want error naming %q, got %v", bad.c, bad.v, bad.want, err)
		}
	}
}

func TestJobTemplateValidate_PodTemplateNameShape(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: t}
spec:
  podTemplate:
    spec:
      containers: [{name: "Bad Name", image: img}]
  steps:
    - {name: s, run: echo}
`
	if _, err := ParseJobTemplate([]byte(y)); err == nil || !strings.Contains(err.Error(), "Bad Name") {
		t.Errorf("JobTemplate podTemplate name shape must be validated, got %v", err)
	}
}
