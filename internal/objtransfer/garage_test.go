//go:build garage

package objtransfer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/agentauth"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/objectstore/objectstoretest"
	"github.com/eirueimi/unified-cd/internal/objtransfer"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
)

// TestGarage_ClientEndToEnd runs the client against a real controller (with
// Postgres) in front of a real Garage store: a 70 MiB stream (two parts at the
// real sizes), a cache save and a restore by restoreKeys prefix, and an
// artifact round trip including a read from another run.
func TestGarage_ClientEndToEnd(t *testing.T) {
	objs := objectstoretest.S3FromEnv(t)
	pg := storetest.NewTestPostgres(t)
	_, err := pg.UpsertBootstrapPAT(t.Context(), "test-bootstrap", controller.HashToken("secret"))
	require.NoError(t, err)
	srv := controller.NewServer(controller.Config{}, pg)
	t.Cleanup(srv.Close)
	srv.SetObjectStore(objs)
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	const agentID = "agent-e2e"
	issued, err := agentauth.Generate(agentauth.AccessToken)
	require.NoError(t, err)
	_, err = pg.IssueExternalAgentAccess(t.Context(), store.AgentCredentialIssue{
		AgentID: agentID, EnrollmentMethod: "test", ExternalSubject: "test:" + agentID,
		Access: store.NewAgentCredential{ID: issued.ID, Kind: "access", TokenHash: issued.Hash, ExpiresAt: time.Now().Add(time.Hour)},
	})
	require.NoError(t, err)

	// Two runs of one job, both claimed by the agent: the second reads the
	// first's artifact.
	spec := []byte(`{}`)
	_, err = pg.UpsertJob(t.Context(), "e2e-job", "unified-cd/v1", spec)
	require.NoError(t, err)
	var runIDs []string
	for range 2 {
		run, err := pg.CreateRun(t.Context(), "e2e-job", nil, spec, nil, nil, "", "")
		require.NoError(t, err)
		_, err = pg.TransitionPendingToQueued(t.Context(), 100)
		require.NoError(t, err)
		claimed, err := pg.ClaimNextRun(t.Context(), agentID, nil, nil)
		require.NoError(t, err)
		require.NotNil(t, claimed)
		require.Equal(t, run.ID, claimed.ID)
		runIDs = append(runIDs, run.ID)
	}

	newClient := func(runID string) *objtransfer.Client {
		return &objtransfer.Client{
			BaseURL: ts.URL, RunID: runID, TempDir: t.TempDir(),
			Auth: func(_ context.Context, r *http.Request) error {
				r.Header.Set("Authorization", "Bearer "+issued.Plaintext)
				return nil
			},
		}
	}
	c1, c2 := newClient(runIDs[0]), newClient(runIDs[1])

	// 70 MiB: part 1 is 64 MiB, part 2 is 6 MiB.
	big := make([]byte, 70<<20)
	rand.New(rand.NewSource(1)).Read(big)
	key1, err := artifactKey(runIDs[0], "big")
	require.NoError(t, err)
	require.NoError(t, c1.Artifacts().Put(t.Context(), key1, bytes.NewReader(big), -1))
	assertContent(t, c1.Artifacts(), key1, big)
	// Another run reads it with sourceRunId.
	assertContent(t, c2.Artifacts(), key1, big)

	// Cache: save, then restore by restoreKeys prefix.
	archive := bytes.Repeat([]byte("cache-bytes-"), 1<<16)
	require.NoError(t, c1.Caches().Save(t.Context(), "deps-v1-abc", 5, bytes.NewReader(archive)))
	rc, matched, err := c2.Caches().Open(t.Context(), "deps-v2-zzz", []string{"deps-v1-"})
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, "deps-v1-abc", matched)
	require.Equal(t, sha256.Sum256(archive), sha256.Sum256(got))
}

func artifactKey(runID, name string) (string, error) {
	return "artifacts/" + runID + "/" + name + ".tar.gz", nil
}

type getter interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

func assertContent(t *testing.T, s getter, key string, want []byte) {
	t.Helper()
	rc, err := s.Get(t.Context(), key)
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(want), sha256.Sum256(got))
}
