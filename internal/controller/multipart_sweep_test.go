package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func addUpload(f *fakePresigner, key string, initiated time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.uploads = append(f.uploads, objectstore.Upload{Key: key, UploadID: "u" + key, Initiated: initiated})
}

func TestAbortRunUploads_AbortsRunArtifactsAndJobCaches(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	otherRun := seedRun(t, st, "job-b", "")
	_, err := st.FinishRun(t.Context(), runID, api.RunSucceeded)
	require.NoError(t, err)

	now := time.Now()
	addUpload(fake, "artifacts/"+runID+"/a.tar.gz", now)
	addUpload(fake, cache.DataKey("job-a", "k"), now)
	addUpload(fake, "artifacts/"+otherRun+"/a.tar.gz", now)
	addUpload(fake, cache.DataKey("job-b", "k"), now)

	s.abortRunUploadsNow(t.Context(), runID, "job-a")
	assert.ElementsMatch(t, []string{"artifacts/" + runID + "/a.tar.gz", cache.DataKey("job-a", "k")}, fake.abortedKeys())
}

// The agent's finish report triggers the abort in the background; the
// request context ending must not cancel it.
func TestAbortRunUploads_CalledOnFinish(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	addUpload(fake, "artifacts/"+runID+"/a.tar.gz", time.Now())

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-a/runs/"+runID+"/finish", strings.NewReader(`{"status":"Succeeded"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, r)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.Eventually(t, func() bool { return len(fake.abortedKeys()) == 1 }, 5*time.Second, 10*time.Millisecond)
}

// Review Focus 1: two runs of one job share the cache prefix, so the end of
// one must not abort the other's in-flight cache upload.
func TestAbortRunUploads_SkipsCachePrefixWhileAnotherRunOfJobIsActive(t *testing.T) {
	s, st, fake := presignTestServer(t, Config{})
	runID := seedClaimedRun(t, st, "job-a", "agent-a", "")
	_ = seedRun(t, st, "job-a", "") // a second, still non-terminal run of job-a
	_, err := st.FinishRun(t.Context(), runID, api.RunSucceeded)
	require.NoError(t, err)

	now := time.Now()
	addUpload(fake, "artifacts/"+runID+"/a.tar.gz", now)
	addUpload(fake, cache.DataKey("job-a", "k"), now)

	s.abortRunUploadsNow(t.Context(), runID, "job-a")
	assert.Equal(t, []string{"artifacts/" + runID + "/a.tar.gz"}, fake.abortedKeys())
}

// fakeSweepStore is a store.Store stand-in for the multipart sweep.
type fakeSweepStore struct {
	store.Store
	lockAcquired bool
}

func (f *fakeSweepStore) AcquireAdvisoryLock(_ context.Context, key int64) (func(), error) {
	if key != multipartSweepLockKey {
		panic("unexpected lock key")
	}
	if !f.lockAcquired {
		return nil, nil
	}
	return func() {}, nil
}

func TestMultipartSweep_AbortsOnlyOlderThan24h(t *testing.T) {
	fake := newFakePresigner()
	now := time.Now()
	addUpload(fake, "artifacts/r1/old.tar.gz", now.Add(-25*time.Hour))
	addUpload(fake, "artifacts/r1/new.tar.gz", now.Add(-23*time.Hour))
	addUpload(fake, cache.DataKey("job-a", "old"), now.Add(-25*time.Hour))
	addUpload(fake, cache.DataKey("job-a", "new"), now.Add(-23*time.Hour))
	addUpload(fake, "logs/r1/old", now.Add(-25*time.Hour)) // outside the swept prefixes

	n, failed, err := runMultipartSweepAsLeader(t.Context(), &fakeSweepStore{lockAcquired: true}, fake, now)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, 0, failed)
	assert.ElementsMatch(t, []string{"artifacts/r1/old.tar.gz", cache.DataKey("job-a", "old")}, fake.abortedKeys())
}

func TestMultipartSweep_LeaderOnly(t *testing.T) {
	fake := newFakePresigner()
	addUpload(fake, "artifacts/r1/old.tar.gz", time.Now().Add(-48*time.Hour))
	n, _, err := runMultipartSweepAsLeader(t.Context(), &fakeSweepStore{lockAcquired: false}, fake, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Empty(t, fake.abortedKeys())
}

func TestMultipartSweepLockKeyIsUnique(t *testing.T) {
	assert.Equal(t, int64(0x6d707274), multipartSweepLockKey)
	for _, other := range []int64{approvalReaperLockKey, appSourceReconcilerLockKey, appSourceSyncReaperLockKey, logArchiverLockKey, auditRetentionLockKey, logTrimLockKey, queuedRunReaperLockKey, runRetentionLockKey, cacheCleanupLockKey, stuckRunReaperLockKey} {
		assert.NotEqual(t, other, multipartSweepLockKey)
	}
}
