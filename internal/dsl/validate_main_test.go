package dsl

import (
	"os"
	"testing"
)

func TestParseGitCheckoutTemplate(t *testing.T) {
	f, err := os.Open("../../templates/git-checkout.yaml")
	if err != nil {
		t.Skip("template file not found:", err)
	}
	defer f.Close()
	job, err := Parse(f)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if job.Metadata.Name != "git-checkout" {
		t.Errorf("expected name git-checkout, got %s", job.Metadata.Name)
	}
	if len(job.Spec.Steps) == 0 {
		t.Error("expected at least one step")
	}
}
