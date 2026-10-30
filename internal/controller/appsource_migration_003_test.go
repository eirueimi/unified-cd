package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAppSourceMigration003_ThroughFirstSync builds a genuine pre-003 database
// with the production migration runner, seeds pre-003 AppSource state (bare job
// names in app_sources.managed_jobs), applies migration 003 and the rest of the
// chain, and then drives the real reconciler's first and repeat syncs.
func TestAppSourceMigration003_ThroughFirstSync(t *testing.T) {
	ctx := context.Background()
	dsn := store.NewEmptyTestDatabase(t)

	// 1. Migrate to version 002 only: managed_jobs exists, managed_resources does not.
	require.NoError(t, (&store.Postgres{}).MigrateTo(dsn, 2))

	raw, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { raw.Close() })

	var hasManagedJobs, hasManagedResources bool
	require.NoError(t, raw.QueryRow(`SELECT
	  EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='app_sources' AND column_name='managed_jobs'),
	  EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='app_sources' AND column_name='managed_resources')`).
		Scan(&hasManagedJobs, &hasManagedResources))
	require.True(t, hasManagedJobs, "pre-003 schema must have managed_jobs")
	require.False(t, hasManagedResources, "pre-003 schema must not have managed_resources")

	// 2. Seed pre-003 rows via SQL: an AppSource managing the bare job "build",
	// that job with run history, and an unmanaged job with its own run.
	const legacySpec = `{"steps":[{"name":"legacy","run":"legacy-marker"}]}`
	const appSpec = `{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/","syncPolicy":{"prune":true}}`
	_, err = raw.Exec(`INSERT INTO app_sources (name, spec, last_commit, last_synced_at, managed_jobs)
	  VALUES ('my-src', $1::jsonb, 'old-sha', now() - interval '10 minutes', ARRAY['build'])`, appSpec)
	require.NoError(t, err)
	_, err = raw.Exec(`INSERT INTO jobs (name, api_version, spec) VALUES
	  ('build', 'unified-cd/v1', $1::jsonb), ('manual', 'unified-cd/v1', $1::jsonb)`, legacySpec)
	require.NoError(t, err)
	var buildRun1, buildRun2, manualRun string
	require.NoError(t, raw.QueryRow(`INSERT INTO runs (job_name, status, spec) VALUES ('build','Succeeded','{}') RETURNING id`).Scan(&buildRun1))
	require.NoError(t, raw.QueryRow(`INSERT INTO runs (job_name, status, spec) VALUES ('build','Failed','{}') RETURNING id`).Scan(&buildRun2))
	require.NoError(t, raw.QueryRow(`INSERT INTO runs (job_name, status, spec) VALUES ('manual','Succeeded','{}') RETURNING id`).Scan(&manualRun))

	// 3. Run migration 003 through the production runner and assert the
	// deliberate verbatim bare-name backfill, exactly.
	require.NoError(t, (&store.Postgres{}).MigrateTo(dsn, 3))
	var managedJSON string
	require.NoError(t, raw.QueryRow(`SELECT managed_resources::text FROM app_sources WHERE name='my-src'`).Scan(&managedJSON))
	var backfilled []map[string]string
	require.NoError(t, json.Unmarshal([]byte(managedJSON), &backfilled))
	assert.Equal(t, []map[string]string{{"kind": "Job", "name": "build"}}, backfilled,
		"migration 003 must backfill the bare stored name verbatim")
	require.NoError(t, raw.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='app_sources' AND column_name='managed_jobs')`).Scan(&hasManagedJobs))
	assert.False(t, hasManagedJobs, "managed_jobs must be dropped by 003")

	// 4. Apply the remaining migrations (with schema verification) and open the store.
	require.NoError(t, (&store.Postgres{}).Migrate(dsn))
	pg, err := store.NewPostgres(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pg.Close)

	src, err := pg.GetAppSource(ctx, "my-src")
	require.NoError(t, err)
	require.Equal(t, []store.ResourceRef{{Kind: "Job", Name: "build"}}, src.ManagedResources)

	// 5. First sync: Git now has the job in a subdirectory, so its desired name is qualified.
	job := []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: build\nspec:\n  steps:\n    - name: c\n      run: \"true\"\n")
	fetcher := &mockAppSourceFetcher{sha: "new-sha", files: map[string][]byte{"jobs/team-a/build.yaml": job}}
	reconcileAppSources(ctx, pg, fetcher, nil)

	// Qualification.
	qj, err := pg.GetJob(ctx, "team-a/build")
	require.NoError(t, err)
	assert.Equal(t, "team-a/build", qj.Name)
	_, err = pg.GetJob(ctx, "build")
	require.Error(t, err, "bare orphan must be gone after the first sync")

	src, err = pg.GetAppSource(ctx, "my-src")
	require.NoError(t, err)
	assert.Equal(t, []store.ResourceRef{{Kind: "Job", Name: "team-a/build"}}, src.ManagedResources)
	assert.Equal(t, "new-sha", src.LastCommit)

	// History preservation: both runs follow the rename.
	for _, id := range []string{buildRun1, buildRun2} {
		r, err := pg.GetRun(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "team-a/build", r.JobName)
	}

	// Prune safety: the unmanaged job and its run are untouched.
	mj, err := pg.GetJob(ctx, "manual")
	require.NoError(t, err)
	assert.Equal(t, "manual", mj.Name)
	mr, err := pg.GetRun(ctx, manualRun)
	require.NoError(t, err)
	assert.Equal(t, "manual", mr.JobName)

	first := snapshotJobsAndRuns(t, raw)
	assert.Equal(t, []string{"manual", "team-a/build"}, first.jobs)
	assert.Equal(t, map[string]string{buildRun1: "team-a/build", buildRun2: "team-a/build", manualRun: "manual"}, first.runs)

	// 6. Repeat sync: force a real re-sync (not a same-SHA skip) and require no change.
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "", time.Now().Add(-10*time.Minute), src.ManagedResources))
	fetcher.fetchDirCalls = 0
	reconcileAppSources(ctx, pg, fetcher, nil)
	require.Equal(t, 1, fetcher.fetchDirCalls, "second sync must actually run")

	assert.Equal(t, first, snapshotJobsAndRuns(t, raw), "repeat sync must change nothing")
	src, err = pg.GetAppSource(ctx, "my-src")
	require.NoError(t, err)
	assert.Equal(t, []store.ResourceRef{{Kind: "Job", Name: "team-a/build"}}, src.ManagedResources)
}

type jobsAndRuns struct {
	jobs []string
	runs map[string]string // run id -> job name
}

func snapshotJobsAndRuns(t *testing.T, db *sql.DB) jobsAndRuns {
	t.Helper()
	out := jobsAndRuns{runs: map[string]string{}}
	rows, err := db.Query(`SELECT name FROM jobs`)
	require.NoError(t, err)
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		out.jobs = append(out.jobs, n)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	sort.Strings(out.jobs)
	rows, err = db.Query(`SELECT id, job_name FROM runs`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id, n string
		require.NoError(t, rows.Scan(&id, &n))
		out.runs[id] = n
	}
	require.NoError(t, rows.Err())
	return out
}
