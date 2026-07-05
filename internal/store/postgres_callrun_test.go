package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustCreateRun creates a job named jobName (if not already present) and a Run
// for it, returning the new run's ID. Mirrors the UpsertJob+CreateRun pattern
// used throughout the other store tests.
func mustCreateRun(t *testing.T, p *Postgres, jobName string) string {
	t.Helper()
	ctx := t.Context()

	_, err := p.UpsertJob(ctx, jobName, "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	run, err := p.CreateRun(ctx, jobName, nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	return run.ID
}

func TestStepReport_ChildRunLink(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	p := NewTestPostgres(t)
	ctx := t.Context()

	// a parent run whose step calls a child, and the child run itself
	parent := mustCreateRun(t, p, "parent-job")
	child := mustCreateRun(t, p, "child-job")

	// report the call step with the child link
	require.NoError(t, p.UpsertStepReport(ctx, parent, 0, 0, "call-child", "", "Succeeded", nil, nil, nil, child, "child-job"))

	// forward: parent's steps carry the child link
	steps, err := p.GetRunSteps(ctx, parent)
	require.NoError(t, err)
	require.Len(t, steps, 1)
	assert.Equal(t, child, steps[0].ChildRunID)
	assert.Equal(t, "child-job", steps[0].CallJobName)

	// reverse: the child resolves its caller
	cb, err := p.GetRunParent(ctx, child)
	require.NoError(t, err)
	require.NotNil(t, cb)
	assert.Equal(t, parent, cb.ParentRunID)
	assert.Equal(t, "parent-job", cb.ParentJobName)
	assert.Equal(t, "call-child", cb.StepName)

	// a run not created by a call has no parent
	cbNone, err := p.GetRunParent(ctx, parent)
	require.NoError(t, err)
	assert.Nil(t, cbNone)
}
