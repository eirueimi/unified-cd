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

// Resources are only meaningful on a uses: entry's runsIn.image (step-level
// runsIn: was removed; see native_test.go / runsin_test.go).
func TestRunsInResources_Valid(t *testing.T) {
	job, err := parseJob(t, "    - name: tpl\n      uses: { job: \"git://example.com/x/tpl.yaml@main\" }\n      runsIn:\n        image: golang:1.22\n        resources:\n          limits:\n            cpu: \"1\"\n            memory: \"512Mi\"\n")
	require.NoError(t, err)
	rs := job.Spec.Steps[0].RunsIn.Resources
	require.NotNil(t, rs)
	assert.Nil(t, rs.Requests)
	assert.Equal(t, "1", rs.Limits.CPU)
	assert.Equal(t, "512Mi", rs.Limits.Memory)
}

func TestRunsInResources_InvalidQuantity(t *testing.T) {
	_, err := parseJob(t, "    - name: tpl\n      uses: { job: \"git://example.com/x/tpl.yaml@main\" }\n      runsIn:\n        image: golang:1.22\n        resources:\n          limits:\n            memory: \"512Megabytes\"\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resources")
}

// TestRunsInResources_RequestsRejected verifies the apply-time rejection of
// runsIn.resources.requests: neither backend has anywhere to route it (the
// host has no request concept, and this scope has no pod to route to
// Kubernetes) — see docs/user-guide/resources/job.md. The error must name the
// offending field and point at podTemplate, which already supports requests.
func TestRunsInResources_RequestsRejected(t *testing.T) {
	_, err := parseJob(t, "    - name: tpl\n      uses: { job: \"git://example.com/x/tpl.yaml@main\" }\n      runsIn:\n        image: golang:1.22\n        resources:\n          requests:\n            cpu: \"500m\"\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.resources.requests")
	assert.Contains(t, err.Error(), "podTemplate")
}

// TestRunsInResources_RequestsRejectedEvenAlongsideValidLimits confirms the
// rejection fires regardless of whether limits: is also present and valid —
// requests is rejected outright, not merged/ignored.
func TestRunsInResources_RequestsRejectedEvenAlongsideValidLimits(t *testing.T) {
	_, err := parseJob(t, "    - name: tpl\n      uses: { job: \"git://example.com/x/tpl.yaml@main\" }\n      runsIn:\n        image: golang:1.22\n        resources:\n          requests:\n            memory: \"256Mi\"\n          limits:\n            cpu: \"1\"\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.resources.requests")
}

func TestRunsInResources_RequiresImage(t *testing.T) {
	_, err := parseJob(t, "    - name: tpl\n      uses: { job: \"git://example.com/x/tpl.yaml@main\" }\n      runsIn:\n        container: job\n        resources:\n          limits:\n            cpu: \"1\"\n")
	require.Error(t, err)
	// runsIn.container is not valid on a uses: step at all now, so the
	// mutual-exclusion-with-container error fires before the
	// resources-requires-image check would.
	assert.Contains(t, err.Error(), "runsIn.container")
}
