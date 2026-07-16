package dsl

import (
	"strings"
	"testing"
)

func wdJob(container string, podTpl string) string {
	return `apiVersion: unified-cd/v1
kind: Job
metadata: {name: x}
spec:
` + podTpl + `
  steps:
    - {name: s, ` + container + `run: echo}
`
}

func TestJobValidate_WorkingDirOnStepTargets(t *testing.T) {
	// job container with workingDir, default-target step -> error
	bad1 := wdJob("", `  podTemplate:
    spec:
      containers: [{name: job, image: img, workingDir: /app}]
`)
	if _, err := Parse(strings.NewReader(bad1)); err == nil || !strings.Contains(err.Error(), "workingDir") {
		t.Errorf("workingDir on the job container must be rejected, got %v", err)
	}

	// container: tools targeted by a step, tools has workingDir -> error
	bad2 := wdJob(`container: tools, `, `  podTemplate:
    spec:
      containers: [{name: job, image: img}, {name: tools, image: img2, workingDir: /app}]
`)
	if _, err := Parse(strings.NewReader(bad2)); err == nil || !strings.Contains(err.Error(), "tools") {
		t.Errorf("workingDir on a step-targeted container must be rejected, got %v", err)
	}

	// sidecar (not targeted) with workingDir -> OK
	ok := wdJob("", `  podTemplate:
    spec:
      containers: [{name: job, image: img}, {name: helper, image: img2, workingDir: /srv}]
`)
	if _, err := Parse(strings.NewReader(ok)); err != nil {
		t.Errorf("workingDir on a non-targeted sidecar must be allowed: %v", err)
	}

	// override container targeted by a step -> error
	bad3 := wdJob(`container: extra, `, `  podTemplate:
    spec:
      containers: [{name: job, image: img}]
    override:
      containers: [{name: extra, image: img3, workingDir: /app}]
`)
	if _, err := Parse(strings.NewReader(bad3)); err == nil || !strings.Contains(err.Error(), "extra") {
		t.Errorf("workingDir on a targeted override container must be rejected, got %v", err)
	}
}
