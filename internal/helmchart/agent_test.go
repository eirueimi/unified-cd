package helmchart

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func agentConfig(t *testing.T, objs []Object) map[string]any {
	t.Helper()
	cm := Find(objs, "ConfigMap", "unified-cd-k8s-agent-config")
	require.NotNil(t, cm)
	raw, ok := dig(t, map[string]any(cm), "data", "k8s-agent-config.yaml").(string)
	require.True(t, ok)
	var cfg map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(raw), &cfg))
	return cfg
}

func podChecksum(t *testing.T, objs []Object, name string) any {
	t.Helper()
	dep := map[string]any(Find(objs, "Deployment", name))
	require.NotNil(t, dep)
	return dig(t, dep, "spec", "template", "metadata", "annotations", "checksum/config")
}

func TestAgentObjectsMatchBase(t *testing.T) {
	objs := Render(t, "unified-cd")
	dep := map[string]any(Find(objs, "Deployment", "unified-cd-k8s-agent"))
	require.NotNil(t, dep)
	assert.Equal(t, "unified-cd", Object(dep).Namespace())
	assert.Equal(t, 2, dig(t, dep, "spec", "replicas"))
	assert.Equal(t, map[string]any{"app": "unified-cd-k8s-agent"}, dig(t, dep, "spec", "selector", "matchLabels"))
	assert.Equal(t, "unified-cd-k8s-agent", dig(t, dep, "metadata", "labels", "app"))
	assert.Equal(t, "unified-cd-k8s-agent", dig(t, dep, "spec", "template", "metadata", "labels", "app"))
	assert.Equal(t, "unified-cd-k8s-agent", dig(t, dep, "spec", "template", "spec", "serviceAccountName"))

	c := dig(t, dep, "spec", "template", "spec", "containers", 0)
	assert.Equal(t, "ghcr.io/eirueimi/unified-cd-k8s-agent:latest", dig(t, c, "image"))
	assert.Equal(t, "50m", dig(t, c, "resources", "requests", "cpu"))
	assert.Equal(t, "128Mi", dig(t, c, "resources", "requests", "memory"))
	assert.Equal(t, "256Mi", dig(t, c, "resources", "limits", "memory"))
	assert.Equal(t, "UNIFIED_K8S_CONFIG", dig(t, c, "env", 0, "name"))
	assert.Equal(t, "/etc/unified-cd/config.yaml", dig(t, c, "env", 0, "value"))
	assert.Equal(t, "/etc/unified-cd/config.yaml", dig(t, c, "volumeMounts", 0, "mountPath"))
	assert.Equal(t, "k8s-agent-config.yaml", dig(t, c, "volumeMounts", 0, "subPath"))
	assert.Equal(t, "/var/run/secrets/unified-cd-agent", dig(t, c, "volumeMounts", 1, "mountPath"))
	assert.Equal(t, true, dig(t, c, "volumeMounts", 1, "readOnly"))

	vols := dig(t, dep, "spec", "template", "spec", "volumes")
	assert.Equal(t, "unified-cd-k8s-agent-config", dig(t, vols, 0, "configMap", "name"))
	tok := dig(t, vols, 1, "projected", "sources", 0, "serviceAccountToken")
	assert.Equal(t, "token", dig(t, tok, "path"))
	assert.Equal(t, "unified-cd-agent-enrollment", dig(t, tok, "audience"))
	assert.Equal(t, 3600, dig(t, tok, "expirationSeconds"))

	sa := Find(objs, "ServiceAccount", "unified-cd-k8s-agent")
	require.NotNil(t, sa)
	assert.Equal(t, "unified-cd", sa.Namespace())

	role := map[string]any(Find(objs, "Role", "unified-cd-k8s-agent"))
	require.NotNil(t, role)
	assert.Equal(t, "ci", Object(role).Namespace())
	assert.Equal(t, []any{"create", "get", "list", "delete", "watch", "update", "patch"}, dig(t, role, "rules", 0, "verbs"))
	assert.Equal(t, []any{"pods", "pods/exec", "pods/log"}, dig(t, role, "rules", 0, "resources"))
	assert.Equal(t, []any{"create", "get", "list", "update", "delete"}, dig(t, role, "rules", 1, "verbs"))
	assert.Equal(t, []any{"persistentvolumeclaims"}, dig(t, role, "rules", 1, "resources"))

	rb := map[string]any(Find(objs, "RoleBinding", "unified-cd-k8s-agent"))
	require.NotNil(t, rb)
	assert.Equal(t, "ci", Object(rb).Namespace())
	assert.Equal(t, "unified-cd", dig(t, rb, "subjects", 0, "namespace"))
	assert.Equal(t, "unified-cd-k8s-agent", dig(t, rb, "roleRef", "name"))
}

func TestAgentDefaultServerIsInCluster(t *testing.T) {
	cfg := agentConfig(t, Render(t, "unified-cd"))
	assert.Equal(t, "http://unified-cd-controller.unified-cd.svc.cluster.local", cfg["server"])
	assert.Equal(t, true, cfg["allowInsecureHTTP"])
	assert.Equal(t, "broker", cfg["sidecarS3SecretMode"])
	assert.Equal(t, "unified-cd-k8s-agents", cfg["enrollmentPolicy"])
	assert.Equal(t, "/var/run/secrets/unified-cd-agent/token", cfg["serviceAccountTokenFile"])
	assert.Equal(t, "golang:1.24-alpine", cfg["podImage"])

	cfg = agentConfig(t, Render(t, "cd-system"))
	assert.Equal(t, "http://unified-cd-controller.cd-system.svc.cluster.local", cfg["server"])

	// The broker mode follows bundledStores.enabled.
	cfg = agentConfig(t, Render(t, "unified-cd", "bundledStores.enabled=false", "controller.existingSecret=my-secret"))
	assert.NotContains(t, cfg, "sidecarS3SecretMode")
}

func TestAgentOnlyRequiresHTTPS(t *testing.T) {
	objs := Render(t, "unified-cd",
		"controller.enabled=false", "bundledStores.enabled=false", "agent.server=https://cd.example.com")
	cfg := agentConfig(t, objs)
	assert.Equal(t, "https://cd.example.com", cfg["server"])
	assert.NotContains(t, cfg, "allowInsecureHTTP")
	assert.NotContains(t, cfg, "sidecarS3SecretMode")
	for _, o := range objs {
		assert.NotContains(t, o.Name(), "unified-cd-controller", "%s/%s", o.Kind(), o.Name())
	}

	_, stderr, err := RenderErr("unified-cd", "agent.server=http://cd.example.com")
	require.Error(t, err)
	assert.Contains(t, stderr, "agent.server must be an https:// URL")
}

func TestJobNamespaceMovesTogether(t *testing.T) {
	objs := Render(t, "unified-cd", "jobNamespace.name=builds")
	ns := map[string]any(Find(objs, "Namespace", "builds"))
	require.NotNil(t, ns)
	assert.Equal(t, "baseline", dig(t, ns, "metadata", "labels", "pod-security.kubernetes.io/enforce"))
	assert.Equal(t, "restricted", dig(t, ns, "metadata", "labels", "pod-security.kubernetes.io/audit"))
	assert.Equal(t, "keep", dig(t, ns, "metadata", "annotations", "helm.sh/resource-policy"))
	assert.Nil(t, Find(objs, "Namespace", "ci"))
	assert.Nil(t, Find(objs, "Namespace", "unified-cd"), "the release namespace is never rendered")

	assert.Equal(t, "builds", Object(Find(objs, "Role", "unified-cd-k8s-agent")).Namespace())
	assert.Equal(t, "builds", Object(Find(objs, "RoleBinding", "unified-cd-k8s-agent")).Namespace())
	assert.Equal(t, "builds", agentConfig(t, objs)["namespace"])
	assert.Equal(t, []any{"builds"},
		dig(t, controllerConfig(t, objs), "agentAuth", "kubernetesStoreCredentialPolicies", 0, "namespaces"))

	objs = Render(t, "unified-cd", "jobNamespace.podSecurity.enforce=privileged", "jobNamespace.podSecurity.audit=baseline")
	ns = map[string]any(Find(objs, "Namespace", "ci"))
	require.NotNil(t, ns)
	assert.Equal(t, "privileged", dig(t, ns, "metadata", "labels", "pod-security.kubernetes.io/enforce"))
	assert.Equal(t, "baseline", dig(t, ns, "metadata", "labels", "pod-security.kubernetes.io/audit"))
}

func TestJobNamespaceCreateFalse(t *testing.T) {
	objs := Render(t, "unified-cd", "jobNamespace.create=false")
	for _, o := range objs {
		assert.NotEqual(t, "Namespace", o.Kind())
	}
	assert.NotNil(t, Find(objs, "Role", "unified-cd-k8s-agent"), "the Role is still rendered in the existing namespace")
}

func TestAgentConfigDeepMerge(t *testing.T) {
	cfg := agentConfig(t, Render(t, "unified-cd", "agent.config.maxConcurrent=10"))
	assert.EqualValues(t, 10, cfg["maxConcurrent"])
	assert.Equal(t, "unified-cd-k8s-agents", cfg["enrollmentPolicy"])
	assert.Equal(t, []any{"kind:kubernetes"}, cfg["labels"])
	assert.Equal(t, "golang:1.24-alpine", cfg["podImage"])
}

func TestConfigOverridesWinOverZeroValues(t *testing.T) {
	cfg := agentConfig(t, Render(t, "unified-cd", "agent.config.allowInsecureHTTP=false"))
	assert.Equal(t, false, cfg["allowInsecureHTTP"])

	cfg = agentConfig(t, Render(t, "unified-cd", "agent.config.maxConcurrent=0"))
	assert.EqualValues(t, 0, cfg["maxConcurrent"])

	// A false boolean nested under a controller map is kept as well.
	objs := Render(t, "unified-cd", "controller.config.server.tls=false")
	assert.Equal(t, false, dig(t, controllerConfig(t, objs), "server", "tls"))
}

func TestConfigChangeRestartsOnlyItsDeployment(t *testing.T) {
	base := Render(t, "unified-cd")
	changed := Render(t, "unified-cd", "agent.config.maxConcurrent=10")
	assert.NotEmpty(t, podChecksum(t, base, "unified-cd-k8s-agent"))
	assert.NotEqual(t, podChecksum(t, base, "unified-cd-k8s-agent"), podChecksum(t, changed, "unified-cd-k8s-agent"))
	assert.Equal(t, podChecksum(t, base, "unified-cd-controller"), podChecksum(t, changed, "unified-cd-controller"))

	changed = Render(t, "unified-cd", "controller.config.server.listen=:9090")
	assert.NotEqual(t, podChecksum(t, base, "unified-cd-controller"), podChecksum(t, changed, "unified-cd-controller"))
	assert.Equal(t, podChecksum(t, base, "unified-cd-k8s-agent"), podChecksum(t, changed, "unified-cd-k8s-agent"))
}

func TestPodAnnotationsMergeWithChecksum(t *testing.T) {
	objs := Render(t, "unified-cd", "agent.podAnnotations.team=ci", "controller.podAnnotations.team=cd")
	for name, team := range map[string]string{"unified-cd-k8s-agent": "ci", "unified-cd-controller": "cd"} {
		dep := map[string]any(Find(objs, "Deployment", name))
		assert.Equal(t, team, dig(t, dep, "spec", "template", "metadata", "annotations", "team"))
		assert.NotEmpty(t, podChecksum(t, objs, name))
	}
}
