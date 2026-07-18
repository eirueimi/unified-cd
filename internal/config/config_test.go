package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// ── FindFlag ────────────────────────────────────────────────────────────────

func TestFindFlag(t *testing.T) {
	cases := []struct {
		args []string
		name string
		want string
	}{
		{[]string{"-f", "agent.yaml"}, "f", "agent.yaml"},
		{[]string{"--f", "agent.yaml"}, "f", "agent.yaml"},
		{[]string{"-f=agent.yaml"}, "f", "agent.yaml"},
		{[]string{"--f=agent.yaml"}, "f", "agent.yaml"},
		{[]string{"--server", "http://x", "-f", "cfg.yaml"}, "f", "cfg.yaml"},
		{[]string{"--server", "http://x"}, "f", ""},
		{[]string{}, "f", ""},
	}
	for _, c := range cases {
		got := config.FindFlag(c.args, c.name)
		assert.Equal(t, c.want, got, "args=%v", c.args)
	}
}

// ── OIDCConfigured ──────────────────────────────────────────────────────────

func TestOIDCConfigured(t *testing.T) {
	cases := []struct {
		name string
		oidc *config.ControllerOIDCConfig
		want bool
	}{
		{"nil", nil, false},
		{"empty", &config.ControllerOIDCConfig{}, false},
		{"issuer only", &config.ControllerOIDCConfig{Issuer: "https://idp.example.com"}, false},
		{"clientId only", &config.ControllerOIDCConfig{ClientID: "abc"}, false},
		{"both set", &config.ControllerOIDCConfig{Issuer: "https://idp.example.com", ClientID: "abc"}, true},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, config.OIDCConfigured(c.oidc), "case=%s", c.name)
	}
}

func TestControllerEffectiveResolvesKubernetesEnrollmentBootstrapPolicy(t *testing.T) {
	path := writeFile(t, `agentAuth:
  kubernetesClusters:
    - name: in-cluster
  kubernetesEnrollmentPolicies:
    - name: unified-cd-k8s-agents
      cluster: in-cluster
      namespaces: [unified-cd]
      serviceAccounts: [unified-cd-k8s-agent]
      allowedLabels: [kind:kubernetes]
      requiredLabels: [kind:kubernetes]
      capabilities: [pod, container]
      accessTokenTTL: 1h
      enabled: true
`)
	eff, err := config.ControllerEffective(path)
	require.NoError(t, err)
	require.Len(t, eff.AgentAuth.KubernetesClusters, 1)
	require.Len(t, eff.AgentAuth.KubernetesEnrollmentPolicies, 1)

	policy, err := eff.AgentAuth.KubernetesEnrollmentPolicies[0].StorePolicy()
	require.NoError(t, err)
	assert.Equal(t, "unified-cd-k8s-agents", policy.Name)
	assert.Equal(t, "kubernetes", policy.Provider)
	assert.Equal(t, time.Hour, policy.AccessTokenTTL)
	assert.True(t, policy.Enabled)
	assert.JSONEq(t, `{"cluster":"in-cluster"}`, string(policy.ProviderConfig))
	assert.JSONEq(t, `{"namespaces":["unified-cd"],"serviceAccounts":["unified-cd-k8s-agent"]}`, string(policy.SubjectConstraints))
	assert.Equal(t, []string{"kind:kubernetes"}, policy.AllowedLabels)
	assert.Equal(t, []string{"kind:kubernetes"}, policy.RequiredLabels)
	assert.Equal(t, []string{"pod", "container"}, policy.AuthorizedCapabilities)
}

func TestDefaultControllerManifestConfiguresKubernetesEnrollment(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
	data, err := os.ReadFile(filepath.Join(root, "manifests", "base", "controller", "config-configmap.yaml"))
	require.NoError(t, err)
	var manifest struct {
		Data map[string]string `yaml:"data"`
	}
	require.NoError(t, yaml.Unmarshal(data, &manifest))
	configPath := writeFile(t, manifest.Data["controller-config.yaml"])
	eff, err := config.ControllerEffective(configPath)
	require.NoError(t, err)
	require.Len(t, eff.AgentAuth.KubernetesClusters, 1)
	assert.Equal(t, "in-cluster", eff.AgentAuth.KubernetesClusters[0].Name)
	require.Len(t, eff.AgentAuth.KubernetesEnrollmentPolicies, 1)
	policy, err := eff.AgentAuth.KubernetesEnrollmentPolicies[0].StorePolicy()
	require.NoError(t, err)
	assert.True(t, policy.Enabled)
	assert.Equal(t, "unified-cd-k8s-agents", policy.Name)
	assert.Equal(t, []string{"kind:kubernetes"}, policy.RequiredLabels)
}

// ── AgentEffective ──────────────────────────────────────────────────────────

func writeFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func TestAgentEffective_EnvOnly(t *testing.T) {
	t.Setenv("UNIFIED_SERVER", "http://env-server")
	t.Setenv("UNIFIED_AGENT_TOKEN", "env-token")
	t.Setenv("UNIFIED_AGENT_ID", "env-id")
	t.Setenv("UNIFIED_AGENT_LABELS", "kind:linux, env:prod")

	eff, err := config.AgentEffective("")
	require.NoError(t, err)
	assert.Equal(t, "http://env-server", eff.Server)
	assert.Equal(t, "env-token", eff.Token)
	assert.Equal(t, "env-id", eff.ID)
	assert.Equal(t, []string{"kind:linux", "env:prod"}, eff.Labels)
}

func TestAgentEffective_CredentialFiles(t *testing.T) {
	t.Setenv("UNIFIED_AGENT_CREDENTIAL_FILE", "/env/credentials.json")
	t.Setenv("UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE", "/env/enrollment")
	path := writeFile(t, "credentialFile: /file/credentials.json\nenrollmentTokenFile: /file/enrollment\n")

	eff, err := config.AgentEffective(path)
	require.NoError(t, err)
	assert.Equal(t, "/file/credentials.json", eff.CredentialFile)
	assert.Equal(t, "/file/enrollment", eff.EnrollmentTokenFile)
}

func TestAgentEffective_FileOverridesEnv(t *testing.T) {
	t.Setenv("UNIFIED_SERVER", "http://env-server")
	t.Setenv("UNIFIED_AGENT_TOKEN", "env-token")

	path := writeFile(t, `
server: http://file-server
token: file-token
id: file-id
labels: [kind:mac, env:staging]
cacheEndpoint: minio:9000
cacheKey: key
cacheSecret: secret
cacheBucket: bucket
`)
	eff, err := config.AgentEffective(path)
	require.NoError(t, err)
	assert.Equal(t, "http://file-server", eff.Server)
	assert.Equal(t, "file-token", eff.Token)
	assert.Equal(t, "file-id", eff.ID)
	assert.Equal(t, []string{"kind:mac", "env:staging"}, eff.Labels)
	assert.Equal(t, "minio:9000", eff.CacheEndpoint)
}

func TestAgentEffective_EnvPreservedWhenFileEmpty(t *testing.T) {
	t.Setenv("UNIFIED_SERVER", "http://env-server")

	path := writeFile(t, "id: only-id\n")
	eff, err := config.AgentEffective(path)
	require.NoError(t, err)
	assert.Equal(t, "http://env-server", eff.Server) // env preserved
	assert.Equal(t, "only-id", eff.ID)               // file wins
}

func TestAgentEffective_UnknownFieldRejected(t *testing.T) {
	path := writeFile(t, "unknownField: oops\n")
	_, err := config.AgentEffective(path)
	require.Error(t, err)
}

func TestAgentEffective_FileNotFound(t *testing.T) {
	_, err := config.AgentEffective(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)
}

func TestAgentConfig_LabelsString(t *testing.T) {
	cfg := &config.AgentConfig{Labels: []string{"kind:linux", "env:prod"}}
	assert.Equal(t, "kind:linux,env:prod", cfg.LabelsString())
}

// ── ControllerEffective ─────────────────────────────────────────────────────────

func TestControllerEffective_EnvOnly(t *testing.T) {
	t.Setenv("UNIFIED_DB_DSN", "postgres://env")
	t.Setenv("UNIFIED_TOKEN", "env-token")

	eff, err := config.ControllerEffective("")
	require.NoError(t, err)
	assert.Equal(t, "postgres://env", eff.DSN)
	assert.Equal(t, "env-token", eff.Token)
	assert.Nil(t, eff.OIDC)
	require.NotNil(t, eff.AgentAuth)
	assert.Empty(t, eff.AgentAuth.LegacySharedToken, "UNIFIED_TOKEN must remain human PAT-only")
}

func TestControllerLegacyAgentAuthConfiguration(t *testing.T) {
	t.Setenv("UNIFIED_AGENT_LEGACY_TOKEN", "legacy")

	t.Run("environment enables explicit compatibility mode", func(t *testing.T) {
		eff, err := config.ControllerEffective("")
		require.NoError(t, err)
		require.NotNil(t, eff.AgentAuth)
		assert.Equal(t, "legacy", eff.AgentAuth.LegacySharedToken)
	})

	t.Run("YAML overrides environment", func(t *testing.T) {
		path := writeFile(t, `agentAuth:
  legacySharedToken: yaml-legacy
  kubernetesClusters:
    - name: prod
      kubeconfig: /secrets/prod-kubeconfig
`)
		eff, err := config.ControllerEffective(path)
		require.NoError(t, err)
		require.NotNil(t, eff.AgentAuth)
		assert.Equal(t, "yaml-legacy", eff.AgentAuth.LegacySharedToken)
		require.Len(t, eff.AgentAuth.KubernetesClusters, 1)
		assert.Equal(t, "prod", eff.AgentAuth.KubernetesClusters[0].Name)
	})

	t.Run("empty YAML compatibility setting keeps UCA-only mode", func(t *testing.T) {
		path := writeFile(t, "agentAuth:\n  legacySharedToken: ''\n")
		eff, err := config.ControllerEffective(path)
		require.NoError(t, err)
		require.NotNil(t, eff.AgentAuth)
		assert.Empty(t, eff.AgentAuth.LegacySharedToken)
	})
}

func TestControllerAgentAuth_RejectsInvalidKubernetesClusters(t *testing.T) {
	for _, yaml := range []string{
		"agentAuth:\n  kubernetesClusters:\n    - name: ''\n      kubeconfig: /one\n",
		"agentAuth:\n  kubernetesClusters:\n    - name: prod\n      kubeconfig: /one\n    - name: prod\n      kubeconfig: /two\n",
		"agentAuth:\n  kubernetesClusters:\n    - name: one\n    - name: two\n",
	} {
		_, err := config.ControllerEffective(writeFile(t, yaml))
		require.Error(t, err)
	}
}

func TestControllerEffective_FileOverridesEnv(t *testing.T) {
	t.Setenv("UNIFIED_DB_DSN", "postgres://env")
	t.Setenv("UNIFIED_TOKEN", "env-token")

	path := writeFile(t, `
dsn: postgres://file
token: file-token
addr: :9090
s3Endpoint: minio:9000
s3Bucket: bucket
s3Key: key
s3Secret: secret
`)
	eff, err := config.ControllerEffective(path)
	require.NoError(t, err)
	assert.Equal(t, "postgres://file", eff.DSN)
	assert.Equal(t, "file-token", eff.Token)
	assert.Equal(t, ":9090", eff.Addr)
	assert.Equal(t, "minio:9000", eff.S3Endpoint)
}

func TestControllerEffective_OIDCFromFile(t *testing.T) {
	path := writeFile(t, `
dsn: postgres://x
token: tok
oidc:
  issuer: https://accounts.google.com
  clientId: client-id
  clientSecret: secret
`)
	eff, err := config.ControllerEffective(path)
	require.NoError(t, err)
	require.NotNil(t, eff.OIDC)
	assert.Equal(t, "https://accounts.google.com", eff.OIDC.Issuer)
	assert.Equal(t, "client-id", eff.OIDC.ClientID)
}

func TestControllerEffective_OIDCEnvOverriddenByFile(t *testing.T) {
	t.Setenv("UNIFIED_OIDC_ISSUER", "https://env-idp")
	t.Setenv("UNIFIED_OIDC_CLIENT_ID", "env-client")

	path := writeFile(t, `
dsn: postgres://x
token: tok
oidc:
  issuer: https://file-idp
  clientId: file-client
`)
	eff, err := config.ControllerEffective(path)
	require.NoError(t, err)
	require.NotNil(t, eff.OIDC)
	assert.Equal(t, "https://file-idp", eff.OIDC.Issuer)
	assert.Equal(t, "file-client", eff.OIDC.ClientID)
}

func TestControllerEffective_ControllerKeyFromFile(t *testing.T) {
	path := writeFile(t, "dsn: postgres://x\ntoken: tok\ncontrollerKey: deadbeef\n")
	eff, err := config.ControllerEffective(path)
	require.NoError(t, err)
	assert.Equal(t, "deadbeef", eff.ControllerKey)
}

func TestAgentEffective_WorkspaceDirFromEnv(t *testing.T) {
	t.Setenv("UNIFIED_SERVER", "http://env-server")
	t.Setenv("UNIFIED_AGENT_TOKEN", "env-token")
	t.Setenv("UNIFIED_AGENT_WORKSPACE_DIR", "/data/ws")

	eff, err := config.AgentEffective("")
	require.NoError(t, err)
	assert.Equal(t, "/data/ws", eff.WorkspaceDir)
}

func TestAgentEffective_WorkspaceDirFileOverridesEnv(t *testing.T) {
	t.Setenv("UNIFIED_SERVER", "http://env-server")
	t.Setenv("UNIFIED_AGENT_TOKEN", "env-token")
	t.Setenv("UNIFIED_AGENT_WORKSPACE_DIR", "/env/ws")

	path := writeFile(t, `
server: http://file-server
token: file-token
id: file-id
workspaceDir: /file/ws
`)
	eff, err := config.AgentEffective(path)
	require.NoError(t, err)
	assert.Equal(t, "/file/ws", eff.WorkspaceDir, "file workspaceDir should override the env value")
}

func TestAgentEffective_WorkspaceRetentionDaysDefaultZero(t *testing.T) {
	t.Setenv("UNIFIED_SERVER", "http://env-server")
	t.Setenv("UNIFIED_AGENT_TOKEN", "env-token")

	eff, err := config.AgentEffective("")
	require.NoError(t, err)
	assert.Zero(t, eff.WorkspaceRetentionDays, "workspace GC must default to disabled")
}

func TestAgentEffective_WorkspaceRetentionDaysFromEnv(t *testing.T) {
	t.Setenv("UNIFIED_SERVER", "http://env-server")
	t.Setenv("UNIFIED_AGENT_TOKEN", "env-token")
	t.Setenv("UNIFIED_AGENT_WORKSPACE_RETENTION_DAYS", "14")

	eff, err := config.AgentEffective("")
	require.NoError(t, err)
	assert.Equal(t, 14, eff.WorkspaceRetentionDays)
}

func TestAgentEffective_WorkspaceRetentionDaysFileOverridesEnv(t *testing.T) {
	t.Setenv("UNIFIED_SERVER", "http://env-server")
	t.Setenv("UNIFIED_AGENT_TOKEN", "env-token")
	t.Setenv("UNIFIED_AGENT_WORKSPACE_RETENTION_DAYS", "14")

	path := writeFile(t, `
server: http://file-server
token: file-token
id: file-id
workspaceRetentionDays: 30
`)
	eff, err := config.AgentEffective(path)
	require.NoError(t, err)
	assert.Equal(t, 30, eff.WorkspaceRetentionDays, "file workspaceRetentionDays should override the env value")
}
