package dsl

import (
	"strings"
	"testing"
)

func TestParseAppSource_AllowManualOverride(t *testing.T) {
	yamlDoc := `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: src
spec:
  repoURL: https://example.com/repo.git
  targetRevision: main
  path: jobs
  syncPolicy:
    allowManualOverride: true
`
	as, err := ParseAppSource(strings.NewReader(yamlDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !as.Spec.SyncPolicy.AllowManualOverride {
		t.Fatal("AllowManualOverride = false, want true")
	}
}
