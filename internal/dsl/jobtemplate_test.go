package dsl

import (
	"strings"
	"testing"
)

const validTemplateYAML = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: tools-tmpl}
spec:
  description: builds with tools
  params:
    inputs:
      - {name: target, type: string, default: all}
  shell: ["/bin/sh", "-c"]
  podTemplate:
    spec:
      containers:
        - {name: tools, image: alpine:3}
      volumes:
        - {name: toolcache, emptyDir: {}}
  steps:
    - {name: build, container: tools, run: make $target}
`

func TestParseJobTemplate_Valid(t *testing.T) {
	tpl, err := ParseJobTemplate([]byte(validTemplateYAML))
	if err != nil {
		t.Fatalf("valid template must parse, got %v", err)
	}
	if tpl.Metadata.Name != "tools-tmpl" || len(tpl.Spec.Steps) != 1 {
		t.Fatalf("unexpected parse result: %+v", tpl)
	}
	if len(tpl.Spec.PodTemplate.Spec.Containers) != 1 || len(tpl.Spec.PodTemplate.Spec.Volumes) != 1 {
		t.Fatalf("podTemplate subset not parsed: %+v", tpl.Spec.PodTemplate)
	}
}

func TestParseJobTemplate_KindJobRejectedWithGuidance(t *testing.T) {
	y := strings.Replace(validTemplateYAML, "kind: JobTemplate", "kind: Job", 1)
	_, err := ParseJobTemplate([]byte(y))
	if err == nil {
		t.Fatal("kind: Job must be rejected")
	}
	if !strings.Contains(err.Error(), "kind: JobTemplate") || !strings.Contains(err.Error(), "call:") {
		t.Fatalf("error must guide conversion or call:, got %v", err)
	}
}

func TestParseJobTemplate_UnknownFieldsRejected(t *testing.T) {
	cases := map[string]string{
		"agentSelector":        "spec:\n  agentSelector: [gpu]\n  steps:\n    - {name: s, run: echo}",
		"podTemplate.reuse":    "spec:\n  podTemplate:\n    reuse: true\n    spec:\n      containers: [{name: t, image: x}]\n  steps:\n    - {name: s, run: echo}",
		"podSpec nodeSelector": "spec:\n  podTemplate:\n    spec:\n      nodeSelector: {disk: ssd}\n      containers: [{name: t, image: x}]\n  steps:\n    - {name: s, run: echo}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			y := "apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: x}\n" + body + "\n"
			if _, err := ParseJobTemplate([]byte(y)); err == nil {
				t.Fatalf("unknown field %s must be rejected by strict decode", name)
			}
		})
	}
}

func TestParseJobTemplate_BasicValidation(t *testing.T) {
	noSteps := "apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: x}\nspec: {}\n"
	if _, err := ParseJobTemplate([]byte(noSteps)); err == nil {
		t.Fatal("a template with no steps must be rejected")
	}
	noName := "apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {}\nspec:\n  steps:\n    - {name: s, run: echo}\n"
	if _, err := ParseJobTemplate([]byte(noName)); err == nil {
		t.Fatal("a template with no metadata.name must be rejected")
	}
}

func TestJobTemplate_Finally(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: t}
spec:
  steps:
    - {name: s, run: echo}
  finally:
    - {name: cleanup, run: echo bye}
`
	tpl, err := ParseJobTemplate([]byte(y))
	if err != nil {
		t.Fatalf("finally must now be accepted on a JobTemplate: %v", err)
	}
	if len(tpl.Spec.Finally) != 1 || tpl.Spec.Finally[0].Name != "cleanup" {
		t.Fatalf("finally not parsed: %+v", tpl.Spec.Finally)
	}
	spec := tpl.ToSpec()
	if len(spec.Finally) != 1 || spec.Finally[0].Name != "cleanup" {
		t.Fatalf("ToSpec must carry finally: %+v", spec.Finally)
	}

	// Duplicate name across steps+finally rejected (shared nameSet).
	dup := `apiVersion: unified-cd/v1
kind: JobTemplate
metadata: {name: t}
spec:
  steps:
    - {name: s, run: echo}
  finally:
    - {name: s, run: echo}
`
	if _, err := ParseJobTemplate([]byte(dup)); err == nil {
		t.Fatal("duplicate step name across steps/finally must be rejected")
	}
}

func TestJobTemplateToSpec(t *testing.T) {
	tpl, err := ParseJobTemplate([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	spec := tpl.ToSpec()
	if len(spec.Steps) != 1 || len(spec.Shell) != 2 || len(spec.Params.Inputs) != 1 {
		t.Fatalf("ToSpec basic fields: %+v", spec)
	}
	if got := DefName(PodTemplateContainers(spec.PodTemplate)[0]); got != "tools" {
		t.Fatalf("ToSpec containers: got %q", got)
	}
	if got := DefName(PodTemplateVolumes(spec.PodTemplate)[0]); got != "toolcache" {
		t.Fatalf("ToSpec volumes: got %q", got)
	}

	// No podTemplate -> nil in the produced Spec.
	plain := "apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: x}\nspec:\n  steps:\n    - {name: s, run: echo}\n"
	tpl2, err := ParseJobTemplate([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	if tpl2.ToSpec().PodTemplate != nil {
		t.Fatal("template without podTemplate must produce a nil Spec.PodTemplate")
	}
}
