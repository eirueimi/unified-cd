package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jobWithSteps(body string) string {
	return "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n" + body
}

func TestParse_RejectsReservedStepEnv(t *testing.T) {
	cases := map[string]string{
		"step": `  steps:
    - name: a
      run: echo
      env:
        UNIFIED_RUN_ID: x
`,
		"finally": `  steps:
    - name: a
      run: echo
  finally:
    - name: f
      run: echo
      env:
        UNIFIED_RUN_ID: x
`,
		"parallel": `  steps:
    - parallel:
        - name: p
          run: echo
          env:
            UNIFIED_RUN_ID: x
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(jobWithSteps(body)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), `uses reserved prefix "UNIFIED_RUN_"`)
		})
	}
}

func TestParse_ReservedStepEnvIsCaseInsensitive(t *testing.T) {
	_, err := Parse(strings.NewReader(jobWithSteps(`  steps:
    - name: a
      run: echo
      env:
        unified_step_x: y
`)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `step "a": env "unified_step_x" uses reserved prefix "UNIFIED_STEP_"`)
}

func TestParse_AllowsOtherUnifiedEnv(t *testing.T) {
	_, err := Parse(strings.NewReader(jobWithSteps(`  steps:
    - name: a
      run: echo
      env:
        UNIFIED_RUNNER: x
`)))
	require.NoError(t, err)
}

func TestJobTemplateValidate_RejectsReservedStepEnv(t *testing.T) {
	tpl := &JobTemplate{
		APIVersion: SupportedAPIVersion, Kind: "JobTemplate",
		Metadata: Metadata{Name: "t"},
		Spec: JobTemplateSpec{Steps: []StepEntry{
			{Name: "a", Run: "echo", Env: map[string]string{"UNIFIED_JOB_NAME": "x"}},
		}},
	}
	err := tpl.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `uses reserved prefix "UNIFIED_JOB_"`)
}

func TestValidateVarKeys_RejectsNewPrefixes(t *testing.T) {
	err := ValidateVarKeys(map[string]string{"UNIFIED_JOB_X": "1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"UNIFIED_JOB_X" is reserved`)
}
