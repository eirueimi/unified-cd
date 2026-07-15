package dsl

import "testing"

func TestIsReservedContainerName(t *testing.T) {
	for _, n := range []string{"job", "unified-artifact", "ucd-shim"} {
		if !IsReservedContainerName(n) {
			t.Errorf("%q should be reserved", n)
		}
	}
	for _, n := range []string{"", "foo", "Job", "workspace"} {
		if IsReservedContainerName(n) {
			t.Errorf("%q should not be a reserved container name", n)
		}
	}
}

func TestIsReservedVolumeName(t *testing.T) {
	for _, n := range []string{"workspace", "ucd-tools"} {
		if !IsReservedVolumeName(n) {
			t.Errorf("%q should be reserved", n)
		}
	}
	for _, n := range []string{"", "cache", "job"} {
		if IsReservedVolumeName(n) {
			t.Errorf("%q should not be a reserved volume name", n)
		}
	}
}

func TestPodTemplateAccessors(t *testing.T) {
	if got := PodTemplateContainers(nil); got != nil {
		t.Fatalf("nil template containers: want nil, got %v", got)
	}
	if got := PodTemplateVolumes(nil); got != nil {
		t.Fatalf("nil template volumes: want nil, got %v", got)
	}
	pt := &PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "foo", "image": "x"},
			"not-a-map", // skipped
		},
		"volumes": []any{
			map[string]any{"name": "cache", "emptyDir": map[string]any{}},
		},
	}}
	cs := PodTemplateContainers(pt)
	if len(cs) != 1 || DefName(cs[0]) != "foo" {
		t.Fatalf("containers: got %v", cs)
	}
	vs := PodTemplateVolumes(pt)
	if len(vs) != 1 || DefName(vs[0]) != "cache" {
		t.Fatalf("volumes: got %v", vs)
	}
	if DefName(map[string]any{}) != "" {
		t.Fatalf("missing name should be empty string")
	}
}

func TestValidateContainerReferences(t *testing.T) {
	pt := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "tools", "image": "x"},
	}}}

	// Valid: empty (default), reserved, and a defined container; parallel + finally.
	okSpec := Spec{
		PodTemplate: pt,
		Steps: []StepEntry{
			{Name: "a", Run: "echo"},                  // empty container -> ok
			{Name: "b", Container: "job", Run: "e"},   // reserved -> ok
			{Name: "c", Container: "tools", Run: "e"}, // defined -> ok
			{Parallel: []Step{{Name: "p", Container: "tools", Run: "e"}}},
			{Name: "scoped", Container: "ignored", ScopeID: "s1"}, // scope-tagged -> skipped
		},
		Finally: []StepEntry{{Name: "f", Container: "tools", Run: "e"}},
	}
	if err := ValidateContainerReferences(okSpec); err != nil {
		t.Fatalf("valid spec should pass, got %v", err)
	}

	// Invalid: main-DAG step references an undefined container.
	badMain := Spec{PodTemplate: pt, Steps: []StepEntry{{Name: "x", Container: "missing", Run: "e"}}}
	if err := ValidateContainerReferences(badMain); err == nil {
		t.Fatal("undefined container in a step must error")
	}

	// Invalid: inside a parallel block.
	badPar := Spec{PodTemplate: pt, Steps: []StepEntry{{Parallel: []Step{{Name: "y", Container: "missing", Run: "e"}}}}}
	if err := ValidateContainerReferences(badPar); err == nil {
		t.Fatal("undefined container in a parallel step must error")
	}

	// Invalid: inside finally.
	badFin := Spec{PodTemplate: pt, Finally: []StepEntry{{Name: "z", Container: "missing", Run: "e"}}}
	if err := ValidateContainerReferences(badFin); err == nil {
		t.Fatal("undefined container in a finally step must error")
	}

	// Reserved is valid even with no podTemplate at all.
	if err := ValidateContainerReferences(Spec{Steps: []StepEntry{{Name: "a", Container: "unified-artifact", Run: "e"}}}); err != nil {
		t.Fatalf("reserved container with no podTemplate should pass, got %v", err)
	}
}
