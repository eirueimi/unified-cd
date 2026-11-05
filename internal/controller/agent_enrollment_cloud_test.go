package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gcpTestAudience = "unified-cd"
	gcpTestProject  = "my-ci"
	gcpTestInstance = "1234567890123456789"
	gcpTestEmail    = "ci@my-ci.iam.gserviceaccount.com"
)

type cloudEnrollFixture struct {
	t       *testing.T
	s       *Server
	st      store.Store
	issuer  *testIssuer
	metrics *metrics.Metrics
	calls   int
}

func newCloudEnrollFixture(t *testing.T) *cloudEnrollFixture {
	t.Helper()
	issuer := newTestIssuer(t)
	pg := store.NewTestPostgres(t)
	s := NewServer(Config{CloudEnrollmentVerifiers: map[string]CloudEnrollmentVerifier{
		"gcp": NewGCPEnrollmentVerifier(issuer.Name, issuer.URL, nil),
	}}, pg)
	t.Cleanup(s.Close)
	m := metrics.New()
	s.SetMetrics(m)
	f := &cloudEnrollFixture{t: t, s: s, st: pg, issuer: issuer, metrics: m}
	f.putGCPPolicy("gcp-ci", []string{"kind:linux", "pool:ci"}, []string{"kind:linux"})
	return f
}

func (f *cloudEnrollFixture) putGCPPolicy(name string, allowed, required []string) {
	f.t.Helper()
	cfg, _ := json.Marshal(store.GCPPolicyConfig{Audience: gcpTestAudience})
	cons, _ := json.Marshal(store.GCPSubjectConstraints{ProjectIDs: []string{gcpTestProject}, ServiceAccounts: []string{gcpTestEmail}})
	_, err := f.st.UpsertAgentEnrollmentPolicy(context.Background(), store.AgentEnrollmentPolicy{
		Name: name, Provider: "gcp", ProviderConfig: cfg, SubjectConstraints: cons, AgentIDTemplate: "gcp:{project}:{instanceId}",
		AllowedLabels: allowed, RequiredLabels: required, AccessTokenTTL: time.Hour, Enabled: true,
	})
	require.NoError(f.t, err)
}

func gcpClaims(over map[string]any) map[string]any {
	c := map[string]any{
		"aud": gcpTestAudience, "sub": "100000000000000000001", "email": gcpTestEmail, "email_verified": true,
		"google": map[string]any{"compute_engine": map[string]any{"project_id": gcpTestProject, "instance_id": gcpTestInstance}},
	}
	for k, v := range over {
		c[k] = v
	}
	return c
}

func (f *cloudEnrollFixture) enroll(body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Each call comes from a fresh address so the per-IP enrollment rate limit
	// (covered by its own tests) does not interfere.
	f.calls++
	req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", f.calls)
	rec := httptest.NewRecorder()
	f.s.Router().ServeHTTP(rec, req)
	return rec
}

const gcpEnrollBody = `{"provider":"gcp","policy":"gcp-ci"}`

func (f *cloudEnrollFixture) requireRejected(rec *httptest.ResponseRecorder) {
	f.t.Helper()
	require.Equal(f.t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(f.t, "enrollment policy rejected\n", rec.Body.String())
}

func TestCloudEnroll_GCPValidTokenIssuesAccess(t *testing.T) {
	f := newCloudEnrollFixture(t)
	rec := f.enroll(`{"provider":"gcp","policy":"gcp-ci","labels":["pool:ci"]}`, f.issuer.Sign(gcpClaims(nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.AgentTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "gcp:my-ci:1234567890123456789", resp.AgentID)
	assert.Contains(t, resp.AccessToken, "uca_")
	assert.Empty(t, resp.RefreshToken)
	assert.Nil(t, resp.RefreshExpiresAt)
	assert.ElementsMatch(t, []string{"kind:linux", "pool:ci"}, resp.Labels)

	identity, err := f.st.GetAgentIdentity(t.Context(), resp.AgentID)
	require.NoError(t, err)
	require.NotNil(t, identity)
	assert.Equal(t, "gcp", identity.EnrollmentMethod)
}

func TestCloudEnroll_GCPRejections(t *testing.T) {
	f := newCloudEnrollFixture(t)
	cases := []struct {
		name  string
		token string
	}{
		{"wrong audience", f.issuer.Sign(gcpClaims(map[string]any{"aud": "other"}))},
		{"other issuer", f.issuer.Sign(gcpClaims(map[string]any{"iss": "https://evil.example.com"}))},
		{"expired", f.issuer.Sign(gcpClaims(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}))},
		{"unknown kid", f.issuer.SignWithKeyID("not-published", gcpClaims(nil))},
		{"project not listed", f.issuer.Sign(gcpClaims(map[string]any{"google": map[string]any{"compute_engine": map[string]any{"project_id": "other", "instance_id": gcpTestInstance}}}))},
		{"service account not listed", f.issuer.Sign(gcpClaims(map[string]any{"email": "other@my-ci.iam.gserviceaccount.com"}))},
		{"email not verified", f.issuer.Sign(gcpClaims(map[string]any{"email_verified": false}))},
		{"missing instance id", f.issuer.Sign(gcpClaims(map[string]any{"google": map[string]any{"compute_engine": map[string]any{"project_id": gcpTestProject}}}))},
		{"garbage token", "not-a-jwt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.requireRejected(f.enroll(gcpEnrollBody, tc.token))
		})
	}
	t.Run("missing bearer", func(t *testing.T) {
		f.requireRejected(f.enroll(gcpEnrollBody, ""))
	})
}

func TestCloudEnroll_GCPJWKSDownIsUnavailable(t *testing.T) {
	f := newCloudEnrollFixture(t)
	token := f.issuer.Sign(gcpClaims(nil))
	f.issuer.Server.Close()
	rec := f.enroll(gcpEnrollBody, token)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, "gcp identity unavailable\n", rec.Body.String())
}

func TestCloudEnroll_LabelAuthorization(t *testing.T) {
	f := newCloudEnrollFixture(t)
	rec := f.enroll(`{"provider":"gcp","policy":"gcp-ci","labels":["pool:admin"]}`, f.issuer.Sign(gcpClaims(nil)))
	f.requireRejected(rec)
}

func TestCloudEnroll_DisabledIdentityOnReproof(t *testing.T) {
	f := newCloudEnrollFixture(t)
	token := f.issuer.Sign(gcpClaims(nil))
	require.Equal(t, http.StatusOK, f.enroll(gcpEnrollBody, token).Code)
	require.NoError(t, f.st.SetAgentIdentityEnabled(t.Context(), "gcp:my-ci:"+gcpTestInstance, false))
	rec := f.enroll(gcpEnrollBody, token)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "agent identity disabled\n", rec.Body.String())
}

func TestCloudEnroll_SecondExchangeReflectsReducedPolicyLabels(t *testing.T) {
	f := newCloudEnrollFixture(t)
	body := `{"provider":"gcp","policy":"gcp-ci","labels":["pool:ci"]}`
	token := f.issuer.Sign(gcpClaims(nil))
	first := f.enroll(body, token)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var r1 api.AgentTokenResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &r1))
	assert.ElementsMatch(t, []string{"kind:linux", "pool:ci"}, r1.Labels)

	// The policy drops pool:ci. A request still asking for it is rejected, and
	// a request that no longer asks for it gets only the reduced set.
	f.putGCPPolicy("gcp-ci", []string{"kind:linux"}, []string{"kind:linux"})
	f.requireRejected(f.enroll(body, token))
	second := f.enroll(gcpEnrollBody, token)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	var r2 api.AgentTokenResponse
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &r2))
	assert.Equal(t, []string{"kind:linux"}, r2.Labels)
	identity, err := f.st.GetAgentIdentity(t.Context(), r2.AgentID)
	require.NoError(t, err)
	assert.Equal(t, []string{"kind:linux"}, identity.AuthorizedLabels)
}

func TestCloudEnroll_ProviderMismatchAndUnknownProvider(t *testing.T) {
	f := newCloudEnrollFixture(t)
	cfg, _ := json.Marshal(store.AWSPolicyConfig{Audience: "unified-cd"})
	cons, _ := json.Marshal(store.AWSSubjectConstraints{AccountIDs: []string{"123456789012"}, RoleNames: []string{"ci-agent"}})
	_, err := f.st.UpsertAgentEnrollmentPolicy(t.Context(), store.AgentEnrollmentPolicy{
		Name: "aws-ci", Provider: "aws", ProviderConfig: cfg, SubjectConstraints: cons, AgentIDTemplate: "aws:{account}:{instanceId}",
		AccessTokenTTL: time.Hour, Enabled: true,
	})
	require.NoError(t, err)
	token := f.issuer.Sign(gcpClaims(nil))
	f.requireRejected(f.enroll(`{"provider":"gcp","policy":"aws-ci"}`, token))
	f.requireRejected(f.enroll(`{"provider":"gcp","policy":"missing"}`, token))
	f.requireRejected(f.enroll(`{"provider":"ibm","policy":"gcp-ci"}`, token))
}

func TestCloudEnroll_MissingVerifierIsUnavailable(t *testing.T) {
	f := newCloudEnrollFixture(t)
	delete(f.s.cloudEnrollmentVerifiers, "gcp")
	rec := f.enroll(gcpEnrollBody, f.issuer.Sign(gcpClaims(nil)))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "gcp identity unavailable\n", rec.Body.String())
}

func TestCloudEnroll_DefaultsRegisterRealGCPVerifier(t *testing.T) {
	s := NewServer(Config{}, nil)
	t.Cleanup(s.Close)
	assert.Contains(t, s.cloudEnrollmentVerifiers, "gcp")
}

func TestCloudEnroll_MetricsRecordedUnderGCP(t *testing.T) {
	f := newCloudEnrollFixture(t)
	require.Equal(t, http.StatusOK, f.enroll(gcpEnrollBody, f.issuer.Sign(gcpClaims(nil))).Code)
	f.requireRejected(f.enroll(gcpEnrollBody, f.issuer.Sign(gcpClaims(map[string]any{"aud": "other"}))))
	f.requireRejected(f.enroll(`{"provider":"ibm"}`, "x"))

	rec := httptest.NewRecorder()
	f.s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	assert.Regexp(t, `provider="gcp",reason="ok",result="success"\} 1`, body)
	assert.Regexp(t, `provider="gcp",reason="policy",result="failure"\} 1`, body)
	assert.Regexp(t, `provider="other",reason="policy",result="failure"\} 1`, body)
}

func TestAgentEnrollRequest_KubernetesJSONUnchanged(t *testing.T) {
	b, err := json.Marshal(api.AgentEnrollRequest{Provider: "kubernetes", Policy: "p"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"provider":"kubernetes","policy":"p"}`, string(b))
	assert.NotContains(t, string(b), "aws")
}

// Rejections before and after verification are invisible to the caller, so the
// reason must reach the controller log (and nothing about the proof).
func TestCloudEnroll_RejectionReasonsAreLogged(t *testing.T) {
	f := newCloudEnrollFixture(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	token := f.issuer.Sign(gcpClaims(nil))
	f.requireRejected(f.enroll(`{"provider":"gcp","policy":"gcp-ci","labels":["pool:admin"]}`, token))
	out := buf.String()
	assert.Contains(t, out, "cloud enrollment rejected")
	assert.Contains(t, out, "provider=gcp")
	assert.Contains(t, out, "policy=gcp-ci")
	assert.Contains(t, out, "reason=\"labels not allowed\"")
	assert.NotContains(t, out, token)

	buf.Reset()
	f.requireRejected(f.enroll(`{"provider":"gcp","policy":"missing"}`, token))
	assert.Contains(t, buf.String(), "reason=\"policy not found\"")

	buf.Reset()
	f.requireRejected(f.enroll(`{"provider":"azure","policy":"gcp-ci"}`, token))
	assert.Contains(t, buf.String(), "reason=\"provider mismatch\"")
}
