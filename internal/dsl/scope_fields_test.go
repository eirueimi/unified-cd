package dsl

import (
	"testing"

	"sigs.k8s.io/yaml"
)

func TestStepEntryScopeFieldsRoundTrip(t *testing.T) {
	se := StepEntry{Name: "x", Run: "true", ScopeID: "scope:build", ScopeImage: "golang:1.22"}
	b, err := yaml.Marshal(se)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got StepEntry
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ScopeID != "scope:build" || got.ScopeImage != "golang:1.22" {
		t.Fatalf("scope fields lost: %+v", got)
	}
}
