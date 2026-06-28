package dsl_test

import (
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validAppSourceYAML = `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: my-pipelines
spec:
  repoURL: https://github.com/org/repo
  targetRevision: main
  path: jobs/
  syncPolicy:
    interval: 5m
    prune: false
`

func TestParseAppSource_Valid(t *testing.T) {
	as, err := dsl.ParseAppSource(strings.NewReader(validAppSourceYAML))
	require.NoError(t, err)
	assert.Equal(t, "my-pipelines", as.Metadata.Name)
	assert.Equal(t, "https://github.com/org/repo", as.Spec.RepoURL)
	assert.Equal(t, "main", as.Spec.TargetRevision)
	assert.Equal(t, "jobs/", as.Spec.Path)
	assert.Equal(t, "5m", as.Spec.SyncPolicy.Interval)
	assert.False(t, as.Spec.SyncPolicy.Prune)
}

func TestParseAppSource_DefaultInterval(t *testing.T) {
	yaml := `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: x
spec:
  repoURL: https://github.com/org/repo
  targetRevision: main
  path: jobs/
`
	as, err := dsl.ParseAppSource(strings.NewReader(yaml))
	require.NoError(t, err)
	assert.Equal(t, 5*60, int(as.Spec.IntervalDuration().Seconds()))
}

func TestParseAppSource_MissingRepoURL(t *testing.T) {
	yaml := `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: x
spec:
  targetRevision: main
  path: jobs/
`
	_, err := dsl.ParseAppSource(strings.NewReader(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repoURL")
}

func TestParseAppSource_PathWithDotDot(t *testing.T) {
	yaml := `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: x
spec:
  repoURL: https://github.com/org/repo
  targetRevision: main
  path: ../etc
`
	_, err := dsl.ParseAppSource(strings.NewReader(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "..")
}

func TestParseAppSource_IntervalTooShort(t *testing.T) {
	yaml := `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: x
spec:
  repoURL: https://github.com/org/repo
  targetRevision: main
  path: jobs/
  syncPolicy:
    interval: 30s
`
	_, err := dsl.ParseAppSource(strings.NewReader(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1m")
}

func TestParseAppSource_MissingTargetRevision(t *testing.T) {
	yaml := `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: x
spec:
  repoURL: https://github.com/org/repo
  path: jobs/
`
	_, err := dsl.ParseAppSource(strings.NewReader(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "targetRevision")
}

func TestParseAppSource_RejectsInvalidNameFormat(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: My_Pipelines
spec:
  repoURL: https://github.com/org/repo
  targetRevision: main
  path: jobs/
`
	_, err := dsl.ParseAppSource(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.name is invalid")
}
