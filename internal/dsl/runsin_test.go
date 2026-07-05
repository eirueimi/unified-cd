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

func TestRunsIn_Image(t *testing.T) {
	job := parseSteps(t, "    - name: build\n      run: go build\n      runsIn:\n        image: golang:1.22\n")
	ri := job.Spec.Steps[0].RunsIn
	require.NotNil(t, ri)
	assert.Equal(t, "golang:1.22", ri.Image)
	assert.Equal(t, "", ri.Container)
}

func TestRunsIn_FlatContainerNormalized(t *testing.T) {
	job := parseSteps(t, "    - name: build\n      run: go build\n      container: job\n")
	ri := job.Spec.Steps[0].RunsIn
	require.NotNil(t, ri)
	assert.Equal(t, "job", ri.Container)
	assert.Equal(t, "", job.Spec.Steps[0].Container, "flat container must be cleared after normalization")
}

func TestRunsIn_ImageAndContainerMutuallyExclusive(t *testing.T) {
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n    - name: s\n      run: x\n      runsIn:\n        image: golang:1.22\n        container: job\n"
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.image and runsIn.container are mutually exclusive")
}

func TestRunsIn_FlatAndRunsInConflict(t *testing.T) {
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n    - name: s\n      run: x\n      container: job\n      runsIn:\n        image: golang:1.22\n"
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot set both container: and runsIn:")
}
