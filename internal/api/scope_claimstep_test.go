package api

import (
	"encoding/json"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

func TestClaimStepScopeFieldsJSON(t *testing.T) {
	cs := ClaimStep{
		Index: 0, Name: "compile", Run: "make", ScopeID: "scope:build", ScopeImage: "golang:1.22",
		ScopeResourceLimits: &dsl.ResourceList{CPU: "1", Memory: "512Mi"},
	}
	b, _ := json.Marshal(cs)
	var got ClaimStep
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ScopeID != "scope:build" || got.ScopeImage != "golang:1.22" {
		t.Fatalf("scope fields lost: %+v", got)
	}
	if got.ScopeResourceLimits == nil || got.ScopeResourceLimits.CPU != "1" || got.ScopeResourceLimits.Memory != "512Mi" {
		t.Fatalf("scope resource limits lost: %+v", got.ScopeResourceLimits)
	}
}
