package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func kubernetesTokenSource(t *testing.T, serverURL, tokenFile string, now func() time.Time) *KubernetesCredentialSource {
	t.Helper()
	return NewKubernetesCredentialSource(KubernetesCredentialSourceConfig{
		Server: serverURL, Policy: "cluster-agents", ServiceAccountTokenFile: tokenFile,
		Labels: []string{"kind:kubernetes"}, Capabilities: []string{"pod", "container"},
		HTTPClient: &http.Client{Timeout: time.Second}, Now: now,
	})
}

func TestKubernetesCredentialSourceExchangesProjectedTokenAndUsesCanonicalIdentity(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tokenFile := filepath.Join(t.TempDir(), "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("k8s-jwt"), 0o600))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/agents/enroll", r.URL.Path)
		assert.Equal(t, "Bearer k8s-jwt", r.Header.Get("Authorization"))
		var request api.AgentEnrollRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, api.AgentEnrollRequest{Provider: "kubernetes", Policy: "cluster-agents", Labels: []string{"kind:kubernetes"}, Capabilities: []string{"pod", "container"}}, request)
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:uid-1", AccessToken: "access-1", AccessExpiresAt: now.Add(time.Hour), Labels: []string{"kind:kubernetes", "pool:ci"}})
	}))
	defer srv.Close()

	source := kubernetesTokenSource(t, srv.URL, tokenFile, func() time.Time { return now })
	token, err := source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "access-1", token)
	assert.Equal(t, "k8s:prod:ci:uid-1", source.AgentID())
	assert.Equal(t, []string{"kind:kubernetes", "pool:ci"}, source.Labels())
}

func TestKubernetesCredentialSourceCachesValidAccessToken(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tokenFile := filepath.Join(t.TempDir(), "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("k8s-jwt"), 0o600))
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:uid-1", AccessToken: "access-1", AccessExpiresAt: now.Add(time.Hour)})
	}))
	defer srv.Close()

	source := kubernetesTokenSource(t, srv.URL, tokenFile, func() time.Time { return now })
	first, err := source.Token(t.Context())
	require.NoError(t, err)
	second, err := source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "access-1", first)
	assert.Equal(t, "access-1", second)
	assert.Equal(t, 1, calls)
}

func TestKubernetesCredentialSourceRefreshesBeforeAccessExpiry(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tokenFile := filepath.Join(t.TempDir(), "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("k8s-jwt"), 0o600))
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:uid-1", AccessToken: "access-" + string(rune('0'+calls)), AccessExpiresAt: now.Add(10 * time.Minute)})
	}))
	defer srv.Close()

	source := kubernetesTokenSource(t, srv.URL, tokenFile, func() time.Time { return now })
	first, err := source.Token(t.Context())
	require.NoError(t, err)
	second, err := source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "access-1", first)
	assert.Equal(t, "access-2", second)
	assert.Equal(t, 2, calls)
}

func TestKubernetesCredentialSourceUsesInjectedJitterForRefreshWindow(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tokenFile := filepath.Join(t.TempDir(), "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("k8s-jwt"), 0o600))
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:uid-1", AccessToken: "access-" + string(rune('0'+calls)), AccessExpiresAt: now.Add(17 * time.Minute)})
	}))
	defer srv.Close()

	source := NewKubernetesCredentialSource(KubernetesCredentialSourceConfig{
		Server: srv.URL, Policy: "cluster-agents", ServiceAccountTokenFile: tokenFile,
		HTTPClient: srv.Client(), Now: func() time.Time { return now }, Jitter: func() time.Duration { return 2 * time.Minute },
	})
	_, err := source.Token(t.Context())
	require.NoError(t, err)
	_, err = source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
}

func TestKubernetesCredentialSourceRetriesUnauthorizedEnrollmentWithRotatedProjectedToken(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tokenFile := filepath.Join(t.TempDir(), "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("stale-jwt"), 0o600))
	var authorizations []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		if len(authorizations) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:uid-1", AccessToken: "access-1", AccessExpiresAt: now.Add(time.Hour)})
	}))
	defer srv.Close()

	source := kubernetesTokenSource(t, srv.URL, tokenFile, func() time.Time { return now })
	source.sleep = func(context.Context, time.Duration) error {
		return os.WriteFile(tokenFile, []byte("rotated-jwt"), 0o600)
	}
	_, err := source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"Bearer stale-jwt", "Bearer rotated-jwt"}, authorizations)
}

func TestKubernetesCredentialSourceReenrollsAndRetriesAfterUnauthorizedAgentRequest(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tokenFile := filepath.Join(t.TempDir(), "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("k8s-jwt"), 0o600))
	enrollments := 0
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/enroll":
			enrollments++
			_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:uid-1", AccessToken: "access-" + string(rune('0'+enrollments)), AccessExpiresAt: now.Add(time.Hour)})
		case "/api/v1/agents/register":
			requests++
			if r.Header.Get("Authorization") == "Bearer access-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			require.Equal(t, "Bearer access-2", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	source := kubernetesTokenSource(t, srv.URL, tokenFile, func() time.Time { return now })
	client := agentlib.NewClientWithTokenSource(srv.URL, source, srv.Client())
	require.NoError(t, client.Register(t.Context(), api.AgentRegisterRequest{AgentID: "k8s:prod:ci:uid-1"}))
	assert.Equal(t, 2, enrollments)
	assert.Equal(t, 2, requests)
}

func TestKubernetesCredentialSourceRereadsReplacedProjectedToken(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("first-jwt"), 0o600))
	var got []string
	current := now
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:uid-1", AccessToken: "access-" + string(r.Header.Get("Authorization")[7]), AccessExpiresAt: current.Add(time.Minute)})
	}))
	defer srv.Close()

	source := kubernetesTokenSource(t, srv.URL, tokenFile, func() time.Time { return current })
	_, err := source.Token(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "replacement"), []byte("second-jwt"), 0o600))
	require.NoError(t, os.Rename(filepath.Join(dir, "replacement"), tokenFile))
	current = current.Add(2 * time.Minute)
	_, err = source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"Bearer first-jwt", "Bearer second-jwt"}, got)
}

func TestKubernetesCredentialSourceExchangesOnceForConcurrentCallers(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tokenFile := filepath.Join(t.TempDir(), "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("k8s-jwt"), 0o600))
	calls := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:uid-1", AccessToken: "access-1", AccessExpiresAt: now.Add(time.Hour)})
	}))
	defer srv.Close()

	source := kubernetesTokenSource(t, srv.URL, tokenFile, func() time.Time { return now })
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := source.Token(context.Background())
			require.NoError(t, err)
			assert.Equal(t, "access-1", token)
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, calls)
}

func TestKubernetesCredentialSourceRetriesUnavailableExchange(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tokenFile := filepath.Join(t.TempDir(), "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("k8s-jwt"), 0o600))
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:uid-1", AccessToken: "access-1", AccessExpiresAt: now.Add(time.Hour)})
	}))
	defer srv.Close()

	source := kubernetesTokenSource(t, srv.URL, tokenFile, func() time.Time { return now })
	source.sleep = func(context.Context, time.Duration) error { return nil }
	token, err := source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "access-1", token)
	assert.Equal(t, 3, calls)
}

func TestKubernetesCredentialSourceDoesNotPersistRefreshCredentials(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "projected-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("k8s-jwt"), 0o600))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshExpiry := now.Add(24 * time.Hour)
		_ = json.NewEncoder(w).Encode(api.AgentTokenResponse{AgentID: "k8s:prod:ci:uid-1", AccessToken: "access-1", AccessExpiresAt: now.Add(time.Hour), RefreshToken: "must-not-persist", RefreshExpiresAt: &refreshExpiry})
	}))
	defer srv.Close()

	source := kubernetesTokenSource(t, srv.URL, tokenFile, func() time.Time { return now })
	_, err := source.Token(t.Context())
	require.NoError(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "projected-token", entries[0].Name())
}

func TestKubernetesCredentialManifestsUseProjectedEnrollmentIdentity(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))

	config, err := os.ReadFile(filepath.Join(root, "manifests", "base", "k8s-agent", "config-configmap.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), "enrollmentPolicy:")
	assert.Contains(t, string(config), "serviceAccountTokenFile: /var/run/secrets/unified-cd-agent/token")
	assert.NotContains(t, string(config), "agentId:")
	assert.NotContains(t, string(config), "token:")
	assert.NotContains(t, string(config), "server: http://")

	deployment, err := os.ReadFile(filepath.Join(root, "manifests", "base", "k8s-agent", "deployment.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(deployment), "audience: unified-cd-agent-enrollment")
	assert.Contains(t, string(deployment), "mountPath: /var/run/secrets/unified-cd-agent")
	assert.Contains(t, string(deployment), "readOnly: true")
	assert.NotContains(t, string(deployment), "UNIFIED_K8S_SECRET")
	assert.NotContains(t, string(deployment), "unified-cd-k8s-agent-secret")

	_, err = os.Stat(filepath.Join(root, "manifests", "base", "k8s-agent", "config-secret.yaml"))
	assert.True(t, os.IsNotExist(err), "the default k8s-agent token Secret must not exist")

	controllerDeployment, err := os.ReadFile(filepath.Join(root, "manifests", "base", "controller", "deployment.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(controllerDeployment), "serviceAccountName: unified-cd-controller")
	rbac, err := os.ReadFile(filepath.Join(root, "manifests", "base", "controller", "rbac.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(rbac), "resources: [\"tokenreviews\"]")
	assert.Contains(t, string(rbac), "verbs: [\"create\"]")
	assert.Contains(t, string(rbac), "resources: [\"pods\"]")
	assert.Contains(t, string(rbac), "verbs: [\"get\"]")
	assert.NotContains(t, strings.ToLower(string(rbac)), "cluster-admin")
}

func TestKubernetesCredentialEnrollmentPodReadMatchesPolicyNamespace(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))

	policy, err := os.ReadFile(filepath.Join(root, "manifests", "base", "controller", "config-configmap.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(policy), "namespaces: [unified-cd]")

	controllerRBAC, err := os.ReadFile(filepath.Join(root, "manifests", "base", "controller", "rbac.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(controllerRBAC), "kind: Role\nmetadata:\n  name: unified-cd-controller-kubernetes-enrollment\n  namespace: unified-cd")
	assert.Contains(t, string(controllerRBAC), "kind: RoleBinding\nmetadata:\n  name: unified-cd-controller-kubernetes-enrollment\n  namespace: unified-cd")

	agentRBAC, err := os.ReadFile(filepath.Join(root, "manifests", "base", "k8s-agent", "rbac.yaml"))
	require.NoError(t, err)
	assert.Contains(t, strings.ReplaceAll(string(agentRBAC), "\r\n", "\n"), "name: unified-cd-k8s-agent\n  namespace: ci")
}

func TestKubernetesCredentialRenderedProductionManifestsUseHTTPS(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	for _, name := range []string{"core-install.yaml", "agent-only.yaml"} {
		manifest, err := os.ReadFile(filepath.Join(root, "manifests", name))
		require.NoError(t, err)
		assert.NotContains(t, string(manifest), "server: http://", name)
	}
	install, err := os.ReadFile(filepath.Join(root, "manifests", "install.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(install), "allowInsecureHTTP: true")
}
