package main

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestConfigureKubernetesEnrollmentClientAppliesBoundedTimeout(t *testing.T) {
	kubeConfig := &rest.Config{Timeout: time.Minute}
	configureKubernetesEnrollmentClient(kubeConfig)
	assert.Equal(t, controller.KubernetesEnrollmentRequestTimeout, kubeConfig.Timeout)
}

func TestBuildKubernetesEnrollmentConfigUsesInClusterIdentityWhenUnspecified(t *testing.T) {
	original := inClusterKubernetesConfig
	inClusterKubernetesConfig = func() (*rest.Config, error) { return &rest.Config{Host: "https://kubernetes.default.svc"}, nil }
	t.Cleanup(func() { inClusterKubernetesConfig = original })

	got, err := buildKubernetesEnrollmentConfig("")
	require.NoError(t, err)
	assert.Equal(t, "https://kubernetes.default.svc", got.Host)
}

type policyBootstrapStore struct{ policies []store.AgentEnrollmentPolicy }

func (s *policyBootstrapStore) UpsertAgentEnrollmentPolicy(_ context.Context, policy store.AgentEnrollmentPolicy) (*store.AgentEnrollmentPolicy, error) {
	s.policies = append(s.policies, policy)
	return &policy, nil
}

func TestBootstrapKubernetesEnrollmentPoliciesUpsertsEnabledPolicyBeforeServing(t *testing.T) {
	st := &policyBootstrapStore{}
	auth := &config.ControllerAgentAuthConfig{KubernetesEnrollmentPolicies: []config.ControllerKubernetesEnrollmentPolicyConfig{{
		Name: "unified-cd-k8s-agents", Cluster: "in-cluster", Namespaces: []string{"unified-cd"}, ServiceAccounts: []string{"unified-cd-k8s-agent"},
		AllowedLabels: []string{"kind:kubernetes"}, RequiredLabels: []string{"kind:kubernetes"}, Capabilities: []string{"pod", "container"}, AccessTokenTTL: "1h", Enabled: true,
	}}}

	require.NoError(t, bootstrapKubernetesEnrollmentPolicies(t.Context(), st, auth))
	require.Len(t, st.policies, 1)
	assert.Equal(t, "unified-cd-k8s-agents", st.policies[0].Name)
	assert.True(t, st.policies[0].Enabled)
	assert.Equal(t, []string{"kind:kubernetes"}, st.policies[0].RequiredLabels)
	assert.Equal(t, []string{"pod", "container"}, st.policies[0].AuthorizedCapabilities)
}

// TestEnvIntOr covers the malformed-value warning path added for review
// finding M10: envIntOr runs at flag-registration time, before the slog
// default logger exists, so it can't log directly — it writes a message into
// the caller-supplied *string instead, for the caller to log once the logger
// is ready.
func TestEnvIntOr(t *testing.T) {
	t.Run("unset falls back to default with no warning", func(t *testing.T) {
		t.Setenv("UNIFIED_TEST_ENVINTOR", "")
		var warning string
		got := envIntOr("UNIFIED_TEST_ENVINTOR", 64, &warning)
		assert.Equal(t, 64, got)
		assert.Empty(t, warning)
	})

	t.Run("valid value parses with no warning", func(t *testing.T) {
		t.Setenv("UNIFIED_TEST_ENVINTOR", "128")
		var warning string
		got := envIntOr("UNIFIED_TEST_ENVINTOR", 64, &warning)
		assert.Equal(t, 128, got)
		assert.Empty(t, warning)
	})

	t.Run("malformed value falls back to default and records a warning", func(t *testing.T) {
		t.Setenv("UNIFIED_TEST_ENVINTOR", "not-a-number")
		var warning string
		got := envIntOr("UNIFIED_TEST_ENVINTOR", 64, &warning)
		assert.Equal(t, 64, got, "malformed value should fall back to default")
		assert.NotEmpty(t, warning, "malformed value should record a warning message")
		assert.Contains(t, warning, "UNIFIED_TEST_ENVINTOR")
		assert.Contains(t, warning, "not-a-number")
	})

	t.Run("nil warning pointer is safe", func(t *testing.T) {
		t.Setenv("UNIFIED_TEST_ENVINTOR", "not-a-number")
		got := envIntOr("UNIFIED_TEST_ENVINTOR", 64, nil)
		assert.Equal(t, 64, got)
	})
}

func TestQueuedRunGraceDefault(t *testing.T) {
	t.Setenv("UNIFIED_QUEUED_RUN_GRACE", "")
	assert.Equal(t, 5*time.Minute, queuedRunGraceDefault())

	t.Setenv("UNIFIED_QUEUED_RUN_GRACE", "20m")
	assert.Equal(t, 20*time.Minute, queuedRunGraceDefault())

	t.Setenv("UNIFIED_QUEUED_RUN_GRACE", "bogus")
	assert.Equal(t, 5*time.Minute, queuedRunGraceDefault())

	t.Setenv("UNIFIED_QUEUED_RUN_GRACE", "-1m")
	assert.Equal(t, 5*time.Minute, queuedRunGraceDefault())
}

func TestGitResolveDeadlineDefault(t *testing.T) {
	t.Setenv("UNIFIED_GIT_RESOLVE_DEADLINE", "")
	assert.Equal(t, time.Hour, gitResolveDeadlineDefault(), "unset falls back to default")

	t.Setenv("UNIFIED_GIT_RESOLVE_DEADLINE", "2h")
	assert.Equal(t, 2*time.Hour, gitResolveDeadlineDefault())

	t.Setenv("UNIFIED_GIT_RESOLVE_DEADLINE", "bogus")
	assert.Equal(t, time.Hour, gitResolveDeadlineDefault(), "invalid value falls back to default")

	t.Setenv("UNIFIED_GIT_RESOLVE_DEADLINE", "-1m")
	assert.Equal(t, time.Hour, gitResolveDeadlineDefault(), "negative value falls back to default")

	t.Setenv("UNIFIED_GIT_RESOLVE_DEADLINE", "0")
	assert.Equal(t, time.Hour, gitResolveDeadlineDefault(), "0 must NOT disable the deadline — it falls back to default")
}

func TestLegacyAgentAuthWarning(t *testing.T) {
	assert.Empty(t, legacyAgentAuthWarning(""))
	assert.Contains(t, legacyAgentAuthWarning("migration-only-token"), "UNIFIED_AGENT_LEGACY_TOKEN")
}
