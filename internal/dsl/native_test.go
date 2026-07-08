package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const nativeJobYAML = `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: ios-release }
spec:
  native: true
  steps:
    - name: build
      run: xcodebuild
`

func TestParse_NativeTrue(t *testing.T) {
	j, err := Parse(strings.NewReader(nativeJobYAML))
	require.NoError(t, err)
	assert.True(t, j.Spec.Native)
}

func TestValidate_NativeRejectsPodTemplate(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: bad }
spec:
  native: true
  podTemplate:
    spec:
      containers: [{ name: mysql, image: mysql:8 }]
  steps:
    - name: s
      run: echo hi
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "native")
	assert.Contains(t, err.Error(), "podTemplate")
}

func TestValidate_NativeRejectsContainerStep(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: bad }
spec:
  native: true
  steps:
    - name: s
      container: mysql
      run: echo hi
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "native")
	assert.Contains(t, err.Error(), "container")
}

func TestValidate_StepRunsInRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: bad }
spec:
  steps:
    - name: s
      runsIn: { image: golang:1.22 }
      run: go build
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	// migration hint
	assert.Contains(t, err.Error(), "runsIn")
	assert.Contains(t, err.Error(), "container:")
}

func TestValidate_UsesRunsInImageStillAllowed(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: ok }
spec:
  steps:
    - name: tpl
      uses: { job: "git://example.com/x/tpl.yaml@main" }
      runsIn: { image: golang:1.22 }
`
	_, err := Parse(strings.NewReader(y))
	require.NoError(t, err)
}

func TestValidate_UsesRunsInContainerRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: bad }
spec:
  steps:
    - name: tpl
      uses: { job: "git://example.com/x/tpl.yaml@main" }
      runsIn: { container: mysql }
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.container")
}
