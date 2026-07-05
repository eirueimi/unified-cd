package api

import (
	"encoding/json"
	"testing"
)

func TestClaimStepScopeFieldsJSON(t *testing.T) {
	cs := ClaimStep{Index: 0, Name: "compile", Run: "make", ScopeID: "scope:build", ScopeImage: "golang:1.22"}
	b, _ := json.Marshal(cs)
	var got ClaimStep
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ScopeID != "scope:build" || got.ScopeImage != "golang:1.22" {
		t.Fatalf("scope fields lost: %+v", got)
	}
}
