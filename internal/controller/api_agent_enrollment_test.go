package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/agentauth"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func enrollmentRequest(t *testing.T, s *Server, token string, body api.CreateAgentEnrollmentRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-enrollments", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestAgentEnrollmentAdminCreateAndList(t *testing.T) {
	s, _ := newTestServer(t)

	before := time.Now().Add(9 * time.Minute)
	rec := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{
		AgentID: "vm-agent-01", Labels: []string{"kind:linux"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, "vm-agent-01", created.AgentID)
	require.Contains(t, created.Token, "uce_")
	require.True(t, created.ExpiresAt.After(before))
	require.True(t, created.ExpiresAt.Before(time.Now().Add(11*time.Minute)))

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/agent-enrollments", nil)
	listReq.Header.Set("Authorization", "Bearer secret")
	listRec := httptest.NewRecorder()
	s.Router().ServeHTTP(listRec, listReq)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	assert.NotContains(t, listRec.Body.String(), created.Token)
	assert.NotContains(t, listRec.Body.String(), "hash")
	var listed []api.AgentEnrollmentMeta
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listed))
	require.Len(t, listed, 1)
	assert.Equal(t, created.ID, listed[0].ID)
}

func TestAgentEnrollmentAdminRejectsInvalidDuration(t *testing.T) {
	s, _ := newTestServer(t)
	for _, body := range []api.CreateAgentEnrollmentRequest{
		{AgentID: "vm-agent-01", ExpiresIn: "not-a-duration"},
		{AgentID: "vm-agent-01", ExpiresIn: "0s"},
		{AgentID: "vm-agent-01", ExpiresIn: "-1m"},
	} {
		rec := enrollmentRequest(t, s, "secret", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestAgentEnrollmentAdminAutogeneratesAgentIDWhenOmitted(t *testing.T) {
	s, _ := newTestServer(t)
	rec := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{}) // no agentId
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var response api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	assert.Regexp(t, `^agent-[0-9a-f]{8}$`, response.AgentID,
		"an omitted agentId must be auto-generated as agent-<8 hex>")
}

func TestAgentEnrollmentAdminRejectsNonPortableAgentID(t *testing.T) {
	s, _ := newTestServer(t)
	for _, id := range []string{"Agent-A", "agent-é", "agent-a.", "con"} {
		t.Run(id, func(t *testing.T) {
			rec := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{AgentID: id})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, "agent ID must use lowercase ASCII letters, digits, '.', '_', or '-', start and end with a letter or digit, and not use a reserved Windows name\n", rec.Body.String())
		})
	}
}

func TestAgentEnrollmentAdminEnforcesMaximumLifetime(t *testing.T) {
	s, _ := newTestServer(t)

	accepted := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{
		AgentID: "vm-agent-max", ExpiresIn: "24h",
	})
	require.Equal(t, http.StatusCreated, accepted.Code, accepted.Body.String())

	rejected := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{
		AgentID: "vm-agent-over-max", ExpiresIn: "24h1ns",
	})
	require.Equal(t, http.StatusBadRequest, rejected.Code, rejected.Body.String())
}

func TestAgentEnrollmentAdminAuthorizationAndRevoke(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, pg, "viewer-token", "viewer")
	require.Equal(t, http.StatusForbidden, enrollmentRequest(t, s, "viewer-token", api.CreateAgentEnrollmentRequest{AgentID: "vm-agent-01"}).Code)

	created := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{AgentID: "vm-agent-01"})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var response api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &response))
	require.Equal(t, http.StatusForbidden, doReq(t, s, http.MethodDelete, "/api/v1/agent-enrollments/"+response.ID, "viewer-token", nil))
	require.Equal(t, http.StatusNoContent, doReq(t, s, http.MethodDelete, "/api/v1/agent-enrollments/"+response.ID, "secret", nil))
}

func TestAgentEnrollmentPolicyAdminCRUD(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, pg, "viewer-token", "viewer")
	body := []byte(`{"name":"prod-k8s","provider":"kubernetes","cluster":"prod","namespaces":["unified-cd"],"serviceAccounts":["unified-cd-k8s-agent"],"agentIdTemplate":"k8s:{cluster}:{namespace}:{podUID}","allowedLabels":["kind:kubernetes"],"requiredLabels":["kind:kubernetes"],"accessTokenTTL":"15m","enabled":true}`)
	require.Equal(t, http.StatusForbidden, doReq(t, s, http.MethodPost, "/api/v1/agent-enrollment-policies", "viewer-token", body))
	require.Equal(t, http.StatusCreated, doReq(t, s, http.MethodPost, "/api/v1/agent-enrollment-policies", "secret", body))
	require.Equal(t, http.StatusOK, doReq(t, s, http.MethodGet, "/api/v1/agent-enrollment-policies/prod-k8s", "viewer-token", nil))
	require.Equal(t, http.StatusOK, doReq(t, s, http.MethodGet, "/api/v1/agent-enrollment-policies", "viewer-token", nil))
	require.Equal(t, http.StatusNoContent, doReq(t, s, http.MethodDelete, "/api/v1/agent-enrollment-policies/prod-k8s", "secret", nil))
}

func TestAgentIdentityAdminLifecycleAndViewerRead(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, pg, "viewer-token", "viewer")
	access, err := agentauth.Generate(agentauth.AccessToken)
	require.NoError(t, err)
	// Seed the persistent identity through a consumed one-time enrollment.
	enrollment, err := agentauth.Generate(agentauth.EnrollmentToken)
	require.NoError(t, err)
	_, err = pg.CreateAgentEnrollmentToken(t.Context(), store.AgentEnrollmentToken{ID: enrollment.ID, AgentID: "vm-agent-01", CreatedBy: "admin", ExpiresAt: time.Now().Add(time.Hour)}, enrollment.Hash)
	require.NoError(t, err)
	_, err = pg.ConsumeAgentEnrollment(t.Context(), enrollment.ID, enrollment.Hash, store.AgentCredentialIssue{
		AgentID: "vm-agent-01", EnrollmentMethod: "enrollment", Access: store.NewAgentCredential{ID: access.ID, Kind: "access", TokenHash: access.Hash, ExpiresAt: time.Now().Add(time.Hour)},
	})
	require.NoError(t, err)

	get := httptest.NewRequest(http.MethodGet, "/api/v1/agent-identities/vm-agent-01", nil)
	get.Header.Set("Authorization", "Bearer viewer-token")
	getRec := httptest.NewRecorder()
	s.Router().ServeHTTP(getRec, get)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	assert.NotContains(t, getRec.Body.String(), access.Plaintext)
	assert.NotContains(t, getRec.Body.String(), "hash")

	base := "/api/v1/agent-identities/vm-agent-01"
	require.Equal(t, http.StatusForbidden, doReq(t, s, http.MethodPost, base+"/disable", "viewer-token", nil))
	require.Equal(t, http.StatusNoContent, doReq(t, s, http.MethodPost, base+"/disable", "secret", nil))
	require.Equal(t, http.StatusNoContent, doReq(t, s, http.MethodPost, base+"/enable", "secret", nil))
	require.Equal(t, http.StatusNoContent, doReq(t, s, http.MethodPost, base+"/credentials/revoke", "secret", nil))
}

func enrollAgent(t *testing.T, s *Server, token string) api.AgentTokenResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response api.AgentTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	return response
}

func refreshAgent(t *testing.T, s *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/token/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestAgentEnrollIssuesVMAccessAndRefreshTokens(t *testing.T) {
	s, _ := newTestServer(t)
	created := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{
		AgentID: "vm-agent-exchange", Labels: []string{"pool:vm"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var enrollment api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &enrollment))

	response := enrollAgent(t, s, enrollment.Token)
	require.Equal(t, "vm-agent-exchange", response.AgentID)
	require.Contains(t, response.AccessToken, "uca_")
	require.Contains(t, response.RefreshToken, "ucr_")
	require.WithinDuration(t, time.Now().Add(time.Hour), response.AccessExpiresAt, 5*time.Second)
	require.NotNil(t, response.RefreshExpiresAt)
	require.WithinDuration(t, time.Now().Add(30*24*time.Hour), *response.RefreshExpiresAt, 5*time.Second)
	require.Equal(t, []string{"pool:vm"}, response.Labels)
	require.Empty(t, response.Capabilities)

	second := refreshAgent(t, s, response.AccessToken)
	require.Equal(t, http.StatusUnauthorized, second.Code, second.Body.String())

	rotated := refreshAgent(t, s, response.RefreshToken)
	require.Equal(t, http.StatusOK, rotated.Code, rotated.Body.String())
	var next api.AgentTokenResponse
	require.NoError(t, json.Unmarshal(rotated.Body.Bytes(), &next))
	require.Contains(t, next.AccessToken, "uca_")
	require.Contains(t, next.RefreshToken, "ucr_")
	require.NotEqual(t, response.RefreshToken, next.RefreshToken)
}

func TestAgentEnrollRejectsRepeatedAndInvalidEnrollment(t *testing.T) {
	s, _ := newTestServer(t)
	created := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{AgentID: "vm-agent-once"})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var enrollment api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &enrollment))
	_ = enrollAgent(t, s, enrollment.Token)

	for _, token := range []string{enrollment.Token, "uce_not-a-token", "uca_not-an-enrollment"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		require.Equal(t, "unauthorized\n", rec.Body.String())
	}
}

func TestAgentEnrollRateLimitCannotBeBypassedWithDistinctPolicies(t *testing.T) {
	s := NewServer(Config{}, nil)
	for i := 0; i < enrollmentLimiterCapacity+10; i++ {
		body, err := json.Marshal(api.AgentEnrollRequest{Policy: fmt.Sprintf("untrusted-%d", i)})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", bytes.NewReader(body))
		req.RemoteAddr = "192.0.2.44:1234"
		req.Header.Set("Authorization", "Bearer uce_invalid")
		rec := httptest.NewRecorder()
		s.handleAgentEnroll(rec, req)

		if i < 5 {
			require.Equal(t, http.StatusUnauthorized, rec.Code, "request %d: %s", i, rec.Body.String())
			continue
		}
		require.Equal(t, http.StatusTooManyRequests, rec.Code, "request %d: %s", i, rec.Body.String())
		require.Equal(t, "6", rec.Header().Get("Retry-After"))
		require.Equal(t, "enrollment rate limit exceeded\n", rec.Body.String())
	}
	require.Equal(t, 1, s.enrollmentLimiter.len())
}

func TestAgentRefreshIsRateLimitedBeforeCredentialParsing(t *testing.T) {
	s := NewServer(Config{}, nil)
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/token/refresh", nil)
		req.RemoteAddr = "192.0.2.45:1234"
		req.Header.Set("Authorization", "Bearer ucr_invalid")
		rec := httptest.NewRecorder()
		s.handleAgentRefresh(rec, req)

		if i < 5 {
			require.Equal(t, http.StatusUnauthorized, rec.Code, "request %d: %s", i, rec.Body.String())
			continue
		}
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		require.Equal(t, "6", rec.Header().Get("Retry-After"))
		require.Equal(t, "enrollment rate limit exceeded\n", rec.Body.String())
	}
}

func TestAgentRefreshUnavailableResponseIsGeneric(t *testing.T) {
	s := NewServer(Config{}, nil)
	refresh, err := agentauth.Generate(agentauth.RefreshToken)
	require.NoError(t, err)

	rec := refreshAgent(t, s, refresh.Plaintext)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "enrollment unavailable\n", rec.Body.String())
}

func TestAgentEnrollExpiredCredentialResponseIsGeneric(t *testing.T) {
	s, st := newTestServer(t)
	enrollment, err := agentauth.Generate(agentauth.EnrollmentToken)
	require.NoError(t, err)
	_, err = st.CreateAgentEnrollmentToken(t.Context(), store.AgentEnrollmentToken{
		ID: enrollment.ID, AgentID: "vm-agent-expired", CreatedBy: "admin", ExpiresAt: time.Now().Add(-time.Minute),
	}, enrollment.Hash)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", nil)
	req.Header.Set("Authorization", "Bearer "+enrollment.Plaintext)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "unauthorized\n", rec.Body.String())
}

func TestAgentEnrollDisabledIdentityResponseIsExplicit(t *testing.T) {
	s, st := newTestServer(t)
	first := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{AgentID: "vm-agent-disabled"})
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	var firstEnrollment api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &firstEnrollment))
	_ = enrollAgent(t, s, firstEnrollment.Token)
	require.NoError(t, st.SetAgentIdentityEnabled(t.Context(), "vm-agent-disabled", false))

	second := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{AgentID: "vm-agent-disabled"})
	require.Equal(t, http.StatusCreated, second.Code, second.Body.String())
	var secondEnrollment api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &secondEnrollment))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", nil)
	req.Header.Set("Authorization", "Bearer "+secondEnrollment.Token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "agent identity disabled\n", rec.Body.String())
}

// enrollOnlyToken mints an ownerless agent.enroll PAT with the given label
// bound (nil = unbounded) and returns its plaintext token.
func enrollOnlyToken(t *testing.T, s *Server, bound []string) string {
	t.Helper()
	code, body, resp := postToken(t, s, "secret",
		api.CreatePATRequest{Name: "prov", Permissions: []string{"agent.enroll"}, EnrollLabels: bound})
	require.Equal(t, http.StatusOK, code, body)
	return resp.Token
}

func TestAgentEnrollmentEnrollOnlyCallerBoundToLabels(t *testing.T) {
	s, _ := breakGlassServer(t)
	bounded := enrollOnlyToken(t, s, []string{"kind:linux", "pool:build"})

	rec := enrollmentRequest(t, s, bounded, api.CreateAgentEnrollmentRequest{Labels: []string{"kind:linux"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Regexp(t, `^agent-[0-9a-f]{8}$`, created.AgentID)

	rec = enrollmentRequest(t, s, bounded, api.CreateAgentEnrollmentRequest{Labels: []string{"kind:linux", "pool:gpu"}})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "label \"pool:gpu\" is not in this token's enrollment labels\n", rec.Body.String())

	// Labels are compared exactly, without trimming.
	rec = enrollmentRequest(t, s, bounded, api.CreateAgentEnrollmentRequest{Labels: []string{" kind:linux"}})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// No labels at all is within any bound.
	rec = enrollmentRequest(t, s, bounded, api.CreateAgentEnrollmentRequest{})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestAgentEnrollmentEnrollOnlyCallerWithoutBound(t *testing.T) {
	s, _ := breakGlassServer(t)
	unbounded := enrollOnlyToken(t, s, nil)

	rec := enrollmentRequest(t, s, unbounded, api.CreateAgentEnrollmentRequest{Labels: []string{"kind:linux"}})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "label \"kind:linux\" is not in this token's enrollment labels\n", rec.Body.String())

	rec = enrollmentRequest(t, s, unbounded, api.CreateAgentEnrollmentRequest{})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestAgentEnrollmentEnrollOnlyCallerCannotSetAgentID(t *testing.T) {
	s, _ := breakGlassServer(t)
	tok := enrollOnlyToken(t, s, []string{"kind:linux"})
	for _, id := range []string{"vm-agent-01", "aws:123456789012:i-0abc12345678", "Bad ID"} {
		rec := enrollmentRequest(t, s, tok, api.CreateAgentEnrollmentRequest{AgentID: id})
		require.Equal(t, http.StatusForbidden, rec.Code, id+": "+rec.Body.String())
		assert.Equal(t, "agentId cannot be set with agent.enroll; it is generated\n", rec.Body.String())
	}
}

func TestAgentEnrollmentManageCallerUnchanged(t *testing.T) {
	s, _ := newTestServer(t)
	rec := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{AgentID: "vm-agent-01", Labels: []string{"anything:goes"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestAgentEnrollmentRejectsCloudShapedAgentID(t *testing.T) {
	s, _ := newTestServer(t)
	rec := enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{AgentID: "aws:123456789012:i-0abc12345678"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestAgentEnrollmentAuditResourceCarriesAgentIDAndLabels(t *testing.T) {
	s, pg := breakGlassServer(t)
	tok := enrollOnlyToken(t, s, []string{"kind:linux", "pool:build"})
	rec := enrollmentRequest(t, s, tok, api.CreateAgentEnrollmentRequest{Labels: []string{"kind:linux", "pool:build"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	logs, err := pg.ListAuditLogs(t.Context(), 20, 0)
	require.NoError(t, err)
	var found bool
	for _, l := range logs {
		if l.Action == "agent.enrollment.create" {
			found = true
			assert.Equal(t, created.ID+" agent="+created.AgentID+" labels=kind:linux,pool:build", l.Resource)
		}
	}
	require.True(t, found)

	// Without labels the resource is just the agent ID.
	rec = enrollmentRequest(t, s, "secret", api.CreateAgentEnrollmentRequest{AgentID: "vm-agent-02"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	logs, err = pg.ListAuditLogs(t.Context(), 20, 0)
	require.NoError(t, err)
	var second api.CreateAgentEnrollmentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &second))
	assert.Equal(t, second.ID+" agent=vm-agent-02", logs[0].Resource)
}

func TestAgentEnrollmentPolicyCloudProviders(t *testing.T) {
	s, _ := newTestServer(t)
	post := func(body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-enrollment-policies", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	get := func(name string) api.AgentEnrollmentPolicyRequest {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agent-enrollment-policies/"+name, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got api.AgentEnrollmentPolicyRequest
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got
	}

	code, body := post(`{"name":"gcp-ci","provider":"gcp","audience":"unified-cd","projectIds":["my-ci"],"serviceAccounts":["ci@my-ci.iam.gserviceaccount.com"],"agentIdTemplate":"gcp:{project}:{instanceId}","allowedLabels":["kind:linux"],"accessTokenTTL":"1h","enabled":true}`)
	require.Equal(t, http.StatusCreated, code, body)
	assert.NotContains(t, body, "cluster")
	assert.NotContains(t, body, "namespaces")
	gcp := get("gcp-ci")
	assert.Equal(t, "gcp", gcp.Provider)
	assert.Equal(t, "unified-cd", gcp.Audience)
	assert.Equal(t, []string{"my-ci"}, gcp.ProjectIDs)
	assert.Equal(t, []string{"ci@my-ci.iam.gserviceaccount.com"}, gcp.ServiceAccounts)

	code, body = post(`{"name":"aws-ci","provider":"aws","audience":"unified-cd","stsRegions":["eu-west-1"],"accountIds":["123456789012"],"roleNames":["ci-agent"],"agentIdTemplate":"aws:{account}:{instanceId}","accessTokenTTL":"1h","enabled":true}`)
	require.Equal(t, http.StatusCreated, code, body)
	aws := get("aws-ci")
	assert.Equal(t, []string{"eu-west-1"}, aws.STSRegions)
	assert.Equal(t, []string{"123456789012"}, aws.AccountIDs)
	assert.Equal(t, []string{"ci-agent"}, aws.RoleNames)

	code, body = post(`{"name":"azure-ci","provider":"azure","tenantId":"11111111-1111-1111-1111-111111111111","audience":"unified-cd","subscriptionIds":["sub-1"],"resourceGroups":["ci"],"managedIdentityObjectIds":["oid-1"],"agentIdTemplate":"azure:{subscription}:{resourceGroup}:{vmName}","accessTokenTTL":"1h","enabled":true}`)
	require.Equal(t, http.StatusCreated, code, body)
	azure := get("azure-ci")
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", azure.TenantID)
	assert.Equal(t, []string{"sub-1"}, azure.SubscriptionIDs)
	assert.Equal(t, []string{"ci"}, azure.ResourceGroups)
	assert.Equal(t, []string{"oid-1"}, azure.ManagedIdentityObjectIDs)

	code, body = post(`{"name":"bad","provider":"gcp","audience":"unified-cd","agentIdTemplate":"gcp:{project}:{instanceId}","accessTokenTTL":"1h","enabled":true}`)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, "invalid enrollment policy: gcp enrollment policy requires audience, projectIds and serviceAccounts")

	code, body = post(`{"name":"bad","provider":"x","agentIdTemplate":"t","accessTokenTTL":"1h","enabled":true}`)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, `invalid enrollment policy: unsupported enrollment provider "x"`)
}
