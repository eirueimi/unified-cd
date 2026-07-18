package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getList(t *testing.T, s *Server, path string, v any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), v))
}

func TestAPI_ListSchedules_IncludesParams(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	_, err = pg.UpsertSchedule(ctx, "nightly", "0 3 * * *", "hello", map[string]string{"env": "prod"})
	require.NoError(t, err)

	var got []api.ScheduleMeta
	getList(t, s, "/api/v1/schedules", &got)
	require.Len(t, got, 1)
	assert.Equal(t, map[string]string{"env": "prod"}, got[0].Params)
}

func TestAPI_ListWebhooks_IncludesSpec(t *testing.T) {
	s, pg := newTestServer(t)
	// Seed with the REAL stored format: parse YAML through the dsl parser and
	// json.Marshal the resulting typed spec, exactly as applyResource does. Since
	// dsl.WebhookReceiverSpec only carries yaml tags, this produces capitalized
	// Go field names ("Trigger", "Auth", ...) — the same shape the store holds
	// in production, not a hand-written lowercase literal.
	wr, err := dsl.ParseWebhookReceiver(strings.NewReader(`
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: gh
spec:
  trigger:
    job: hello
  auth:
    type: none
    allowUnauthenticated: true
`))
	require.NoError(t, err)
	spec, err := json.Marshal(wr.Spec)
	require.NoError(t, err)

	_, err = pg.UpsertWebhookReceiver(context.Background(), "gh", spec)
	require.NoError(t, err)

	var got []api.WebhookReceiverMeta
	getList(t, s, "/api/v1/webhooks", &got)
	require.Len(t, got, 1)
	assert.JSONEq(t, string(spec), string(got[0].Spec))
}

func TestAPI_ListAppSources_IncludesSyncPolicyAndManaged(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "src1",
		[]byte(`{"repoURL":"https://x/y.git","targetRevision":"main","path":"jobs","syncPolicy":{"prune":true,"allowManualOverride":true}}`))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "src1", "sha", time.Now(),
		[]store.ResourceRef{{Kind: "Job", Name: "team-a/build"}}))

	var got []api.AppSourceMeta
	getList(t, s, "/api/v1/appsources", &got)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].SyncPolicy)
	assert.True(t, got[0].SyncPolicy.Prune)
	assert.True(t, got[0].SyncPolicy.AllowManualOverride)
	require.Len(t, got[0].ManagedResources, 1)
	assert.Equal(t, api.ResourceRef{Kind: "Job", Name: "team-a/build"}, got[0].ManagedResources[0])
}
