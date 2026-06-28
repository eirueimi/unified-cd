package dsl_test

import (
	"strings"
	"testing"
	"time"

	"github.com/unified-cd/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSchedule_Valid(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly-build
spec:
  cron: "0 2 * * *"
  job: nightly-build
  params:
    env: production
`
	s, err := dsl.ParseSchedule(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "nightly-build", s.Metadata.Name)
	assert.Equal(t, "0 2 * * *", s.Spec.Cron)
	assert.Equal(t, "nightly-build", s.Spec.Job)
	assert.Equal(t, "production", s.Spec.Params["env"])
}

func TestParseSchedule_NoParams(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly-build
spec:
  cron: "0 2 * * *"
  job: nightly-build
`
	s, err := dsl.ParseSchedule(strings.NewReader(input))
	require.NoError(t, err)
	assert.Nil(t, s.Spec.Params)
}

func TestParseSchedule_InvalidCron(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: bad-cron
spec:
  cron: "not-valid-cron"
  job: some-job
`
	_, err := dsl.ParseSchedule(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.cron is invalid")
}

func TestParseSchedule_MissingJob(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly
spec:
  cron: "0 2 * * *"
`
	_, err := dsl.ParseSchedule(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.job is required")
}

func TestParseSchedule_MissingCron(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly
spec:
  job: some-job
`
	_, err := dsl.ParseSchedule(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.cron is required")
}

func TestParseSchedule_RejectsInvalidNameFormat(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: Nightly_Build
spec:
  cron: "0 2 * * *"
  job: nightly-build
`
	_, err := dsl.ParseSchedule(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.name is invalid")
}

func TestNextCronTime(t *testing.T) {
	base, _ := time.Parse(time.RFC3339, "2026-06-16T10:00:00Z")
	next, err := dsl.NextCronTime("0 2 * * *", base)
	require.NoError(t, err)
	assert.Equal(t, "2026-06-17T02:00:00Z", next.UTC().Format("2006-01-02T15:04:05Z"))
}
