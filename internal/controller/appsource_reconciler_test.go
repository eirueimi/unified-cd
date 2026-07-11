package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockAppSourceFetcher struct {
	sha           string
	shaErr        error
	files         map[string][]byte
	filesErr      error
	fetchDirCalls int
	resolveErr    error
	fetchErr      error
}

func (m *mockAppSourceFetcher) ResolveCommitSHA(_ context.Context, _, _, _, _ string) (string, error) {
	if m.resolveErr != nil {
		return "", m.resolveErr
	}
	return m.sha, m.shaErr
}
func (m *mockAppSourceFetcher) FetchDir(_ context.Context, _, _, _, _, _ string) (map[string][]byte, error) {
	m.fetchDirCalls++
	if m.fetchErr != nil {
		return nil, m.fetchErr
	}
	return m.files, m.filesErr
}

const appSourceSpecJSON = `{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/"}`

const jobYAML = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
spec:
  steps:
    - name: compile
      run: go build ./...
`

func TestReconciler_AppliesJobsFromGit(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	fetcher := &mockAppSourceFetcher{
		sha:   "abc123",
		files: map[string][]byte{"jobs/build.yaml": []byte(jobYAML)},
	}

	reconcileAppSources(ctx, pg, fetcher, nil)

	job, err := pg.GetJob(ctx, "build")
	require.NoError(t, err)
	assert.Equal(t, "build", job.Name)

	src, err := pg.GetAppSource(ctx, "my-src")
	require.NoError(t, err)
	assert.Equal(t, "abc123", src.LastCommit)
	assert.NotNil(t, src.LastSyncedAt)
}

// TestReconciler_SkipsInvalidFileButAppliesValidOnes verifies that a file which
// fails to apply (here: a job using the removed `needs:` field) is skipped with
// a warning while the other valid files in the same sync are still applied and
// the sync completes successfully.
func TestReconciler_SkipsInvalidFileButAppliesValidOnes(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	badYAML := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: bad-job
spec:
  steps:
    - name: a
      run: echo a
    - name: b
      needs: [a]
      run: echo b
`
	fetcher := &mockAppSourceFetcher{
		sha: "sha-mix",
		files: map[string][]byte{
			"jobs/build.yaml": []byte(jobYAML),
			"jobs/bad.yaml":   []byte(badYAML),
		},
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	// The valid job is applied.
	_, err = pg.GetJob(ctx, "build")
	require.NoError(t, err)
	// The invalid job (needs:) is skipped, not applied.
	_, err = pg.GetJob(ctx, "bad-job")
	require.Error(t, err)
	// The sync still completes successfully despite the skipped file.
	src, err := pg.GetAppSource(ctx, "my-src")
	require.NoError(t, err)
	assert.Equal(t, "sha-mix", src.LastCommit)
}

func TestReconciler_SkipsWhenSHAUnchanged(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "abc123", time.Now(), nil))

	fetcher := &mockAppSourceFetcher{
		sha:   "abc123",
		files: map[string][]byte{"jobs/build.yaml": []byte(jobYAML)},
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	assert.Equal(t, 0, fetcher.fetchDirCalls, "FetchDir should not be called when SHA unchanged")
}

func TestReconciler_PruneDeletesRemovedJobs(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	pruneSpec := `{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/","syncPolicy":{"prune":true}}`
	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(pruneSpec))
	require.NoError(t, err)

	_, _ = pg.UpsertJob(ctx, "old-job", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"x"}]}`))
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "old-sha", time.Now().Add(-10*time.Minute), []store.ResourceRef{{Kind: "Job", Name: "old-job"}}))

	fetcher := &mockAppSourceFetcher{
		sha:   "new-sha",
		files: map[string][]byte{"jobs/build.yaml": []byte(jobYAML)},
	}

	reconcileAppSources(ctx, pg, fetcher, nil)

	_, err = pg.GetJob(ctx, "build")
	require.NoError(t, err)

	_, err = pg.GetJob(ctx, "old-job")
	require.Error(t, err)
}

func TestReconciler_WarnOnlyWithoutPrune(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	_, _ = pg.UpsertJob(ctx, "old-job", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"x"}]}`))
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "old-sha", time.Now().Add(-10*time.Minute), []store.ResourceRef{{Kind: "Job", Name: "old-job"}}))

	fetcher := &mockAppSourceFetcher{
		sha:   "new-sha",
		files: map[string][]byte{"jobs/build.yaml": []byte(jobYAML)},
	}

	reconcileAppSources(ctx, pg, fetcher, nil)

	_, err = pg.GetJob(ctx, "old-job")
	require.NoError(t, err, "old-job should still exist when prune is false")
}

func TestReconciler_AppliesAllKinds(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "multi", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	files := map[string][]byte{
		"a-job.yaml":      []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j1\nspec:\n  agentSelector: [kind:docker]\n  steps:\n    - name: s\n      run: echo hi"),
		"b-schedule.yaml": []byte("apiVersion: unified-cd/v1\nkind: Schedule\nmetadata:\n  name: sc1\nspec:\n  cron: \"* * * * *\"\n  job: j1"),
		"c-webhook.yaml":  []byte("apiVersion: unified-cd/v1\nkind: WebhookReceiver\nmetadata:\n  name: wh1\nspec:\n  trigger:\n    job: j1\n  auth:\n    type: none"),
	}
	fetcher := &mockAppSourceFetcher{sha: "sha1", files: files}

	reconcileAppSources(ctx, pg, fetcher, nil)

	if _, err := pg.GetJob(ctx, "j1"); err != nil {
		t.Errorf("job not applied: %v", err)
	}
	if _, err := pg.GetSchedule(ctx, "sc1"); err != nil {
		t.Errorf("schedule not applied: %v", err)
	}
	_, err = pg.GetWebhookReceiver(ctx, "wh1")
	require.NoError(t, err)
	as, err := pg.GetAppSource(ctx, "multi")
	require.NoError(t, err)
	if len(as.ManagedResources) != 3 {
		t.Errorf("managed = %+v, want 3 entries", as.ManagedResources)
	}
}

func TestReconciler_PruneNonCascadeAppSource(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	// Parent manages a child AppSource; parent has prune enabled.
	parentSpec := `{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"apps/","syncPolicy":{"prune":true}}`
	_, err := pg.UpsertAppSource(ctx, "parent-prune", []byte(parentSpec))
	require.NoError(t, err)

	childDoc := []byte("apiVersion: unified-cd/v1\nkind: AppSource\nmetadata:\n  name: child\nspec:\n  repoURL: https://x/y\n  targetRevision: main\n  path: jobs")

	fetcher := &mockAppSourceFetcher{sha: "sha1", files: map[string][]byte{"child.yaml": childDoc}}
	reconcileAppSources(ctx, pg, fetcher, nil)

	// Confirm the child AppSource was created by the first sync.
	_, err = pg.GetAppSource(ctx, "child")
	require.NoError(t, err)

	// Give the child a managed Job directly, to prove non-cascade deletion.
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "child", "x", time.Now(), []store.ResourceRef{{Kind: "Job", Name: "orphan"}}))
	_, err = pg.UpsertJob(ctx, "orphan", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)

	// Back-date the parent's last sync so the second sync isn't skipped by the interval check.
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "parent-prune", "sha1", time.Now().Add(-10*time.Minute), []store.ResourceRef{{Kind: "AppSource", Name: "child"}}))

	// Second sync: child removed from Git -> parent prunes the child AppSource (but not its resources).
	fetcher2 := &mockAppSourceFetcher{sha: "sha2", files: map[string][]byte{}}
	reconcileAppSources(ctx, pg, fetcher2, nil)

	if _, err := pg.GetAppSource(ctx, "child"); err == nil {
		t.Error("child AppSource should be pruned")
	}
	if _, err := pg.GetJob(ctx, "orphan"); err != nil {
		t.Error("non-cascade violated: child's Job must NOT be deleted")
	}
}

func TestReconciler_ForceSyncOnEmptyCommit(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "abc123", time.Now(), nil))
	require.NoError(t, pg.ResetAppSourceCommit(ctx, "my-src"))

	fetcher := &mockAppSourceFetcher{
		sha:   "abc123",
		files: map[string][]byte{"jobs/build.yaml": []byte(jobYAML)},
	}

	reconcileAppSources(ctx, pg, fetcher, nil)

	_, err = pg.GetJob(ctx, "build")
	require.NoError(t, err, "should sync when last_commit is empty (forced sync)")
}

func TestReconcile_RecordsFailedStatusOnError(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	fetcher := &mockAppSourceFetcher{
		resolveErr: fmt.Errorf("auth denied"),
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	src, err := pg.GetAppSource(ctx, "my-src")
	require.NoError(t, err)
	assert.Equal(t, "Failed", src.SyncStatus)
	assert.Contains(t, src.LastError, "auth denied")
}

func TestReconciler_DuplicateResourceFirstWins(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "dup-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	files := map[string][]byte{
		"a.yaml": []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: dup\nspec:\n  agentSelector: [kind:docker]\n  steps:\n    - name: s\n      run: echo A"),
		"b.yaml": []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: dup\nspec:\n  agentSelector: [kind:docker]\n  steps:\n    - name: s\n      run: echo B"),
	}
	fetcher := &mockAppSourceFetcher{sha: "sha1", files: files}

	reconcileAppSources(ctx, pg, fetcher, nil)

	job, err := pg.GetJob(ctx, "dup")
	require.NoError(t, err, "exactly one Job named dup should exist")
	// Duplicate {kind,name} resources are deduped BEFORE applyResource writes to
	// the store, so the lexicographically-first file (a.yaml) wins the stored spec.
	assert.Contains(t, string(job.Spec), "echo A", "first file (a.yaml) should win the stored spec")
	assert.NotContains(t, string(job.Spec), "echo B", "second file (b.yaml) must never reach the store")

	as, err := pg.GetAppSource(ctx, "dup-src")
	require.NoError(t, err)
	require.Len(t, as.ManagedResources, 1, "ManagedResources should contain exactly one entry for the duplicate")
	assert.Equal(t, store.ResourceRef{Kind: "Job", Name: "dup"}, as.ManagedResources[0])
}

// TestReconciler_LegacyBareJobNameNotPrunedOnUpgrade is the regression test for
// bug #25 (data loss). Before commit 51ce318, a job in a subdirectory
// (jobs/team-a/build.yaml) was stored under the BARE name "build" and recorded in
// managed_resources as {Job,"build"}. After the upgrade the reconciler computes the
// desired set with the QUALIFIED name "team-a/build". The prune loop must recognize
// that the legacy bare {Job,"build"} entry and the new {Job,"team-a/build"} entry are
// the SAME resource merely re-keyed, and must NOT delete the live job.
func TestReconciler_LegacyBareJobNameNotPrunedOnUpgrade(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	// prune:true is the dangerous case: without the guard the job is deleted+recreated,
	// breaking run history and any Schedule/WebhookReceiver referencing the bare name.
	pruneSpec := `{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/","syncPolicy":{"prune":true}}`
	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(pruneSpec))
	require.NoError(t, err)

	// Seed the store as an upgraded DB would look after the 003 migration backfill:
	// the live job is stored under the BARE name, and managed_resources records it bare.
	// We stamp a recognizable spec so we can prove the row is never delete+recreated.
	const seededSpec = `{"steps":[{"name":"legacy","run":"legacy-marker"}]}`
	_, err = pg.UpsertJob(ctx, "build", "unified-cd/v1", []byte(seededSpec))
	require.NoError(t, err)
	// A run recorded against the BARE legacy name; it must be repointed to the
	// qualified name so run history survives the re-key (bug #25 follow-up).
	legacyRun, err := pg.CreateRun(ctx, "build", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "old-sha", time.Now().Add(-10*time.Minute),
		[]store.ResourceRef{{Kind: "Job", Name: "build"}}))

	// Git tree now has the SAME file in a subdirectory; the desired name is qualified.
	job := func(name string) []byte {
		return []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: " + name +
			"\nspec:\n  steps:\n    - name: c\n      run: \"true\"\n")
	}
	fetcher := &mockAppSourceFetcher{
		sha:   "new-sha",
		files: map[string][]byte{"jobs/team-a/build.yaml": job("build")},
	}

	reconcileAppSources(ctx, pg, fetcher, nil)

	// PROPERTY 1 (non-negotiable): the live job must NOT be deleted.
	// The current git content is now applied under the qualified name "team-a/build".
	qj, err := pg.GetJob(ctx, "team-a/build")
	require.NoError(t, err, "qualified job team-a/build must exist after upgrade sync")
	assert.Equal(t, "team-a/build", qj.Name)

	// PROPERTY 2 (true in-place rename, bug #25 follow-up): the bare orphan row is
	// now REMOVED. The re-key completes at the store level via RenameJob, which
	// repoints run history onto the (already-applied) qualified row and then deletes
	// the bare orphan — so there is exactly ONE row under the qualified name, and no
	// lingering orphan. The "never delete a live job" guarantee is kept because the
	// qualified row exists before the bare row is dropped.
	_, err = pg.GetJob(ctx, "build")
	require.Error(t, err, "bare orphan must be removed after in-place rename")

	// Run history recorded against the bare name must now reference the qualified name.
	gotRun, err := pg.GetRun(ctx, legacyRun.ID)
	require.NoError(t, err)
	assert.Equal(t, "team-a/build", gotRun.JobName, "run history must be repointed to the qualified name")

	// PROPERTY 3 (idempotent + clean state): managed_resources ends up in the new
	// qualified form so subsequent syncs no longer see a bare prev entry.
	src, err := pg.GetAppSource(ctx, "my-src")
	require.NoError(t, err)
	require.Len(t, src.ManagedResources, 1)
	assert.Equal(t, store.ResourceRef{Kind: "Job", Name: "team-a/build"}, src.ManagedResources[0],
		"managed_resources must be rewritten to the qualified name")

	// A second sync with the same tree must be a no-op prune-wise (idempotent): the
	// qualified job survives and nothing is deleted.
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "", time.Now().Add(-10*time.Minute), src.ManagedResources))
	fetcher2 := &mockAppSourceFetcher{
		sha:   "new-sha",
		files: map[string][]byte{"jobs/team-a/build.yaml": job("build")},
	}
	reconcileAppSources(ctx, pg, fetcher2, nil)
	_, err = pg.GetJob(ctx, "team-a/build")
	require.NoError(t, err, "qualified job must survive a second idempotent sync")
}

// TestReconciler_LegacyBareGuardDoesNotSpareGenuinelyRemovedJob guards the collision
// heuristic: the legacy leaf-match must ONLY spare a bare prev entry when a qualified
// seen entry has the SAME leaf. A bare prev entry whose leaf appears NOWHERE in the
// desired set is a genuine removal and must still be pruned.
func TestReconciler_LegacyBareGuardDoesNotSpareGenuinelyRemovedJob(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	pruneSpec := `{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/","syncPolicy":{"prune":true}}`
	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(pruneSpec))
	require.NoError(t, err)

	// "gone" is a bare legacy entry whose leaf ("gone") is not present in git anymore.
	_, err = pg.UpsertJob(ctx, "gone", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"x"}]}`))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "old-sha", time.Now().Add(-10*time.Minute),
		[]store.ResourceRef{{Kind: "Job", Name: "gone"}}))

	job := func(name string) []byte {
		return []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: " + name +
			"\nspec:\n  steps:\n    - name: c\n      run: \"true\"\n")
	}
	fetcher := &mockAppSourceFetcher{
		sha:   "new-sha",
		files: map[string][]byte{"jobs/team-a/build.yaml": job("build")},
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	_, err = pg.GetJob(ctx, "gone")
	require.Error(t, err, "a genuinely-removed legacy job must still be pruned")
}

func TestReconciler_DirectoryBecomesQualifiedName(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	job := func(name string) []byte {
		return []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: " + name +
			"\nspec:\n  steps:\n    - name: c\n      run: \"true\"\n")
	}
	fetcher := &mockAppSourceFetcher{
		sha: "sha1",
		files: map[string][]byte{
			"jobs/team-a/build.yaml": job("build"),
			"jobs/team-b/build.yaml": job("build"),
			"jobs/root.yaml":         job("root"),
		},
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	for _, want := range []string{"team-a/build", "team-b/build", "root"} {
		j, err := pg.GetJob(ctx, want)
		require.NoErrorf(t, err, "expected job %q", want)
		assert.Equal(t, want, j.Name)
	}
}

// TestReconciler_SchedulesAppliedAfterJobsInSortedTree is the regression test
// for the sorted-order FK wedge: schedules/nightly.yaml (a Schedule
// referencing job "team-a/build") sorts lexicographically BEFORE
// team-a/build.yaml (the Job it references). Applying strictly in sorted-path
// order therefore tries to UpsertSchedule before the referenced job exists,
// which hits the schedules.job_name FK and returns errStoreWrite, aborting the
// whole sync before last_commit is ever set — the same failure repeats every
// tick, forever. The fix applies GitCredential/Job files before
// Schedule/WebhookReceiver/AppSource/unknown-kind files within a single sync,
// regardless of path sort order, so the tree exported by `unified-cli export`
// (which places schedules/ alongside qualified job paths) always syncs in one
// pass.
func TestReconciler_SchedulesAppliedAfterJobsInSortedTree(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	jobDoc := []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: build\nspec:\n  steps:\n    - name: c\n      run: \"true\"\n")
	scheduleDoc := []byte("apiVersion: unified-cd/v1\nkind: Schedule\nmetadata:\n  name: nightly\nspec:\n  cron: \"0 3 * * *\"\n  job: team-a/build\n")

	fetcher := &mockAppSourceFetcher{
		sha: "sha1",
		files: map[string][]byte{
			// Lexicographically, "schedules/nightly.yaml" < "team-a/build.yaml",
			// so a naive sorted-order apply would try the Schedule (and its FK on
			// job_name) before the Job exists.
			"schedules/nightly.yaml": scheduleDoc,
			"team-a/build.yaml":      jobDoc,
		},
	}

	reconcileAppSources(ctx, pg, fetcher, nil)

	_, err = pg.GetJob(ctx, "team-a/build")
	require.NoError(t, err, "job must be applied")
	_, err = pg.GetSchedule(ctx, "nightly")
	require.NoError(t, err, "schedule must be applied in the same sync as the job it references")

	src, err := pg.GetAppSource(ctx, "src")
	require.NoError(t, err)
	assert.Equal(t, "sha1", src.LastCommit, "sync must succeed (last_commit set) in one pass")
	assert.NotEqual(t, "Failed", src.SyncStatus)
	require.Len(t, src.ManagedResources, 2)
}

// TestRedactURLCredentials covers bug #33: credentials embedded in a repoURL
// (https://user:secret@host/...) must never leak into the persisted last_error.
func TestRedactURLCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "user and password",
			in:   `failed to clone https://alice:s3cr3t@github.com/org/repo.git: not found`,
			want: `failed to clone https://***:***@github.com/org/repo.git: not found`,
		},
		{
			name: "user only",
			in:   `error at ssh://git@example.com/x while syncing`,
			want: `error at ssh://***@example.com/x while syncing`,
		},
		{
			name: "no credentials untouched",
			in:   `failed to resolve commit SHA: https://github.com/org/repo not found`,
			want: `failed to resolve commit SHA: https://github.com/org/repo not found`,
		},
		{
			name: "token in userinfo",
			in:   `dial https://ghp_ABC123token@github.com/org/repo: timeout`,
			want: `dial https://***@github.com/org/repo: timeout`,
		},
		{
			name: "plain message no url",
			in:   `auth denied`,
			want: `auth denied`,
		},
		{
			name: "multiple urls",
			in:   `https://u:p@a.com/x and https://x:y@b.com/z both failed`,
			want: `https://***:***@a.com/x and https://***:***@b.com/z both failed`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactURLCredentials(c.in)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestReconciler_RedactsCredentialsInLastError proves the redaction is wired into the
// reconciler error path: a repoURL with embedded credentials that fails to resolve
// must not leak the secret into the stored last_error.
func TestReconciler_RedactsCredentialsInLastError(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	// Note: parse-side validation is intentionally left permissive (see #33); the spec
	// is stored via UpsertAppSource which does not re-validate, so the credentialed URL
	// reaches the reconciler and its error string.
	spec := `{"repoURL":"https://alice:s3cr3t@github.com/org/repo","targetRevision":"main","path":"jobs/"}`
	_, err := pg.UpsertAppSource(ctx, "cred-src", []byte(spec))
	require.NoError(t, err)

	fetcher := &mockAppSourceFetcher{
		resolveErr: fmt.Errorf("clone https://alice:s3cr3t@github.com/org/repo failed"),
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	src, err := pg.GetAppSource(ctx, "cred-src")
	require.NoError(t, err)
	assert.Equal(t, "Failed", src.SyncStatus)
	assert.NotContains(t, src.LastError, "s3cr3t", "password must be redacted from last_error")
	assert.NotContains(t, src.LastError, "alice", "username must be redacted from last_error")
	assert.Contains(t, src.LastError, "***", "redaction marker should be present")
}
