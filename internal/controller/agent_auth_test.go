package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/agentauth"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

func issueAgentAccessForTest(t *testing.T, st store.Store, agentID string, labels, capabilities []string) string {
	return issueAgentAccessForTestWithExpiry(t, st, agentID, labels, capabilities, time.Now().Add(time.Hour))
}

func issueAgentAccessForTestWithExpiry(t *testing.T, st store.Store, agentID string, labels, capabilities []string, expiresAt time.Time) string {
	t.Helper()
	issued, err := agentauth.Generate(agentauth.AccessToken)
	require.NoError(t, err)
	_, err = st.IssueExternalAgentAccess(context.Background(), store.AgentCredentialIssue{
		AgentID: agentID, EnrollmentMethod: "test", ExternalSubject: "test:" + agentID,
		AuthorizedLabels: labels, AuthorizedCapabilities: capabilities,
		Access: store.NewAgentCredential{ID: issued.ID, Kind: "access", TokenHash: issued.Hash, ExpiresAt: expiresAt},
	})
	require.NoError(t, err)
	return issued.Plaintext
}

func TestAgentAuth_AcceptsValidCredential(t *testing.T) {
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "agent-a", []string{"pool:default"}, []string{"native"})

	body := []byte(`{"agentId":"agent-a","hostname":"host-a","os":"linux"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestAgentAuth_AttachesPrincipal(t *testing.T) {
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "agent-a", []string{"pool:default"}, []string{"native"})
	handler := s.agentAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := agentPrincipalFromContext(r.Context())
		require.True(t, ok)
		require.Equal(t, "agent-a", principal.AgentID)
		require.Equal(t, "bearer", principal.AuthMethod)
		require.Equal(t, []string{"pool:default"}, principal.AuthorizedLabels)
		require.Equal(t, []string{"native"}, principal.AuthorizedCapabilities)
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestAgentAuth_RejectsWrongExpiredAndDisabledCredentials(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, st store.Store, token string)
	}{
		{name: "wrong secret", setup: func(t *testing.T, st store.Store, token string) {}},
		{name: "expired credential", setup: func(t *testing.T, st store.Store, token string) {}},
		{name: "disabled identity", setup: func(t *testing.T, st store.Store, token string) {
			require.NoError(t, st.SetAgentIdentityEnabled(context.Background(), "agent-a", false))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := newTestServer(t)
			token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
			if tc.name == "expired credential" {
				token = issueAgentAccessForTestWithExpiry(t, st, "agent-expired", nil, nil, time.Now().Add(-time.Hour))
			}
			tc.setup(t, st, token)
			if tc.name == "wrong secret" {
				replacement := "A"
				if token[len(token)-1] == 'A' {
					replacement = "B"
				}
				token = token[:len(token)-1] + replacement
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-a/heartbeat", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)
			require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		})
	}
}

func TestAgentAuth_RejectsSharedTokenBearer(t *testing.T) {
	// A non-uca_ bearer (e.g. an arbitrary static token from an unmigrated
	// fleet) is never accepted: enrollment is the only agent-auth model, so
	// there is no fallback path. The credential handler is never reached.
	s, _ := newTestServer(t)
	handlerReached := false
	handler := s.agentAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerReached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer some-shared-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	require.False(t, handlerReached, "credential handler must not run for a shared-token bearer")
}

func TestAgentAuth_SharedTokenBearerToAgentRouteReturns401(t *testing.T) {
	// End-to-end through the router: a shared-token bearer sent to a real
	// agent route is rejected at the auth middleware with 401.
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader([]byte(`{"agentId":"legacy-agent","os":"linux"}`)))
	req.Header.Set("Authorization", "Bearer some-shared-token")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}

func TestAgentAuth_RejectsCredentialOnAnotherAgentPath(t *testing.T) {
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-b/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestAgentRegister_RejectsBodyIdentityMismatch(t *testing.T) {
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	body := []byte(`{"agentId":"agent-b","hostname":"host-b","os":"linux"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestAgentClaim_UsesAuthorizedPrincipalLabels(t *testing.T) {
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "agent-a", []string{"pool:default"}, []string{"native"})
	_, err := st.UpsertJob(t.Context(), "principal-labels", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)
	_, err = st.CreateRun(t.Context(), "principal-labels", nil, []byte(`{"steps":[]}`), []string{"pool:default"}, nil, "")
	require.NoError(t, err)
	_, err = st.TransitionPendingToQueued(t.Context(), 1)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-a/claim?timeout=1ms&labels=trusted:production", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.NotEmpty(t, response.RunID)
}
