package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/stretchr/testify/require"
)

func TestAgentRouteIdentityMatrixEntriesCarryTheirRegistrations(t *testing.T) {
	for _, route := range agentRouteIdentityMatrix {
		require.NotNil(t, route.handler, route.method+" "+route.path)
	}
}

func TestAgentRouteIdentityMatrixRejectsImpersonation(t *testing.T) {
	s, st := newTestServer(t)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))
	tokenA := issueAgentAccessForTest(t, st, "agent-a", nil, nil)

	_, err := st.UpsertJob(t.Context(), "route-matrix", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	run, err := st.CreateRun(t.Context(), "route-matrix", nil, []byte(`{"steps":[]}`), nil, nil, "")
	require.NoError(t, err)
	_, err = st.TransitionPendingToQueued(t.Context(), 1)
	require.NoError(t, err)
	claimed, err := st.ClaimNextRun(t.Context(), "agent-b", nil)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID)

	for _, route := range agentRouteIdentityMatrix {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			path := strings.NewReplacer("{agentId}", "agent-b", "{runId}", run.ID, "{name}", "proof", "{stepIndex}", "0").Replace(route.path)
			req := httptest.NewRequest(route.method, path, nil)
			req.Header.Set("Authorization", "Bearer "+tokenA)
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}
}

func TestAgentRegisterRouteRejectsBodyIdentityMismatch(t *testing.T) {
	s, st := newTestServer(t)
	tokenA := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", strings.NewReader(`{"agentId":"agent-b","hostname":"agent-b","os":"linux"}`))
	req.Header.Set("Authorization", "Bearer "+tokenA)
	rec := httptest.NewRecorder()

	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}
