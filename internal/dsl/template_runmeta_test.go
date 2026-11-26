package dsl

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandStepTemplate_RunFields(t *testing.T) {
	data := TemplateData{
		Run:   RunData{ID: "r1", CreatedAt: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)},
		Job:   JobData{Name: "j", Labels: map[string]string{"team": "a"}},
		Step:  CurrentStepData{Name: "s", Attempt: 2},
		Agent: AgentData{ID: "ag1"},
	}
	out, err := ExpandStepTemplate(`{{ .Run.ID }} {{ .Job.Labels.team }} {{ .Step.Attempt }} {{ .Agent.ID }} {{ .Run.CreatedAt.Format "2006-01-02" }}`, data, "")
	require.NoError(t, err)
	assert.Equal(t, "r1 a 2 ag1 2026-11-26", out)
}

func TestExpandStepTemplate_StepStatus(t *testing.T) {
	data := TemplateData{Steps: map[string]StepData{"build": {Status: "Failed", ExitCode: 3}}}
	out, err := ExpandStepTemplate(`{{ .Steps.build.Status }}/{{ .Steps.build.ExitCode }}`, data, "")
	require.NoError(t, err)
	assert.Equal(t, "Failed/3", out)
}

func TestIsReservedVarName_NewPrefixes(t *testing.T) {
	for _, n := range []string{"UNIFIED_RUN_ID", "unified_job_name", "UNIFIED_STEP_X"} {
		assert.True(t, IsReservedVarName(n), n)
	}
	assert.False(t, IsReservedVarName("UNIFIED_RUNNER"))
}
