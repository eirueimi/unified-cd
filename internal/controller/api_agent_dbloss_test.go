package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/agentauth"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func agentRequest(s *Server, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func agentListed(t *testing.T, s *Server, id string) bool {
	t.Helper()
	rec := agentRequest(s, http.MethodGet, "/api/v1/agents", "secret")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var agents []api.AgentInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &agents))
	for _, a := range agents {
		if a.ID == id {
			return true
		}
	}
	return false
}

// Full database loss: the controller has neither agent inventory nor credential
// records. A host agent still holding credentials issued by the lost database
// must fail closed (no automatic acceptance of unknown credentials, no
// inventory row) until it is re-enrolled with a new one-time enrollment.
func TestAgentDatabaseLoss_HostAgentMustReenroll(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()

	// Credentials that were valid on the lost database: well-formed, never stored here.
	staleAccess, err := agentauth.Generate(agentauth.AccessToken)
	require.NoError(t, err)
	staleRefresh, err := agentauth.Generate(agentauth.RefreshToken)
	require.NoError(t, err)

	rec := agentRequest(s, http.MethodPost, "/api/v1/agents/vm-lost/claim?timeout=100ms", staleAccess.Plaintext)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "unauthorized\n", rec.Body.String())

	rec = agentRequest(s, http.MethodPost, "/api/v1/agents/vm-lost/heartbeat", staleAccess.Plaintext)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = refreshAgent(t, s, staleRefresh.Plaintext)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	got, err := pg.GetAgent(ctx, "vm-lost")
	require.NoError(t, err)
	assert.Nil(t, got, "a rejected credential must not create an inventory row")
	assert.False(t, agentListed(t, s, "vm-lost"))

	// Recovery: new one-time enrollment, exchanged through the real API.
	created := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{
		AgentID: "vm-lost", Labels: []string{"kind:linux"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var enrollment api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &enrollment))
	tokens := enrollAgent(t, s, enrollment.Token)
	require.Equal(t, "vm-lost", tokens.AgentID)

	rec = agentRequest(s, http.MethodPost, "/api/v1/agents/vm-lost/claim?timeout=100ms", tokens.AccessToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, agentListed(t, s, "vm-lost"), "re-enrolled agent must appear after its first claim")

	// The stale credential is still rejected.
	rec = agentRequest(s, http.MethodPost, "/api/v1/agents/vm-lost/claim?timeout=100ms", staleAccess.Plaintext)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

type fixedKubernetesVerifier struct{ identity KubernetesEnrollmentIdentity }

func (v fixedKubernetesVerifier) Verify(context.Context, string, store.AgentEnrollmentPolicy) (KubernetesEnrollmentIdentity, error) {
	return v.identity, nil
}

// A Kubernetes agent needs no new one-time enrollment after database loss: once
// the enrollment policy exists again (the controller re-creates policies from
// its config at startup, see bootstrapKubernetesEnrollmentPolicies), the agent
// proves its ServiceAccount token and receives fresh credentials.
func TestAgentDatabaseLoss_KubernetesAgentRecoversOncePolicyExists(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, err := pg.UpsertBootstrapPAT(context.Background(), "test-bootstrap", HashToken("secret"))
	require.NoError(t, err)
	s := NewServer(Config{KubernetesEnrollmentVerifiers: map[string]KubernetesEnrollmentVerifier{
		"in-cluster": fixedKubernetesVerifier{identity: KubernetesEnrollmentIdentity{
			Cluster: "in-cluster", Namespace: "unified-cd", ServiceAccount: "unified-cd-k8s-agent", PodName: "agent-0", PodUID: "pod-uid",
		}},
	}}, pg)
	t.Cleanup(s.Close)

	enroll := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", bytes.NewBufferString(`{"provider":"kubernetes","policy":"unified-cd-k8s-agents","labels":["kind:kubernetes"]}`))
		req.Header.Set("Authorization", "Bearer projected-token")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}

	// Fresh database, policy not yet re-created: enrollment is refused.
	assert.NotEqual(t, http.StatusOK, enroll().Code)

	policy, err := config.ControllerKubernetesEnrollmentPolicyConfig{
		Name: "unified-cd-k8s-agents", Cluster: "in-cluster", Namespaces: []string{"unified-cd"}, ServiceAccounts: []string{"unified-cd-k8s-agent"},
		AllowedLabels: []string{"kind:kubernetes"}, RequiredLabels: []string{"kind:kubernetes"}, AccessTokenTTL: "1h", Enabled: true,
	}.StorePolicy()
	require.NoError(t, err)
	_, err = pg.UpsertAgentEnrollmentPolicy(context.Background(), policy)
	require.NoError(t, err)

	rec := enroll()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var tokens api.AgentTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tokens))
	require.NotEmpty(t, tokens.AgentID)

	rec = agentRequest(s, http.MethodPost, "/api/v1/agents/"+tokens.AgentID+"/claim?timeout=100ms", tokens.AccessToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, agentListed(t, s, tokens.AgentID))
}
