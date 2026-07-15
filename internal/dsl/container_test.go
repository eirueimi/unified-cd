package dsl

import "testing"

func TestIsReservedContainerName(t *testing.T) {
	for _, n := range []string{"job", "unified-artifact"} {
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
