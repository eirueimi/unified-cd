package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseSteps(t *testing.T, stepsYAML string) *Job {
	t.Helper()
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n" + stepsYAML
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	return job
}

func TestRunsIn_StepLevelRejected(t *testing.T) {
	// Step-level runsIn: was removed post-2026-07-08; container: is canonical.
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n    - name: build\n      run: go build\n      runsIn:\n        image: golang:1.22\n"
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn")
	assert.Contains(t, err.Error(), "container:")
}

func TestRunsIn_FlatContainerStillWorks(t *testing.T) {
	job := parseSteps(t, "    - name: build\n      run: go build\n      container: job\n")
	assert.Equal(t, "job", job.Spec.Steps[0].Container)
	assert.Nil(t, job.Spec.Steps[0].RunsIn)
}

func TestRunsIn_ImageAndContainerMutuallyExclusive(t *testing.T) {
	// Mutual exclusion now applies to a uses: entry's runsIn.
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n    - name: s\n      uses: { job: \"git://example.com/x/tpl.yaml@main\" }\n      runsIn:\n        image: golang:1.22\n        container: job\n"
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.container")
}

func TestRunsIn_FlatAndRunsInConflict(t *testing.T) {
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n    - name: s\n      run: x\n      container: job\n      runsIn:\n        image: golang:1.22\n"
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	// plain step: step-level runsIn: is rejected outright before the
	// container conflict would even be considered.
	assert.Contains(t, err.Error(), "runsIn")
}

// TestRunsIn_UsesContainerAndRunsInConflict covers checkStepExecTarget's
// uses-entry branch: unlike a plain step (rejected outright above), a uses:
// entry may legally carry container: doesn't apply to it — instead
// container: alongside runsIn: on the same uses: entry hits the
// "cannot set both container: and runsIn:" branch specifically.
func TestRunsIn_UsesContainerAndRunsInConflict(t *testing.T) {
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n    - name: s\n      uses: { job: \"git://example.com/x/tpl.yaml@main\" }\n      container: job\n      runsIn:\n        image: golang:1.22\n"
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot set both container: and runsIn:")
}
