package helmchart

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// dig walks nested maps/slices by key (string) or index (int).
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			require.Truef(t, ok, "expected map at %q, got %T", k, v)
			v = m[k]
		case int:
			s, ok := v.([]any)
			require.Truef(t, ok, "expected list at [%d], got %T", k, v)
			require.Less(t, k, len(s))
			v = s[k]
		}
	}
	return v
}

func controllerConfig(t *testing.T, objs []Object) map[string]any {
	t.Helper()
	cm := Find(objs, "ConfigMap", "unified-cd-controller-config")
	require.NotNil(t, cm)
	raw, ok := dig(t, map[string]any(cm), "data", "controller-config.yaml").(string)
	require.True(t, ok)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(raw), &cfg))
	return cfg
}

func TestControllerObjectsMatchBase(t *testing.T) {
	objs := Render(t, "unified-cd")
	for _, want := range []struct{ kind, name string }{
		{"Deployment", "unified-cd-controller"},
		{"Service", "unified-cd-controller"},
		{"ServiceAccount", "unified-cd-controller"},
		{"ClusterRole", "unified-cd-controller-kubernetes-enrollment"},
		{"ClusterRoleBinding", "unified-cd-controller-kubernetes-enrollment"},
		{"Role", "unified-cd-controller-kubernetes-enrollment"},
		{"RoleBinding", "unified-cd-controller-kubernetes-enrollment"},
		{"ConfigMap", "unified-cd-controller-config"},
	} {
		assert.NotNilf(t, Find(objs, want.kind, want.name), "%s/%s", want.kind, want.name)
	}

	dep := map[string]any(Find(objs, "Deployment", "unified-cd-controller"))
	assert.Equal(t, "unified-cd", Object(dep).Namespace())
	assert.Equal(t, 1, dig(t, dep, "spec", "replicas"))
	assert.Equal(t, map[string]any{"app": "unified-cd-controller"}, dig(t, dep, "spec", "selector", "matchLabels"))
	assert.Equal(t, "unified-cd-controller", dig(t, dep, "metadata", "labels", "app"))
	assert.Equal(t, "unified-cd", dig(t, dep, "metadata", "labels", "app.kubernetes.io/name"))

	c := dig(t, dep, "spec", "template", "spec", "containers", 0)
	assert.Equal(t, "ghcr.io/eirueimi/unified-cd-controller:latest", dig(t, c, "image"))
	assert.Equal(t, []any{"-f", "/etc/unified-cd/controller-config.yaml"}, dig(t, c, "args"))
	assert.Equal(t, 8080, dig(t, c, "ports", 0, "containerPort"))
	assert.Equal(t, "/healthz", dig(t, c, "livenessProbe", "httpGet", "path"))
	assert.Equal(t, "/readyz", dig(t, c, "readinessProbe", "httpGet", "path"))
	assert.Equal(t, "100m", dig(t, c, "resources", "requests", "cpu"))
	assert.Equal(t, "256Mi", dig(t, c, "resources", "requests", "memory"))
	assert.Equal(t, "512Mi", dig(t, c, "resources", "limits", "memory"))
	assert.Equal(t, "UNIFIED_CONTROLLER_KEY_FILE", dig(t, c, "env", 0, "name"))
	assert.Equal(t, "/etc/unified-cd/kek", dig(t, c, "env", 0, "value"))
	assert.Equal(t, "unified-cd-controller", dig(t, c, "envFrom", 0, "secretRef", "name"))

	vols := dig(t, dep, "spec", "template", "spec", "volumes")
	assert.Equal(t, "unified-cd-controller-config", dig(t, vols, 0, "configMap", "name"))
	assert.Equal(t, "unified-cd-controller-kek", dig(t, vols, 1, "secret", "secretName"))
	assert.Equal(t, 256, dig(t, vols, 1, "secret", "defaultMode"), "0400 octal")

	svc := map[string]any(Find(objs, "Service", "unified-cd-controller"))
	assert.Equal(t, "ClusterIP", dig(t, svc, "spec", "type"))
	assert.Equal(t, 80, dig(t, svc, "spec", "ports", 0, "port"))
	assert.Equal(t, 8080, dig(t, svc, "spec", "ports", 0, "targetPort"))
	assert.Equal(t, map[string]any{"app": "unified-cd-controller"}, dig(t, svc, "spec", "selector"))

	cfg := controllerConfig(t, objs)
	pol := dig(t, cfg, "agentAuth", "kubernetesEnrollmentPolicies", 0)
	assert.Equal(t, "unified-cd-k8s-agents", dig(t, pol, "name"))
	assert.Equal(t, []any{"unified-cd"}, dig(t, pol, "namespaces"))
	assert.Equal(t, []any{"unified-cd-k8s-agent"}, dig(t, pol, "serviceAccounts"))
	assert.Equal(t, []any{"kind:kubernetes"}, dig(t, pol, "requiredLabels"))
	store := dig(t, cfg, "agentAuth", "kubernetesStoreCredentialPolicies", 0)
	assert.Equal(t, []any{"ci"}, dig(t, store, "namespaces"))
	assert.Equal(t, []any{"default"}, dig(t, store, "serviceAccounts"))
}

// The policy that lets job Pods ask for presigned object URLs is rendered
// whenever the controller is, not only with the bundled stores: a production
// install with its own store needs it just as much.
func TestControllerConfig_StoreCredentialPoliciesWithoutBundledStores(t *testing.T) {
	objs := Render(t, "unified-cd",
		"bundledStores.enabled=false", "controller.existingSecret=my-secret", "agent.server=https://cd.example.com")
	cfg := controllerConfig(t, objs)
	store := dig(t, cfg, "agentAuth", "kubernetesStoreCredentialPolicies", 0)
	assert.Equal(t, "in-cluster", dig(t, store, "cluster"))
	assert.Equal(t, []any{"ci"}, dig(t, store, "namespaces"))
	assert.Equal(t, []any{"default"}, dig(t, store, "serviceAccounts"))
	dep := map[string]any(Find(objs, "Deployment", "unified-cd-controller"))
	assert.Equal(t, "my-secret", dig(t, dep, "spec", "template", "spec", "containers", 0, "envFrom", 0, "secretRef", "name"))
}

func TestControllerConfigFollowsNamespace(t *testing.T) {
	objs := Render(t, "cd-system", "jobNamespace.name=builds")
	cfg := controllerConfig(t, objs)
	assert.Equal(t, []any{"cd-system"}, dig(t, cfg, "agentAuth", "kubernetesEnrollmentPolicies", 0, "namespaces"))
	assert.Equal(t, []any{"builds"}, dig(t, cfg, "agentAuth", "kubernetesStoreCredentialPolicies", 0, "namespaces"))
	crb := map[string]any(Find(objs, "ClusterRoleBinding", "unified-cd-controller-kubernetes-enrollment"))
	assert.Equal(t, "cd-system", dig(t, crb, "subjects", 0, "namespace"))
	for _, k := range []string{"Deployment", "Service", "ServiceAccount", "ConfigMap", "Role", "RoleBinding"} {
		for _, o := range objs {
			if o.Kind() == k && strings.HasPrefix(o.Name(), "unified-cd-controller") && o.Name() != jobPodsRBACName {
				assert.Equalf(t, "cd-system", o.Namespace(), "%s/%s", k, o.Name())
			}
		}
	}
}

const jobPodsRBACName = "unified-cd-controller-job-pods"

func jobPodsObjects(objs []Object) (roles, bindings []Object) {
	for _, o := range objs {
		if o.Name() != jobPodsRBACName {
			continue
		}
		switch o.Kind() {
		case "Role":
			roles = append(roles, o)
		case "RoleBinding":
			bindings = append(bindings, o)
		}
	}
	return roles, bindings
}

func namespacesOf(objs []Object) []string {
	var ns []string
	for _, o := range objs {
		ns = append(ns, o.Namespace())
	}
	return ns
}

// The controller reads the job Pod to verify its projected token, so it needs
// pods/get in the job namespace, not only in the release namespace.
func TestControllerJobPodsRBAC_Default(t *testing.T) {
	objs := Render(t, "unified-cd")
	role := map[string]any(Find(objs, "Role", jobPodsRBACName))
	require.NotNil(t, role)
	assert.Equal(t, "ci", Object(role).Namespace())
	assert.Equal(t, []any{
		map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"get"}},
	}, dig(t, role, "rules"))
	assert.Equal(t, "unified-cd", dig(t, role, "metadata", "labels", "app.kubernetes.io/name"))

	rb := map[string]any(Find(objs, "RoleBinding", jobPodsRBACName))
	require.NotNil(t, rb)
	assert.Equal(t, "ci", Object(rb).Namespace())
	assert.Equal(t, "ServiceAccount", dig(t, rb, "subjects", 0, "kind"))
	assert.Equal(t, "unified-cd-controller", dig(t, rb, "subjects", 0, "name"))
	assert.Equal(t, "unified-cd", dig(t, rb, "subjects", 0, "namespace"))
	assert.Equal(t, "Role", dig(t, rb, "roleRef", "kind"))
	assert.Equal(t, jobPodsRBACName, dig(t, rb, "roleRef", "name"))
	assert.Equal(t, "rbac.authorization.k8s.io", dig(t, rb, "roleRef", "apiGroup"))
}

func TestControllerJobPodsRBAC_ReleaseNamespaceSkipped(t *testing.T) {
	objs := Render(t, "unified-cd", "jobNamespace.name=unified-cd")
	roles, bindings := jobPodsObjects(objs)
	assert.Empty(t, roles)
	assert.Empty(t, bindings)
	seen := map[string]bool{}
	for _, o := range objs {
		key := o.Kind() + "/" + o.Namespace() + "/" + o.Name()
		assert.Falsef(t, seen[key], "duplicate object %s", key)
		seen[key] = true
	}
}

func TestControllerJobPodsRBAC_UserPolicies(t *testing.T) {
	objs := Render(t, "unified-cd",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].cluster=in-cluster",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].namespaces={team-b,team-a,unified-cd}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].serviceAccounts={default}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[1].cluster=in-cluster",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[1].namespaces={team-a}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[1].serviceAccounts={runner}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[2].cluster=remote",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[2].namespaces={elsewhere}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[2].serviceAccounts={default}",
	)
	roles, bindings := jobPodsObjects(objs)
	assert.Equal(t, []string{"team-a", "team-b"}, namespacesOf(roles))
	assert.Equal(t, []string{"team-a", "team-b"}, namespacesOf(bindings))
}

// The controller's own cluster is the kubernetesClusters entry without a
// kubeconfig, whatever its name; a cluster with a kubeconfig is reached with
// other credentials and gets no Role from the chart.
func TestControllerJobPodsRBAC_OwnClusterByKubeconfig(t *testing.T) {
	objs := Render(t, "unified-cd",
		"controller.config.agentAuth.kubernetesClusters[0].name=home",
		"controller.config.agentAuth.kubernetesClusters[1].name=remote",
		"controller.config.agentAuth.kubernetesClusters[1].kubeconfig=/etc/kube/remote",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].cluster=home",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].namespaces={team-home}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[0].serviceAccounts={default}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[1].cluster=remote",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[1].namespaces={team-remote}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[1].serviceAccounts={default}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[2].cluster=in-cluster",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[2].namespaces={not-listed}",
		"controller.config.agentAuth.kubernetesStoreCredentialPolicies[2].serviceAccounts={default}",
	)
	roles, bindings := jobPodsObjects(objs)
	assert.Equal(t, []string{"team-home"}, namespacesOf(roles))
	assert.Equal(t, []string{"team-home"}, namespacesOf(bindings))
}

func TestControllerConfigDeepMerge(t *testing.T) {
	objs := Render(t, "unified-cd", "controller.config.server.listen=:9090")
	cfg := controllerConfig(t, objs)
	assert.Equal(t, ":9090", dig(t, cfg, "server", "listen"))
	assert.Equal(t, "unified-cd-k8s-agents", dig(t, cfg, "agentAuth", "kubernetesEnrollmentPolicies", 0, "name"))

	// A user-supplied list replaces the computed list; maps merge.
	objs = Render(t, "unified-cd", "controller.config.agentAuth.kubernetesEnrollmentPolicies[0].name=custom")
	cfg = controllerConfig(t, objs)
	pols := dig(t, cfg, "agentAuth", "kubernetesEnrollmentPolicies").([]any)
	require.Len(t, pols, 1)
	assert.Equal(t, "custom", dig(t, pols, 0, "name"))
	assert.Nil(t, dig(t, pols, 0, "cluster"), "list is replaced, not merged element-wise")
	assert.NotNil(t, dig(t, cfg, "agentAuth", "kubernetesClusters"))
}

func TestControllerImageTag(t *testing.T) {
	objs := Render(t, "unified-cd", "controller.image.tag=v9.9.9")
	dep := map[string]any(Find(objs, "Deployment", "unified-cd-controller"))
	assert.Equal(t, "ghcr.io/eirueimi/unified-cd-controller:v9.9.9",
		dig(t, dep, "spec", "template", "spec", "containers", 0, "image"))
}

func TestControllerDisabled(t *testing.T) {
	objs := Render(t, "unified-cd",
		"controller.enabled=false", "bundledStores.enabled=false", "agent.server=https://cd.example.com")
	for _, o := range objs {
		assert.NotContains(t, o.Name(), "unified-cd-controller", "%s/%s", o.Kind(), o.Name())
	}
	_, stderr, err := RenderErr("unified-cd", "controller.enabled=false", "agent.enabled=false")
	require.Error(t, err)
	assert.Contains(t, stderr, "nothing to install")
}

func TestControllerTrustedProxiesValue(t *testing.T) {
	cfg := controllerConfig(t, Render(t, "unified-cd"))
	_, set := cfg["trustedProxies"]
	assert.False(t, set, "trustedProxies is omitted by default so the controller ignores forwarded headers")

	cfg = controllerConfig(t, Render(t, "unified-cd", "controller.trustedProxies[0]=10.244.0.0/16"))
	assert.Equal(t, []any{"10.244.0.0/16"}, cfg["trustedProxies"])
}

// ingressSet enables an Ingress for cd.example.com. The bundled trial
// credentials are refused behind an Ingress, so it uses an existing Secret.
func ingressSet(extra ...string) []string {
	return append([]string{
		"bundledStores.enabled=false", "controller.existingSecret=prod",
		"ingress.enabled=true", "ingress.hosts[0].host=cd.example.com",
	}, extra...)
}

func TestControllerExternalURL_Explicit(t *testing.T) {
	cfg := controllerConfig(t, Render(t, "unified-cd",
		ingressSet("controller.externalUrl=https://x.example")...))
	assert.Equal(t, "https://x.example", cfg["externalUrl"])
}

func TestControllerExternalURL_FromIngressTLS(t *testing.T) {
	cfg := controllerConfig(t, Render(t, "unified-cd",
		ingressSet("ingress.tls[0].secretName=cd-tls", "ingress.tls[0].hosts[0]=cd.example.com")...))
	assert.Equal(t, "https://cd.example.com", cfg["externalUrl"])
}

func TestControllerExternalURL_FromIngressNoTLS(t *testing.T) {
	cfg := controllerConfig(t, Render(t, "unified-cd", ingressSet()...))
	assert.Equal(t, "http://cd.example.com", cfg["externalUrl"])
}

func TestControllerExternalURL_Unset(t *testing.T) {
	cfg := controllerConfig(t, Render(t, "unified-cd"))
	_, set := cfg["externalUrl"]
	assert.False(t, set, "externalUrl is omitted when neither a value nor an ingress host is set")
}

func TestControllerExternalURL_ConfigOverrides(t *testing.T) {
	cfg := controllerConfig(t, Render(t, "unified-cd",
		ingressSet("controller.config.externalUrl=https://override.example")...))
	assert.Equal(t, "https://override.example", cfg["externalUrl"])
}

func TestControllerConfigAccountsFromValues(t *testing.T) {
	objs := Render(t, "unified-cd")
	cfg := controllerConfig(t, objs)
	assert.NotContains(t, cfg, "serviceAccounts")
	assert.NotContains(t, cfg, "tokens")
	assert.NotContains(t, cfg, "ui")

	objs = Render(t, "unified-cd",
		"controller.serviceAccounts.ci-bot.enabled=true",
		"controller.tokens.defaultLifetime=720h",
		"controller.tokens.maxLifetime=2160h",
		"controller.ui.allowTokenEntry=true")
	cfg = controllerConfig(t, objs)
	assert.Equal(t, true, dig(t, cfg, "serviceAccounts", "ci-bot", "enabled"))
	assert.Equal(t, "720h", dig(t, cfg, "tokens", "defaultLifetime"))
	assert.Equal(t, "2160h", dig(t, cfg, "tokens", "maxLifetime"))
	assert.Equal(t, true, dig(t, cfg, "ui", "allowTokenEntry"))
}
