package controller

import (
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlannedSteps(t *testing.T) {
	const y = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: j
spec:
  steps:
    - name: checkout
      run: echo hi
    - name: restore-cache
      cache:
        path: p
        key: k
    - name: build
      matrix:
        os: [linux, windows]
      run: echo build
    - name: upload
      uploadArtifact:
        name: a
        path: p
  finally:
    - name: notify
      run: echo done
`
	job, err := dsl.Parse(strings.NewReader(y))
	require.NoError(t, err)
	ps := plannedSteps(job.Spec)

	require.Len(t, ps, 5) // matrix counts as ONE planned entry
	// index/stageIndex are position-based across steps then finally (shared counter)
	assert.Equal(t, "checkout", ps[0].Name)
	assert.Equal(t, "run", ps[0].Kind)
	assert.Equal(t, "main", ps[0].Section)
	assert.Equal(t, 0, ps[0].StageIndex)
	assert.Equal(t, "Pending", ps[0].Status)

	assert.Equal(t, "restore-cache", ps[1].Name)
	assert.Equal(t, "cache", ps[1].Kind)
	assert.Equal(t, 1, ps[1].StageIndex)

	assert.Equal(t, "build", ps[2].Name)
	assert.Equal(t, "run", ps[2].Kind)
	assert.True(t, ps[2].Matrix)
	assert.Equal(t, 2, ps[2].StageIndex)

	assert.Equal(t, "upload", ps[3].Name)
	assert.Equal(t, "uploadArtifact", ps[3].Kind)
	assert.Equal(t, 3, ps[3].StageIndex)

	// finally: section=finally, stageIndex restarts at 0, stepIndex continues
	assert.Equal(t, "notify", ps[4].Name)
	assert.Equal(t, "finally", ps[4].Section)
	assert.Equal(t, 4, ps[4].Index)
	assert.Equal(t, 0, ps[4].StageIndex)
}
