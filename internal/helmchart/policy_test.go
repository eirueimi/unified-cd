package helmchart

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func storePolicySAs(t *testing.T, set ...string) []any {
	t.Helper()
	cfg := controllerConfig(t, Render(t, "unified-cd", set...))
	return dig(t, cfg, "agentAuth", "kubernetesStoreCredentialPolicies", 0, "serviceAccounts").([]any)
}

func TestStorePolicyDefaultServiceAccounts(t *testing.T) {
	assert.Equal(t, []any{"default"}, storePolicySAs(t))
}

// The default store policy follows the ServiceAccounts the rendered agent
// config can run job Pods under.
func TestStorePolicyDerivesServiceAccountsFromAgentConfig(t *testing.T) {
	got := storePolicySAs(t,
		"agent.config.podTemplates.zeta.spec.serviceAccountName=zeta",
		"agent.config.podTemplates.alpha.spec.serviceAccountName=alpha",
		"agent.config.podTemplates.dup.spec.serviceAccountName=alpha",
		"agent.config.podTemplates.legacy.spec.serviceAccount=legacy",
		"agent.config.podTemplates.none.spec.containers[0].name=x",
		"agent.config.jobPodPolicy.allowServiceAccounts={builder,default}",
	)
	assert.Equal(t, []any{"alpha", "builder", "default", "legacy", "zeta"}, got)
}

// A supplied list replaces the computed one entirely (nothing is derived).
func TestStorePolicySuppliedListReplacesDerived(t *testing.T) {
	cfg := controllerConfig(t, Render(t, "unified-cd",
		"agent.config.podTemplates.t.spec.serviceAccountName=builder",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].cluster=in-cluster",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].namespaces={ci}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].serviceAccounts={only}",
	))
	assert.Equal(t, []any{"only"}, dig(t, cfg, "agentAuth", "kubernetesStoreCredentialPolicies", 0, "serviceAccounts"))
	assert.Len(t, dig(t, cfg, "agentAuth", "kubernetesStoreCredentialPolicies"), 1)
}

// F6: agent.config.namespace cannot diverge from jobNamespace.name.
func TestAgentConfigNamespaceOverrideFailsRender(t *testing.T) {
	_, stderr, err := RenderErr("unified-cd", "agent.config.namespace=builds")
	require.Error(t, err)
	assert.Contains(t, stderr, "jobNamespace.name")
	assert.Contains(t, stderr, "agent.config.namespace")

	// Setting it to the same value is harmless.
	Render(t, "unified-cd", "agent.config.namespace=ci")
	// Moving the namespace through jobNamespace.name is the supported way.
	Render(t, "unified-cd", "jobNamespace.name=builds")
}

// F7: replacing kubernetesClusters without "in-cluster".
func TestComputedPoliciesFollowFirstOwnCluster(t *testing.T) {
	cfg := controllerConfig(t, Render(t, "unified-cd",
		"controller.config.agentAuth.kubernetesClusters[0].name=remote",
		"controller.config.agentAuth.kubernetesClusters[0].kubeconfig=/etc/kube/remote",
		"controller.config.agentAuth.kubernetesClusters[1].name=home",
	))
	assert.Equal(t, "home", dig(t, cfg, "agentAuth", "kubernetesEnrollmentPolicies", 0, "cluster"))
	assert.Equal(t, "home", dig(t, cfg, "agentAuth", "kubernetesStoreCredentialPolicies", 0, "cluster"))
}

func TestNoOwnClusterFailsRender(t *testing.T) {
	_, stderr, err := RenderErr("unified-cd",
		"controller.config.agentAuth.kubernetesClusters[0].name=remote",
		"controller.config.agentAuth.kubernetesClusters[0].kubeconfig=/etc/kube/remote",
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "kubernetesClusters")
	assert.Contains(t, stderr, "kubernetesEnrollmentPolicies")
}

// Supplying both the clusters and the policies that name them is valid, even
// with no own cluster.
func TestRemoteClusterWithOwnPoliciesRenders(t *testing.T) {
	Render(t, "unified-cd",
		"controller.config.agentAuth.kubernetesClusters[0].name=remote",
		"controller.config.agentAuth.kubernetesClusters[0].kubeconfig=/etc/kube/remote",
		"controller.config.agentAuth.kubernetesEnrollmentPolicies[0].name=p",
		"controller.config.agentAuth.kubernetesEnrollmentPolicies[0].cluster=remote",
		"controller.config.agentAuth.kubernetesEnrollmentPolicies[0].namespaces={a}",
		"controller.config.agentAuth.kubernetesEnrollmentPolicies[0].serviceAccounts={b}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].cluster=remote",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].namespaces={ci}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].serviceAccounts={default}",
	)
}
