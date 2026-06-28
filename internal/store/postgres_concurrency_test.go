package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_MutexAcquireAndRelease(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run1, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	run2, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	ok, err := pg.AcquireMutex(ctx, "deploy-prod", run1.ID)
	require.NoError(t, err)
	assert.True(t, ok)

	ok2, err := pg.AcquireMutex(ctx, "deploy-prod", run2.ID)
	require.NoError(t, err)
	assert.False(t, ok2)

	require.NoError(t, pg.ReleaseMutex(ctx, "deploy-prod"))

	ok3, err := pg.AcquireMutex(ctx, "deploy-prod", run2.ID)
	require.NoError(t, err)
	assert.True(t, ok3)
}

func TestPostgres_SemaphorePool(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run1, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	run2, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	run3, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	require.NoError(t, pg.UpsertSemaphorePool(ctx, "tokens", 2))

	ok1, err := pg.AcquireSemaphore(ctx, "tokens", run1.ID)
	require.NoError(t, err)
	assert.True(t, ok1)

	ok2, err := pg.AcquireSemaphore(ctx, "tokens", run2.ID)
	require.NoError(t, err)
	assert.True(t, ok2)

	ok3, err := pg.AcquireSemaphore(ctx, "tokens", run3.ID)
	require.NoError(t, err)
	assert.False(t, ok3)

	require.NoError(t, pg.ReleaseSemaphore(ctx, "tokens", run1.ID))

	ok4, err := pg.AcquireSemaphore(ctx, "tokens", run3.ID)
	require.NoError(t, err)
	assert.True(t, ok4)
}

func TestPostgres_TransitionPendingToQueued_WithMutex(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	specWithMutex := []byte(`{"concurrency":{"mutex":"deploy-prod"}}`)
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run1, _ := pg.CreateRun(ctx, "j", nil, specWithMutex, nil, "")
	run2, _ := pg.CreateRun(ctx, "j", nil, specWithMutex, nil, "")

	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	r1, _ := pg.GetRun(ctx, run1.ID)
	r2, _ := pg.GetRun(ctx, run2.ID)
	assert.Equal(t, "Queued", string(r1.Status))
	assert.Equal(t, "Pending", string(r2.Status))
}

func TestPostgres_MarkRunFinished_ReleasesMutex(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	specWithMutex := []byte(`{"concurrency":{"mutex":"deploy-prod"}}`)
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run1, _ := pg.CreateRun(ctx, "j", nil, specWithMutex, nil, "")
	run2, _ := pg.CreateRun(ctx, "j", nil, specWithMutex, nil, "")

	_, _ = pg.TransitionPendingToQueued(ctx, 10)
	_, _ = pg.ClaimNextRun(ctx, "agent-1", nil)
	require.NoError(t, pg.MarkRunRunning(ctx, run1.ID))
	require.NoError(t, pg.MarkRunFinished(ctx, run1.ID, "Succeeded"))

	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	r2, _ := pg.GetRun(ctx, run2.ID)
	assert.Equal(t, "Queued", string(r2.Status))
}

func TestAcquireAdvisoryLock_AcquireAndRelease(t *testing.T) {
	pg := NewTestPostgres(t)
	const key = int64(0x74657374) // 'test' — test-only key

	// acquire succeeds
	release, err := pg.AcquireAdvisoryLock(t.Context(), key)
	require.NoError(t, err)
	require.NotNil(t, release, "should successfully acquire the lock")

	// trying the same key from another session (a different connection from the same pool) should fail
	release2, err2 := pg.AcquireAdvisoryLock(t.Context(), key)
	require.NoError(t, err2)
	require.Nil(t, release2, "should not acquire because another session holds the lock")

	// re-acquire is possible after release
	release()

	release3, err3 := pg.AcquireAdvisoryLock(t.Context(), key)
	require.NoError(t, err3)
	require.NotNil(t, release3, "should be able to re-acquire after release")
	release3()
}

func TestPostgres_TransitionPendingToQueued_ExpandsTemplatedMutex(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	specWithTemplatedMutex := []byte(`{"concurrency":{"mutex":"deploy-{{ .Params.env }}"}}`)
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run1, _ := pg.CreateRun(ctx, "j", map[string]string{"env": "prod"}, specWithTemplatedMutex, nil, "")
	run2, _ := pg.CreateRun(ctx, "j", map[string]string{"env": "prod"}, specWithTemplatedMutex, nil, "")

	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only one run should win the expanded mutex 'deploy-prod'")

	r1, _ := pg.GetRun(ctx, run1.ID)
	r2, _ := pg.GetRun(ctx, run2.ID)
	statuses := []string{string(r1.Status), string(r2.Status)}
	assert.Contains(t, statuses, "Queued")
	assert.Contains(t, statuses, "Pending")

	// A different env value must expand to a different, non-contending mutex name
	// ('deploy-staging' vs 'deploy-prod'). Without expansion, run3 would share the
	// same literal template string as run1/run2 and would NOT be able to queue.
	run3, _ := pg.CreateRun(ctx, "j", map[string]string{"env": "staging"}, specWithTemplatedMutex, nil, "")
	n2, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n2, "run3 should queue immediately since its expanded mutex is 'deploy-staging', not 'deploy-prod'")
	r3, _ := pg.GetRun(ctx, run3.ID)
	assert.Equal(t, "Queued", string(r3.Status))
}

func TestPostgres_TransitionPendingToQueued_ExpandsTemplatedSemaphorePool(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	specWithTemplatedPool := []byte(`{"concurrency":{"semaphores":[{"pool":"{{ .Params.env }}-tokens","capacity":1}]}}`)
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.CreateRun(ctx, "j", map[string]string{"env": "staging"}, specWithTemplatedPool, nil, "")
	_, _ = pg.CreateRun(ctx, "j", map[string]string{"env": "staging"}, specWithTemplatedPool, nil, "")

	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only one run should win the expanded pool 'staging-tokens' (capacity 1)")

	// A different env value expands to a different, unrelated pool — must not contend.
	run3, _ := pg.CreateRun(ctx, "j", map[string]string{"env": "prod"}, specWithTemplatedPool, nil, "")
	n2, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n2, "run3 should queue immediately since it expands to a different pool 'prod-tokens'")
	r3, _ := pg.GetRun(ctx, run3.ID)
	assert.Equal(t, "Queued", string(r3.Status))
}

func TestPostgres_TransitionPendingToQueued_BadTemplateFailsOnlyThatRun(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	badSpec := []byte(`{"concurrency":{"mutex":"deploy-{{ .Params.env"}}`) // missing closing }}
	goodSpec := []byte(`{}`)
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	badRun, _ := pg.CreateRun(ctx, "j", map[string]string{"env": "prod"}, badSpec, nil, "")
	goodRun, _ := pg.CreateRun(ctx, "j", nil, goodSpec, nil, "")

	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err, "a bad template must not abort the whole batch")
	assert.Equal(t, 1, n, "only the good run should be queued")

	br, _ := pg.GetRun(ctx, badRun.ID)
	gr, _ := pg.GetRun(ctx, goodRun.ID)
	assert.Equal(t, "Failed", string(br.Status))
	assert.Equal(t, "Queued", string(gr.Status))

	logs, err := pg.TailLogs(ctx, badRun.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, -1, logs[0].StepIndex)
	assert.Equal(t, "stderr", logs[0].Stream)
	assert.Contains(t, logs[0].Line, "concurrency template expansion failed")
	assert.Contains(t, logs[0].Line, "concurrency.mutex")
}

func TestPostgres_TransitionPendingToQueued_OrLockAcquiresFreeCandidate(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))

	// env-a is already held by another run; only env-b should be free.
	holder, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	ok, err := pg.AcquireMutex(ctx, "env-a", holder.ID)
	require.NoError(t, err)
	require.True(t, ok)

	specWithOrLock := []byte(`{"concurrency":{"orLocks":[{"name":"env","in":{"literal":["env-a","env-b"]}}]}}`)
	run, _ := pg.CreateRun(ctx, "j", nil, specWithOrLock, nil, "")

	// holder itself has no concurrency constraints, so it also queues trivially
	// in this same batch alongside run (which must win env-b); n covers both.
	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	r, _ := pg.GetRun(ctx, run.ID)
	assert.Equal(t, "Queued", string(r.Status))
	assert.Equal(t, "env-b", r.Params["ENV_LOCK_VALUE"])
}

func TestPostgres_TransitionPendingToQueued_OrLockAllCandidatesExhausted(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))

	holder1, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	holder2, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	_, err := pg.AcquireMutex(ctx, "env-a", holder1.ID)
	require.NoError(t, err)
	_, err = pg.AcquireMutex(ctx, "env-b", holder2.ID)
	require.NoError(t, err)

	specWithOrLock := []byte(`{"concurrency":{"orLocks":[{"name":"env","in":{"literal":["env-a","env-b"]}}]}}`)
	run, _ := pg.CreateRun(ctx, "j", nil, specWithOrLock, nil, "")

	// holder1 and holder2 have no concurrency constraints, so they queue trivially
	// in this same batch; n covers both holders, but run itself must stay Pending
	// since every candidate is exhausted.
	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "the two holder runs queue trivially; the OrLock run must not")

	r, _ := pg.GetRun(ctx, run.ID)
	assert.Equal(t, "Pending", string(r.Status), "no candidate is free, run must stay Pending")
}

func TestPostgres_TransitionPendingToQueued_OrLockParamConflictFailsRun(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	specWithOrLock := []byte(`{"concurrency":{"orLocks":[{"name":"env","in":{"literal":["env-a","env-b"]}}]}}`)
	run, _ := pg.CreateRun(ctx, "j", map[string]string{"ENV_LOCK_VALUE": "already-set"}, specWithOrLock, nil, "")

	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	r, _ := pg.GetRun(ctx, run.ID)
	assert.Equal(t, "Failed", string(r.Status))

	logs, err := pg.TailLogs(ctx, run.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, -1, logs[0].StepIndex)
	assert.Equal(t, "stderr", logs[0].Stream)
	assert.Contains(t, logs[0].Line, "ENV_LOCK_VALUE")
	assert.Contains(t, logs[0].Line, "conflicts")
}

func TestPostgres_MarkRunFinished_ReleasesOrLockCandidate(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	specWithOrLock := []byte(`{"concurrency":{"orLocks":[{"name":"env","in":{"literal":["env-a"]}}]}}`)
	run1, _ := pg.CreateRun(ctx, "j", nil, specWithOrLock, nil, "")
	run2, _ := pg.CreateRun(ctx, "j", nil, specWithOrLock, nil, "")

	_, _ = pg.TransitionPendingToQueued(ctx, 10)
	_, _ = pg.ClaimNextRun(ctx, "agent-1", nil)
	require.NoError(t, pg.MarkRunRunning(ctx, run1.ID))
	require.NoError(t, pg.MarkRunFinished(ctx, run1.ID, "Succeeded"))

	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	r2, _ := pg.GetRun(ctx, run2.ID)
	assert.Equal(t, "Queued", string(r2.Status))
	assert.Equal(t, "env-a", r2.Params["ENV_LOCK_VALUE"])
}
