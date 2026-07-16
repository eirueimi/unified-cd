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

func TestAppSourceValidate_GitArgvHardening(t *testing.T) {
	base := func() AppSource {
		return AppSource{
			APIVersion: SupportedAPIVersion, Kind: "AppSource",
			Metadata: Metadata{Name: "a"},
			Spec:     AppSourceSpec{RepoURL: "https://github.com/org/repo.git", TargetRevision: "main", Path: "apps/x"},
		}
	}
	valid := base()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid appsource must pass: %v", err)
	}
	cases := map[string]func(*AppSource){
		"upload-pack repoURL": func(a *AppSource) { a.Spec.RepoURL = "--upload-pack=touch /tmp/pwned" },
		"schemeless repoURL":  func(a *AppSource) { a.Spec.RepoURL = "github.com/org/repo" },
		"dash revision":       func(a *AppSource) { a.Spec.TargetRevision = "-main" },
		"tilde revision":      func(a *AppSource) { a.Spec.TargetRevision = "HEAD~1" },
		"dash path":           func(a *AppSource) { a.Spec.Path = "--output=/etc/x" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			a := base()
			mut(&a)
			if err := a.Validate(); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
}
