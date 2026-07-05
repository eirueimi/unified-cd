package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseJob(t *testing.T, stepsYAML string) (*Job, error) {
	t.Helper()
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n" + stepsYAML
	return Parse(strings.NewReader(input))
}

func TestRunsInResources_Valid(t *testing.T) {
	job, err := parseJob(t, "    - name: build\n      run: go build\n      runsIn:\n        image: golang:1.22\n        resources:\n          requests:\n            cpu: \"500m\"\n            memory: \"256Mi\"\n          limits:\n            cpu: \"1\"\n            memory: \"512Mi\"\n")
	require.NoError(t, err)
	rs := job.Spec.Steps[0].RunsIn.Resources
	require.NotNil(t, rs)
	assert.Equal(t, "500m", rs.Requests.CPU)
	assert.Equal(t, "512Mi", rs.Limits.Memory)
}

func TestRunsInResources_InvalidQuantity(t *testing.T) {
	_, err := parseJob(t, "    - name: build\n      run: go build\n      runsIn:\n        image: golang:1.22\n        resources:\n          limits:\n            memory: \"512Megabytes\"\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resources")
}

func TestRunsInResources_RequiresImage(t *testing.T) {
	_, err := parseJob(t, "    - name: build\n      run: go build\n      runsIn:\n        container: job\n        resources:\n          limits:\n            cpu: \"1\"\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.resources requires runsIn.image")
}
