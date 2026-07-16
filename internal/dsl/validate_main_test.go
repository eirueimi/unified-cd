package dsl

import (
	"os"
	"testing"
)

func TestParseGitCheckoutTemplate(t *testing.T) {
	data, err := os.ReadFile("../../templates/git-checkout.yaml")
	if err != nil {
		t.Skip("template file not found:", err)
	}
	tpl, err := ParseJobTemplate(data)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if tpl.Metadata.Name != "git-checkout" {
		t.Errorf("expected name git-checkout, got %s", tpl.Metadata.Name)
	}
	if len(tpl.Spec.Steps) == 0 {
		t.Error("expected at least one step")
	}
}
