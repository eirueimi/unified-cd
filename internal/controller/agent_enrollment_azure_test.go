package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	azTenant   = "11111111-1111-1111-1111-111111111111"
	azAudience = "https://management.azure.com/"
	azOID      = "22222222-2222-2222-2222-222222222222"
	azSub      = "33333333-3333-3333-3333-333333333333"
	azMIRID    = "/subscriptions/" + azSub + "/resourceGroups/ci/providers/Microsoft.Compute/virtualMachines/vm-01"
)

func azPolicy(t *testing.T, mutate func(*store.AzureSubjectConstraints)) store.AgentEnrollmentPolicy {
	t.Helper()
	cons := store.AzureSubjectConstraints{SubscriptionIDs: []string{azSub}, ResourceGroups: []string{"ci"}, ManagedIdentityObjectIDs: []string{azOID}}
	if mutate != nil {
		mutate(&cons)
	}
	cfg, _ := json.Marshal(store.AzurePolicyConfig{TenantID: azTenant, Audience: azAudience})
	c, _ := json.Marshal(cons)
	return store.AgentEnrollmentPolicy{
		Name: "azure-ci", Provider: "azure", ProviderConfig: cfg, SubjectConstraints: c, AgentIDTemplate: "azure:{subscription}:{resourceGroup}:{vmName}",
		AllowedLabels: []string{"kind:linux", "pool:ci"}, RequiredLabels: []string{"kind:linux"}, AccessTokenTTL: time.Hour, Enabled: true,
	}
}

func azClaims(authority string, over map[string]any) map[string]any {
	c := map[string]any{
		"iss": authority + "/" + azTenant + "/v2.0", "aud": azAudience, "tid": azTenant, "oid": azOID, "sub": azOID, "xms_mirid": azMIRID,
	}
	for k, v := range over {
		c[k] = v
	}
	return c
}

func azVerifier(t *testing.T) (*testIssuer, CloudEnrollmentVerifier) {
	t.Helper()
	issuer := newTestIssuer(t)
	return issuer, NewAzureEnrollmentVerifier(issuer.Server.URL, nil)
}

func verifyAz(t *testing.T, v CloudEnrollmentVerifier, token string, p store.AgentEnrollmentPolicy) (CloudEnrollmentIdentity, error) {
	t.Helper()
	return v.Verify(context.Background(), CloudEnrollmentProof{BearerToken: token}, p)
}

func TestAzureVerifier_ValidTokens(t *testing.T) {
	issuer, v := azVerifier(t)
	t.Run("v2 issuer", func(t *testing.T) {
		id, err := verifyAz(t, v, issuer.Sign(azClaims(issuer.Server.URL, nil)), azPolicy(t, nil))
		require.NoError(t, err)
		assert.Equal(t, "azure:"+azSub+":ci:vm-01", id.AgentID)
	})
	t.Run("v1 issuer", func(t *testing.T) {
		id, err := verifyAz(t, v, issuer.Sign(azClaims(issuer.Server.URL, map[string]any{"iss": "https://sts.windows.net/" + azTenant + "/"})), azPolicy(t, nil))
		require.NoError(t, err)
		assert.Equal(t, "azure:"+azSub+":ci:vm-01", id.AgentID)
	})
	t.Run("case-insensitive resource id, lower-cased agent id", func(t *testing.T) {
		mirid := "/SUBSCRIPTIONS/" + strings.ToUpper(azSub) + "/RESOURCEGROUPS/CI/PROVIDERS/MICROSOFT.COMPUTE/VIRTUALMACHINES/VM-01"
		id, err := verifyAz(t, v, issuer.Sign(azClaims(issuer.Server.URL, map[string]any{"xms_mirid": mirid})), azPolicy(t, nil))
		require.NoError(t, err)
		assert.Equal(t, "azure:"+azSub+":ci:vm-01", id.AgentID)
	})
	t.Run("empty resourceGroups allows any", func(t *testing.T) {
		mirid := "/subscriptions/" + azSub + "/resourceGroups/Other/providers/Microsoft.Compute/virtualMachines/vm-02"
		id, err := verifyAz(t, v, issuer.Sign(azClaims(issuer.Server.URL, map[string]any{"xms_mirid": mirid})), azPolicy(t, func(c *store.AzureSubjectConstraints) { c.ResourceGroups = nil }))
		require.NoError(t, err)
		assert.Equal(t, "azure:"+azSub+":other:vm-02", id.AgentID)
	})
}

func TestAzureVerifier_Rejections(t *testing.T) {
	issuer, v := azVerifier(t)
	otherTenant := "99999999-9999-9999-9999-999999999999"
	vmss := "/subscriptions/" + azSub + "/resourceGroups/ci/providers/Microsoft.Compute/virtualMachineScaleSets/x"
	uai := "/subscriptions/" + azSub + "/resourceGroups/ci/providers/Microsoft.ManagedIdentity/userAssignedIdentities/x"
	vmssVM := vmss + "/virtualMachines/0"
	cases := []struct {
		name   string
		claims map[string]any
		mutate func(*store.AzureSubjectConstraints)
	}{
		{"other tenant issuer v2", map[string]any{"iss": issuer.Server.URL + "/" + otherTenant + "/v2.0"}, nil},
		{"other tenant issuer v1", map[string]any{"iss": "https://sts.windows.net/" + otherTenant + "/"}, nil},
		{"v1 issuer without trailing slash", map[string]any{"iss": "https://sts.windows.net/" + azTenant}, nil},
		{"unrelated issuer", map[string]any{"iss": "https://evil.example.com"}, nil},
		{"wrong audience", map[string]any{"aud": "other"}, nil},
		{"expired", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, nil},
		{"tid mismatch", map[string]any{"tid": otherTenant}, nil},
		{"vmss", map[string]any{"xms_mirid": vmss}, nil},
		{"vmss instance", map[string]any{"xms_mirid": vmssVM}, nil},
		{"user assigned identity", map[string]any{"xms_mirid": uai}, nil},
		{"missing xms_mirid", map[string]any{"xms_mirid": ""}, nil},
		{"trailing segment", map[string]any{"xms_mirid": azMIRID + "/extra"}, nil},
		{"oid not listed", nil, func(c *store.AzureSubjectConstraints) { c.ManagedIdentityObjectIDs = []string{"other"} }},
		{"subscription not listed", nil, func(c *store.AzureSubjectConstraints) { c.SubscriptionIDs = []string{"other"} }},
		{"resource group not listed", nil, func(c *store.AzureSubjectConstraints) { c.ResourceGroups = []string{"prod"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifyAz(t, v, issuer.Sign(azClaims(issuer.Server.URL, tc.claims)), azPolicy(t, tc.mutate))
			require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
		})
	}
	t.Run("unknown kid", func(t *testing.T) {
		_, err := verifyAz(t, v, issuer.SignWithKeyID("not-published", azClaims(issuer.Server.URL, nil)), azPolicy(t, nil))
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
	})
	t.Run("garbage token", func(t *testing.T) {
		_, err := verifyAz(t, v, "not-a-jwt", azPolicy(t, nil))
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
	})
	t.Run("bad policy", func(t *testing.T) {
		p := azPolicy(t, nil)
		p.ProviderConfig = json.RawMessage(`{"tenantId":"` + azTenant + `"}`)
		_, err := verifyAz(t, v, issuer.Sign(azClaims(issuer.Server.URL, nil)), p)
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
	})
}

func TestAzureVerifier_JWKSDownIsUnavailable(t *testing.T) {
	issuer, v := azVerifier(t)
	token := issuer.Sign(azClaims(issuer.Server.URL, nil))
	issuer.Server.Close()
	_, err := verifyAz(t, v, token, azPolicy(t, nil))
	require.ErrorIs(t, err, ErrCloudEnrollmentUnavailable)
}

// jwksServer answers every request with status and body.
func jwksServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Pins the JWKS failure classification shared by the GCP and Azure verifiers:
// a JWKS endpoint that answers 5xx, or 200 with an undecodable body, is an
// outage of the identity service (503), not a bad token (403).
func TestOIDCVerifiers_JWKSFailureClassification(t *testing.T) {
	signer := newTestIssuer(t) // supplies a well-formed token; its keys are never served below
	gcpToken := signer.Sign(gcpClaims(nil))
	azToken := signer.Sign(azClaims("https://login.example.com", nil))
	gcpPolicy := func() store.AgentEnrollmentPolicy {
		cfg, _ := json.Marshal(store.GCPPolicyConfig{Audience: gcpTestAudience})
		cons, _ := json.Marshal(store.GCPSubjectConstraints{ProjectIDs: []string{gcpTestProject}, ServiceAccounts: []string{gcpTestEmail}})
		return store.AgentEnrollmentPolicy{Name: "gcp-ci", Provider: "gcp", ProviderConfig: cfg, SubjectConstraints: cons}
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"500", http.StatusInternalServerError, "internal error"},
		{"200 garbage body", http.StatusOK, "<html>not json</html>"},
	} {
		t.Run("gcp "+tc.name, func(t *testing.T) {
			srv := jwksServer(t, tc.status, tc.body)
			v := NewGCPEnrollmentVerifier(signer.Name, srv.URL, nil)
			_, err := v.Verify(context.Background(), CloudEnrollmentProof{BearerToken: gcpToken}, gcpPolicy())
			require.ErrorIs(t, err, ErrCloudEnrollmentUnavailable)
		})
		t.Run("azure "+tc.name, func(t *testing.T) {
			srv := jwksServer(t, tc.status, tc.body)
			v := NewAzureEnrollmentVerifier(srv.URL, nil)
			_, err := v.Verify(context.Background(), CloudEnrollmentProof{BearerToken: azToken}, azPolicy(t, nil))
			require.ErrorIs(t, err, ErrCloudEnrollmentUnavailable)
		})
	}
}

func TestVerifyOIDCToken_TruncatesErrorText(t *testing.T) {
	signer := newTestIssuer(t)
	srv := jwksServer(t, http.StatusInternalServerError, strings.Repeat("A", 100000))
	v := NewAzureEnrollmentVerifier(srv.URL, nil)
	_, err := v.Verify(context.Background(), CloudEnrollmentProof{BearerToken: signer.Sign(azClaims(srv.URL, nil))}, azPolicy(t, nil))
	require.ErrorIs(t, err, ErrCloudEnrollmentUnavailable)
	assert.Less(t, len(err.Error()), maxLoggedOIDCErrorBytes+100)
}

func TestParseAzureVMResourceID(t *testing.T) {
	sub, rg, vm, ok := parseAzureVMResourceID(azMIRID)
	assert.True(t, ok)
	assert.Equal(t, []string{azSub, "ci", "vm-01"}, []string{sub, rg, vm})
	for _, bad := range []string{"", "/", "subscriptions/a/resourceGroups/b/providers/Microsoft.Compute/virtualMachines/c",
		"/subscriptions//resourceGroups/b/providers/Microsoft.Compute/virtualMachines/c",
		"/subscriptions/a/resourceGroups/b/providers/Microsoft.Network/virtualMachines/c"} {
		_, _, _, ok := parseAzureVMResourceID(bad)
		assert.False(t, ok, bad)
	}
}

func TestCloudEnroll_AzureThroughHandler(t *testing.T) {
	issuer := newTestIssuer(t)
	pg := store.NewTestPostgres(t)
	s := NewServer(Config{CloudEnrollmentVerifiers: map[string]CloudEnrollmentVerifier{
		"azure": NewAzureEnrollmentVerifier(issuer.Server.URL, nil),
	}}, pg)
	t.Cleanup(s.Close)
	_, err := pg.UpsertAgentEnrollmentPolicy(context.Background(), azPolicy(t, nil))
	require.NoError(t, err)

	post := func(token, addr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", strings.NewReader(`{"provider":"azure","policy":"azure-ci","labels":["pool:ci"]}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}
	mirid := "/subscriptions/" + strings.ToUpper(azSub) + "/resourceGroups/CI/providers/Microsoft.Compute/virtualMachines/VM-01"
	rec := post(issuer.Sign(azClaims(issuer.Server.URL, map[string]any{"xms_mirid": mirid})), "192.0.2.1:1234")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.AgentTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "azure:"+azSub+":ci:vm-01", resp.AgentID)
	assert.NotEmpty(t, resp.AccessToken)

	rec = post(issuer.Sign(azClaims(issuer.Server.URL, map[string]any{"aud": "other"})), "192.0.2.2:1234")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "enrollment policy rejected\n", rec.Body.String())
}

func TestDefaultCloudEnrollmentVerifiers_IncludesAzure(t *testing.T) {
	assert.Contains(t, defaultCloudEnrollmentVerifiers(), "azure")
}

func TestAzureVerifier_RejectsNonGUIDTenant(t *testing.T) {
	issuer, v := azVerifier(t)
	for _, tenant := range []string{"..", "%2f", `a\b`, "tenant-1", "a b", "x@y", azTenant + "/", ""} {
		p := azPolicy(t, nil)
		p.ProviderConfig, _ = json.Marshal(store.AzurePolicyConfig{TenantID: tenant, Audience: azAudience})
		_, err := verifyAz(t, v, issuer.Sign(azClaims(issuer.Server.URL, nil)), p)
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected, tenant)
	}
}

// An unsigned-by-policy token whose audience mentions "fetching keys" must be a
// rejection (403), not an outage (503): go-oidc embeds aud in its error text.
func TestAzureVerifier_AttackerTextDoesNotLookLikeOutage(t *testing.T) {
	issuer, v := azVerifier(t)
	for _, aud := range []string{"fetching keys", "failed to verify signature: fetching keys"} {
		_, err := verifyAz(t, v, issuer.Sign(azClaims(issuer.Server.URL, map[string]any{"aud": aud})), azPolicy(t, nil))
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected, aud)
		require.NotErrorIs(t, err, ErrCloudEnrollmentUnavailable, aud)
	}
}

func TestVerifyOIDCToken_TruncationKeepsValidUTF8(t *testing.T) {
	signer := newTestIssuer(t)
	v := NewAzureEnrollmentVerifier(signer.Server.URL, nil)
	// The rejected-audience error embeds aud. Three-byte runes with 0..2 leading
	// ASCII bytes put the 512-byte cut mid-rune for at least two of the offsets
	// whatever the fixed prefix length is.
	for pad := 0; pad < 3; pad++ {
		aud := strings.Repeat("x", pad) + strings.Repeat("€", 400)
		_, err := v.Verify(context.Background(),
			CloudEnrollmentProof{BearerToken: signer.Sign(azClaims(signer.Server.URL, map[string]any{"aud": aud}))}, azPolicy(t, nil))
		require.ErrorIs(t, err, ErrCloudEnrollmentRejected)
		msg := err.Error()
		assert.True(t, utf8.ValidString(msg), "pad %d", pad)
		assert.True(t, strings.HasSuffix(msg, "(truncated)"), "pad %d", pad)
	}
}
