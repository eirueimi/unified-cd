package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParse_ShellArgv covers shell: on spec, a plain step, a parallel-block
// step, and a post: hook — all parse into []string fields — plus the
// validation rules: nil (unset) is valid, an empty array is rejected, an
// array containing an empty string is rejected, and a bare scalar
// (`shell: bash`) is a parse error because v1 is array-only.
func TestParse_ShellArgv(t *testing.T) {
	t.Run("spec level", func(t *testing.T) {
		y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  shell: [bash, -lc]
  steps:
    - name: s
      run: echo hi
`
		job, err := Parse(strings.NewReader(y))
		require.NoError(t, err)
		assert.Equal(t, []string{"bash", "-lc"}, job.Spec.Shell)
	})

	t.Run("plain step level", func(t *testing.T) {
		y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - name: s
      shell: [python3, -c]
      run: print("hi")
`
		job, err := Parse(strings.NewReader(y))
		require.NoError(t, err)
		assert.Equal(t, []string{"python3", "-c"}, job.Spec.Steps[0].Shell)
	})

	t.Run("parallel step level", func(t *testing.T) {
		y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - parallel:
        - name: a
          shell: [bash, -euo, pipefail, -c]
          run: echo hi
`
		job, err := Parse(strings.NewReader(y))
		require.NoError(t, err)
		require.Len(t, job.Spec.Steps[0].Parallel, 1)
		assert.Equal(t, []string{"bash", "-euo", "pipefail", "-c"}, job.Spec.Steps[0].Parallel[0].Shell)
	})

	t.Run("finally step level", func(t *testing.T) {
		y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - name: s
      run: echo hi
  finally:
    - name: cleanup
      shell: [sh, -c]
      run: echo bye
`
		job, err := Parse(strings.NewReader(y))
		require.NoError(t, err)
		assert.Equal(t, []string{"sh", "-c"}, job.Spec.Finally[0].Shell)
	})

	t.Run("post hook level", func(t *testing.T) {
		y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - name: s
      shell: [python3, -c]
      run: print("hi")
      post:
        shell: [sh, -c]
        run: echo cleanup
`
		job, err := Parse(strings.NewReader(y))
		require.NoError(t, err)
		require.NotNil(t, job.Spec.Steps[0].Post)
		assert.Equal(t, []string{"sh", "-c"}, job.Spec.Steps[0].Post.Shell)
	})

	t.Run("unset is valid (nil)", func(t *testing.T) {
		y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - name: s
      run: echo hi
`
		job, err := Parse(strings.NewReader(y))
		require.NoError(t, err)
		assert.Nil(t, job.Spec.Shell)
		assert.Nil(t, job.Spec.Steps[0].Shell)
	})
}

func TestParse_ShellArgv_EmptyArrayRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  shell: []
  steps:
    - name: s
      run: echo hi
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell")
}

func TestParse_ShellArgv_EmptyStringElementRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - name: s
      shell: ["", "-c"]
      run: echo hi
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell")
}

func TestParse_ShellArgv_StepLevelEmptyArrayRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - name: s
      shell: []
      run: echo hi
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell")
}

func TestParse_ShellArgv_ParallelStepEmptyArrayRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - parallel:
        - name: a
          shell: []
          run: echo hi
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell")
}

func TestParse_ShellArgv_PostEmptyStringElementRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - name: s
      run: echo hi
      post:
        shell: ["sh", ""]
        run: echo bye
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell")
}

func TestParse_ShellArgv_FinallyEmptyArrayRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - name: s
      run: echo hi
  finally:
    - name: cleanup
      shell: []
      run: echo bye
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell")
}

// v1 is array-only: a bare scalar must fail to parse, not be silently
// accepted or string-split.
func TestParse_ShellArgv_ScalarRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - name: s
      shell: bash
      run: echo hi
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
}

func TestParse_ShellArgv_SpecScalarRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  shell: bash
  steps:
    - name: s
      run: echo hi
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
}

func TestValidShellArgv(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		wantErr bool
	}{
		{"nil unset", nil, false},
		{"valid argv", []string{"bash", "-lc"}, false},
		{"empty array", []string{}, true},
		{"empty string element", []string{"", "-c"}, true},
		{"empty string first element", []string{"bash", ""}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validShellArgv(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "shell")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// Pins the parallel-branch copy of the post.shell check (parse.go's Step
// branch): validateStepEntries duplicates the post-shell validation across
// the parallel and concrete branches, so a top-level-step test alone would
// not catch a regression in the parallel copy.
func TestParse_ShellArgv_ParallelPostEmptyArrayRejected(t *testing.T) {
	y := `
apiVersion: unified-cd/v1
kind: Job
metadata: { name: j }
spec:
  steps:
    - parallel:
      - name: p1
        run: echo hi
        post:
          shell: []
          run: echo bye
`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell")
}
