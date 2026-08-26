package controller

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jobWithGoTemplateIf is a job that applied cleanly before apply-time `if:`
// validation existed. Its condition is Go-template syntax, which is not CEL —
// at run time it fails to compile, EvalCondition is fail-safe, and the step
// RUNS. That is the bug the apply-time check fixes. This file is what such a
// job looks like sitting in a Git repository at upgrade time.
const jobWithGoTemplateIf = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: legacy
spec:
  steps:
    - name: deploy
      if: '{{ eq .Params.env "production" }}'
      run: ./deploy.sh
`

// The apply-time `if:` check does not merely stop a Git-managed job from being
// UPDATED — with syncPolicy.prune, it gets the job DELETED.
//
// The chain: dsl.Parse now rejects the file, applyTrackedResource returns the
// error, and the reconcile loop takes the skip-one-file branch — which
// `continue`s BEFORE `seen[ref] = true`. finishSync then prunes every
// previously-managed resource missing from `seen`, and this job is missing
// from it for a reason that has nothing to do with the file being removed from
// Git. The file is still there. The job is gone.
//
// This test exists because the migration guide
// (docs/operator-manual/migrations/apply-time-if-validation.md) makes exactly
// that claim, and a migration guide that overstates a consequence is as bad as
// one that understates it.
func TestReconciler_InvalidIfDeletesTheJobUnderPrune(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	pruneSpec := `{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/","syncPolicy":{"prune":true}}`
	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(pruneSpec))
	require.NoError(t, err)

	// The job as it exists today: applied by an earlier version, live, and
	// recorded as managed by this AppSource.
	_, err = pg.UpsertJob(ctx, "legacy", "unified-cd/v1", []byte(`{"steps":[{"name":"deploy","run":"./deploy.sh"}]}`))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "old-sha", time.Now().Add(-10*time.Minute),
		[]store.ResourceRef{{Kind: "Job", Name: "legacy"}}))

	// The upgrade happens. Git is unchanged — the same file is still there.
	fetcher := &mockAppSourceFetcher{
		sha:   "new-sha",
		files: map[string][]byte{"jobs/legacy.yaml": []byte(jobWithGoTemplateIf)},
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	_, err = pg.GetJob(ctx, "legacy")
	assert.Error(t, err,
		"a job whose if: no longer validates is pruned as though it had been removed from Git")
}

// Without prune the same job is not deleted — it silently stops being updated
// from Git while the old stored spec keeps running. Both halves are in the
// migration guide, so both are pinned: the consequence an operator faces
// depends entirely on a syncPolicy flag they may not remember setting.
func TestReconciler_InvalidIfStopsUpdatesWithoutPrune(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	_, err = pg.UpsertJob(ctx, "legacy", "unified-cd/v1", []byte(`{"steps":[{"name":"deploy","run":"./old-deploy.sh"}]}`))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "old-sha", time.Now().Add(-10*time.Minute),
		[]store.ResourceRef{{Kind: "Job", Name: "legacy"}}))

	fetcher := &mockAppSourceFetcher{
		sha:   "new-sha",
		files: map[string][]byte{"jobs/legacy.yaml": []byte(jobWithGoTemplateIf)},
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	job, err := pg.GetJob(ctx, "legacy")
	require.NoError(t, err, "without prune the job survives")
	assert.Contains(t, string(job.Spec), "old-deploy.sh",
		"the stored spec is the OLD one: the file was skipped, so nothing from Git reached the store")
}
