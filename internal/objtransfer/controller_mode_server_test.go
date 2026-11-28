package objtransfer_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/agentauth"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/objtransfer"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
)

// localEnv is a real controller (with Postgres) in front of a local filesystem
// store, which cannot presign.
type localEnv struct {
	ts       *httptest.Server
	runIDs   []string
	token    string
	mu       sync.Mutex
	paths    []string
	presigns atomic.Int32
}

func newLocalEnv(t *testing.T) *localEnv {
	t.Helper()
	pg := storetest.NewTestPostgres(t)
	srv := newAdminServer(t, pg)
	t.Cleanup(srv.Close)
	srv.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))

	e := &localEnv{}
	e.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.paths = append(e.paths, r.Method+" "+r.URL.Path)
		e.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/objects/presign") {
			e.presigns.Add(1)
		}
		srv.Router().ServeHTTP(w, r)
	}))
	t.Cleanup(e.ts.Close)

	const agentID = "agent-local"
	issued, err := agentauth.Generate(agentauth.AccessToken)
	require.NoError(t, err)
	_, err = pg.IssueExternalAgentAccess(t.Context(), store.AgentCredentialIssue{
		AgentID: agentID, EnrollmentMethod: "test", ExternalSubject: "test:" + agentID,
		Access: store.NewAgentCredential{ID: issued.ID, Kind: "access", TokenHash: issued.Hash, ExpiresAt: time.Now().Add(time.Hour)},
	})
	require.NoError(t, err)
	e.token = issued.Plaintext

	spec := []byte(`{}`)
	_, err = pg.UpsertJob(t.Context(), "local-job", "unified-cd/v1", spec, store.JobMeta{})
	require.NoError(t, err)
	for range 2 {
		run, err := pg.CreateRun(t.Context(), "local-job", nil, spec, nil, nil, "", "", store.RunOrigin{})
		require.NoError(t, err)
		_, err = pg.TransitionPendingToQueued(t.Context(), 100)
		require.NoError(t, err)
		claimed, err := pg.ClaimNextRun(t.Context(), agentID, nil, nil)
		require.NoError(t, err)
		require.Equal(t, run.ID, claimed.ID)
		e.runIDs = append(e.runIDs, run.ID)
	}
	return e
}

func (e *localEnv) client(t *testing.T, runID string, force bool) *objtransfer.Client {
	return &objtransfer.Client{
		BaseURL: e.ts.URL, RunID: runID, TempDir: t.TempDir(), ForceController: force,
		Auth: func(_ context.Context, r *http.Request) error {
			r.Header.Set("Authorization", "Bearer "+e.token)
			return nil
		},
	}
}

func roundTrip(t *testing.T, e *localEnv, c1, c2 *objtransfer.Client) {
	t.Helper()
	data := bytes.Repeat([]byte("artifact-bytes-"), 1<<14)
	key := "artifacts/" + e.runIDs[0] + "/out.tar.gz"
	require.NoError(t, c1.Artifacts().Put(t.Context(), key, bytes.NewReader(data), -1))
	for _, c := range []*objtransfer.Client{c1, c2} {
		rc, err := c.Artifacts().Get(t.Context(), key)
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		require.Equal(t, data, got)
	}

	archive := bytes.Repeat([]byte("cache-bytes-"), 1<<14)
	require.NoError(t, c1.Caches().Save(t.Context(), "deps-v1-abc", 5, bytes.NewReader(archive)))
	rc, matched, err := c2.Caches().Open(t.Context(), "deps-v2-zzz", []string{"deps-v1-"})
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, "deps-v1-abc", matched)
	require.Equal(t, archive, got)

	_, _, err = c2.Caches().Open(t.Context(), "absent", []string{"nothing-"})
	require.ErrorIs(t, err, cache.ErrCacheMiss)
}

func TestControllerMode_AutoFromPresignAnswer(t *testing.T) {
	e := newLocalEnv(t)
	c1, c2 := e.client(t, e.runIDs[0], false), e.client(t, e.runIDs[1], false)
	roundTrip(t, e, c1, c2)

	// Each client asked once and remembered the answer; every other request
	// went to a streaming route. There is no store URL anywhere to request.
	require.EqualValues(t, 2, e.presigns.Load(), "one presign per Client")
	e.mu.Lock()
	defer e.mu.Unlock()
	streamed := 0
	for _, p := range e.paths {
		if strings.Contains(p, "/objects/caches") || strings.Contains(p, "/objects/artifacts/") {
			streamed++
		}
	}
	require.Positive(t, streamed)
}

func TestControllerMode_ForcedSkipsPresignAgainstRealController(t *testing.T) {
	e := newLocalEnv(t)
	roundTrip(t, e, e.client(t, e.runIDs[0], true), e.client(t, e.runIDs[1], true))
	require.EqualValues(t, 0, e.presigns.Load(), "ForceController must never call the presign endpoint")
}
