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

func TestControllerStoreCredentialPolicyFollowsBundledStores(t *testing.T) {
	objs := Render(t, "unified-cd",
		"bundledStores.enabled=false", "controller.existingSecret=my-secret", "agent.server=https://cd.example.com")
	cfg := controllerConfig(t, objs)
	assert.Nil(t, dig(t, cfg, "agentAuth", "kubernetesStoreCredentialPolicies"))
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
			if o.Kind() == k && strings.HasPrefix(o.Name(), "unified-cd-controller") {
				assert.Equalf(t, "cd-system", o.Namespace(), "%s/%s", k, o.Name())
			}
		}
	}
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
