package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	hostagent "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCloudEnroll_E2EHostAgentRegistersAndClaims drives the real host-agent
// credential source against an in-process controller: a GCP policy created
// through the admin API, a fake metadata server that mints a JWT from the test
// JWKS issuer, then register and one claim poll with the authorized labels.
func TestCloudEnroll_E2EHostAgentRegistersAndClaims(t *testing.T) {
	issuer := newTestIssuer(t)
	pg := storetest.NewTestPostgres(t)
	_, err := pg.UpsertBootstrapPAT(context.Background(), "test-bootstrap", HashToken("secret"))
	require.NoError(t, err)
	s := NewServer(Config{CloudEnrollmentVerifiers: map[string]CloudEnrollmentVerifier{
		"gcp": NewGCPEnrollmentVerifier(issuer.Name, issuer.URL, nil),
	}}, pg)
	t.Cleanup(s.Close)
	controller := httptest.NewServer(s.Router())
	t.Cleanup(controller.Close)

	policy := `{"name":"gcp-ci","provider":"gcp","audience":"` + gcpTestAudience + `","projectIds":["` + gcpTestProject + `"],"serviceAccounts":["` + gcpTestEmail + `"],"agentIdTemplate":"gcp:{project}:{instanceId}","allowedLabels":["kind:linux","pool:ci"],"requiredLabels":["kind:linux"],"accessTokenTTL":"1h","enabled":true}`
	req, err := http.NewRequest(http.MethodPost, controller.URL+"/api/v1/agent-enrollment-policies", strings.NewReader(policy))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity" ||
			r.Header.Get("Metadata-Flavor") != "Google" || r.URL.Query().Get("audience") != gcpTestAudience {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(issuer.Sign(gcpClaims(nil))))
	}))
	t.Cleanup(metadata.Close)

	source, err := hostagent.NewCloudCredentialSource(hostagent.CloudCredentialSourceConfig{
		Server: controller.URL, Provider: "gcp", Policy: "gcp-ci", Audience: gcpTestAudience,
		Labels: []string{"pool:ci"}, Capabilities: []string{"native"}, MetadataBaseURL: metadata.URL,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token, err := source.Token(ctx)
	require.NoError(t, err)
	assert.Contains(t, token, "uca_")
	agentID := source.AgentID()
	labels := source.Labels()
	assert.Equal(t, "gcp:my-ci:1234567890123456789", agentID)
	assert.ElementsMatch(t, []string{"kind:linux", "pool:ci"}, labels)

	cli := hostagent.NewClientWithTokenSource(controller.URL, source, nil)
	require.NoError(t, cli.Register(ctx, api.AgentRegisterRequest{
		AgentID: agentID, Hostname: "vm-1", OS: "linux", Labels: labels, Capabilities: []string{"native"},
	}))
	claim, err := cli.Claim(ctx, agentID, "1s", labels, api.ClaimRequest{})
	require.NoError(t, err)
	assert.Empty(t, claim.RunID, "no run is queued, so the poll returns empty")
}
