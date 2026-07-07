package agent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

func podTmpl(containers ...map[string]any) *dsl.PodTemplate {
	cs := make([]any, len(containers))
	for i, c := range containers {
		cs[i] = c
	}
	return &dsl.PodTemplate{Spec: map[string]any{"containers": cs}}
}

func TestNamedContainerDef_Found(t *testing.T) {
	pt := podTmpl(
		map[string]any{"name": "other", "image": "busybox"},
		map[string]any{
			"name":  "tools",
			"image": "node:20",
			"env":   []any{map[string]any{"name": "FOO", "value": "bar"}},
			"resources": map[string]any{
				"limits": map[string]any{"cpu": "500m", "memory": "256Mi"},
			},
		},
	)
	def, err := namedContainerDef(pt, "tools")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if def.Name != "tools" || def.Image != "node:20" {
		t.Fatalf("unexpected def: %+v", def)
	}
	if len(def.Env) != 1 || def.Env[0] != "FOO=bar" {
		t.Fatalf("env = %v, want [FOO=bar]", def.Env)
	}
	if def.CPULimit != "0.5" {
		t.Fatalf("CPULimit = %q, want 0.5", def.CPULimit)
	}
	if def.MemLimit != "268435456" {
		t.Fatalf("MemLimit = %q, want 268435456", def.MemLimit)
	}
}

func TestNamedContainerDef_NoPodTemplate(t *testing.T) {
	if _, err := namedContainerDef(nil, "tools"); err == nil {
		t.Fatal("expected error when podTemplate is nil")
	}
}

func TestNamedContainerDef_UnknownName(t *testing.T) {
	pt := podTmpl(map[string]any{"name": "tools", "image": "node:20"})
	if _, err := namedContainerDef(pt, "missing"); err == nil {
		t.Fatal("expected error when container name is absent")
	}
}

func TestLimitStrings(t *testing.T) {
	cpu, mem := limitStrings("500m", "256Mi")
	if cpu != "0.5" {
		t.Fatalf("cpu = %q, want 0.5", cpu)
	}
	if mem != "268435456" {
		t.Fatalf("mem = %q, want 268435456", mem)
	}
	if c, m := limitStrings("", ""); c != "" || m != "" {
		t.Fatalf("empty in must yield empty out, got %q %q", c, m)
	}
}
