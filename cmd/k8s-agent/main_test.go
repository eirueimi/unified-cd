package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/k8sagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBootstrapAgentClientUsesKubernetesEnrollmentIdentity(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("projected-jwt"), 0o600))
	var registered api.AgentRegisterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/enroll":
			assert.Equal(t, "Bearer projected-jwt", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:pod-uid", AccessToken: "access-token", AccessExpiresAt: time.Now().Add(time.Hour), Labels: []string{"kind:kubernetes", "pool:ci"}})
		case "/api/v1/agents/register":
			assert.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
			require.NoError(t, json.NewDecoder(r.Body).Decode(&registered))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := k8sagent.Config{Server: srv.URL, EnrollmentPolicy: "cluster-agents", ServiceAccountTokenFile: tokenFile, Labels: []string{"kind:kubernetes"}}
	client, err := bootstrapAgentClient(t.Context(), &cfg, srv.Client())
	require.NoError(t, err)
	require.NoError(t, client.Register(t.Context(), api.AgentRegisterRequest{AgentID: cfg.AgentID, Labels: cfg.Labels}))
	assert.Equal(t, "k8s:prod:ci:pod-uid", cfg.AgentID)
	assert.Equal(t, []string{"kind:kubernetes", "pool:ci"}, cfg.Labels)
	assert.Equal(t, cfg.AgentID, registered.AgentID)
	assert.Equal(t, cfg.Labels, registered.Labels)
}
