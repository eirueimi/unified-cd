package helmchart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func baseManifestPath(rel ...string) string {
	return filepath.Join(append([]string{ChartDir(), "..", "..", "manifests", "base"}, rel...)...)
}

// baseObject loads one manifest from manifests/base and normalises it for
// comparison with the chart: the namespace is dropped, because the chart uses
// the release namespace.
func baseObject(t *testing.T, rel string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(baseManifestPath(rel))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &m))
	delete(dig(t, m, "metadata").(map[string]any), "namespace")
	return m
}

// chartObject returns the rendered object normalised the same way, with the
// standard labels and the checksum annotation removed so that only the content
// the base manifests define is compared.
func chartObject(t *testing.T, objs []Object, kind, name string) map[string]any {
	t.Helper()
	o := Find(objs, kind, name)
	require.NotNilf(t, o, "%s/%s", kind, name)
	m := map[string]any(o)
	meta := dig(t, m, "metadata").(map[string]any)
	delete(meta, "namespace")
	if labels, ok := meta["labels"].(map[string]any); ok {
		if app, ok := labels["app"]; ok {
			meta["labels"] = map[string]any{"app": app}
		} else {
			delete(meta, "labels")
		}
	}
	if kind == "Deployment" {
		tm := dig(t, m, "spec", "template", "metadata").(map[string]any)
		tm["labels"] = map[string]any{"app": dig(t, tm, "labels", "app")}
		delete(tm, "annotations")
	}
	return m
}

func stripComments(s string) string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		out = append(out, strings.TrimRight(l, " "))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func hookAnnotations(o Object) map[string]any {
	meta, _ := o["metadata"].(map[string]any)
	a, _ := meta["annotations"].(map[string]any)
	return a
}

func TestTrialSecretValues(t *testing.T) {
	objs := Render(t, "unified-cd")
	sec := Find(objs, "Secret", "unified-cd-controller")
	require.NotNil(t, sec)
	assert.Equal(t, "unified-cd", sec.Namespace())
	assert.Equal(t, map[string]any{
		"UNIFIED_DB_DSN":      "postgres://unified:unified@postgres.unified-cd.svc.cluster.local:5432/unified?sslmode=disable",
		"UNIFIED_TOKEN":       "dev-token-change-me",
		"UNIFIED_S3_ENDPOINT": "garage.unified-cd.svc.cluster.local:3900",
		"UNIFIED_S3_BUCKET":   "unified-cd-logs",
		"UNIFIED_S3_KEY":      "garageadmin",
		"UNIFIED_S3_SECRET":   "garageadmin12345",
	}, dig(t, map[string]any(sec), "stringData"))

	prov, _ := dig(t, map[string]any(sec), "metadata", "annotations", "unified-cd.io/kek-provenance").(string)
	assert.Contains(t, prov, "generated once, at install")
	assert.Contains(t, prov, "A Postgres dump alone is not")

	garage := chartObject(t, objs, "Deployment", "garage")
	env := dig(t, garage, "spec", "template", "spec", "containers", 0, "env").([]any)
	var rpc any
	for _, e := range env {
		if e.(map[string]any)["name"] == "GARAGE_RPC_SECRET" {
			rpc = e.(map[string]any)["value"]
		}
	}
	assert.Equal(t, strings.Repeat("0", 64), rpc)
}

func TestTrialFollowsNamespace(t *testing.T) {
	objs := Render(t, "cd-system")
	sec := map[string]any(Find(objs, "Secret", "unified-cd-controller"))
	require.NotNil(t, sec)
	assert.Equal(t, "postgres://unified:unified@postgres.cd-system.svc.cluster.local:5432/unified?sslmode=disable",
		dig(t, sec, "stringData", "UNIFIED_DB_DSN"))
	assert.Equal(t, "garage.cd-system.svc.cluster.local:3900", dig(t, sec, "stringData", "UNIFIED_S3_ENDPOINT"))
	for _, o := range objs {
		assert.NotEqualf(t, "unified-cd", o.Namespace(), "%s/%s", o.Kind(), o.Name())
	}
}

func TestStoresMatchBase(t *testing.T) {
	objs := Render(t, "unified-cd")
	// chartObject normalises the rendered objects in place, so read the
	// checksum first.
	assert.NotEmpty(t, podChecksum(t, objs, "garage"))
	for _, c := range []struct{ kind, name, base string }{
		{"Deployment", "postgres", "postgres/deployment.yaml"},
		{"Service", "postgres", "postgres/service.yaml"},
		{"PersistentVolumeClaim", "postgres-data", "postgres/pvc.yaml"},
		{"Deployment", "garage", "garage/deployment.yaml"},
		{"Service", "garage", "garage/service.yaml"},
		{"PersistentVolumeClaim", "garage-data", "garage/pvc.yaml"},
	} {
		t.Run(c.kind+"/"+c.name, func(t *testing.T) {
			assert.Equal(t, baseObject(t, c.base), chartObject(t, objs, c.kind, c.name))
		})
	}

	// The garage ConfigMap has a fixed name here (Kustomize hashes it) and
	// carries the same configuration as the base garage.toml.
	cm := map[string]any(Find(objs, "ConfigMap", "garage-config"))
	require.NotNil(t, cm)
	got, _ := dig(t, cm, "data", "garage.toml").(string)
	raw, err := os.ReadFile(baseManifestPath("garage", "garage.toml"))
	require.NoError(t, err)
	assert.Equal(t, stripComments(string(raw)), stripComments(got))
	assert.Equal(t, "garage-config", dig(t, chartObject(t, objs, "Deployment", "garage"),
		"spec", "template", "spec", "volumes", 1, "configMap", "name"))
}

func TestStoresConfigurable(t *testing.T) {
	// storageClassName is only rendered when set.
	objs := Render(t, "unified-cd")
	for _, n := range []string{"postgres-data", "garage-data"} {
		spec := dig(t, map[string]any(Find(objs, "PersistentVolumeClaim", n)), "spec").(map[string]any)
		assert.NotContains(t, spec, "storageClassName", n)
	}

	objs = Render(t, "unified-cd",
		"bundledStores.postgres.storageClassName=fast", "bundledStores.postgres.storage=8Gi",
		"bundledStores.garage.storageClassName=bulk", "bundledStores.garage.storage=20Gi")
	pg := map[string]any(Find(objs, "PersistentVolumeClaim", "postgres-data"))
	assert.Equal(t, "fast", dig(t, pg, "spec", "storageClassName"))
	assert.Equal(t, "8Gi", dig(t, pg, "spec", "resources", "requests", "storage"))
	g := map[string]any(Find(objs, "PersistentVolumeClaim", "garage-data"))
	assert.Equal(t, "bulk", dig(t, g, "spec", "storageClassName"))
	assert.Equal(t, "20Gi", dig(t, g, "spec", "resources", "requests", "storage"))
}

func TestNoTrialSecretWithExistingSecret(t *testing.T) {
	objs := Render(t, "unified-cd", "controller.existingSecret=mine")
	assert.Nil(t, Find(objs, "Secret", "unified-cd-controller"))
	assert.Nil(t, Find(objs, "Secret", "mine"))
	dep := map[string]any(Find(objs, "Deployment", "unified-cd-controller"))
	require.NotNil(t, dep)
	assert.Equal(t, "mine", dig(t, dep, "spec", "template", "spec", "containers", 0, "envFrom", 0, "secretRef", "name"))
}

func TestProductionModeRendersNoSecretsOrStores(t *testing.T) {
	objs := Render(t, "unified-cd", "bundledStores.enabled=false", "agent.server=https://cd.example.com")
	for _, o := range objs {
		assert.NotEqualf(t, "Secret", o.Kind(), "%s", o.Name())
		assert.NotContainsf(t, []string{"postgres", "postgres-data", "garage", "garage-data", "garage-config", "unified-cd-kek-init"}, o.Name(), "%s", o.Kind())
	}
	assert.NotContains(t, agentConfig(t, objs), "sidecarS3SecretMode")
	assert.NotContains(t, dig(t, controllerConfig(t, objs), "agentAuth"), "kubernetesStoreCredentialPolicies")
}

func TestKEKJobHook(t *testing.T) {
	objs := Render(t, "cd-system")
	names := []struct{ kind, weight string }{
		{"ServiceAccount", "-1"}, {"Role", "-1"}, {"RoleBinding", "-1"}, {"Job", "0"},
	}
	for _, n := range names {
		o := Find(objs, n.kind, "unified-cd-kek-init")
		require.NotNilf(t, o, n.kind)
		assert.Equal(t, "cd-system", o.Namespace(), n.kind)
		a := hookAnnotations(o)
		assert.Equal(t, "post-install,post-upgrade", a["helm.sh/hook"], n.kind)
		assert.Equal(t, "before-hook-creation,hook-succeeded", a["helm.sh/hook-delete-policy"], n.kind)
		assert.Equal(t, n.weight, a["helm.sh/hook-weight"], n.kind)
	}

	// The hook Job runs under the ServiceAccount the Role is bound to, all in
	// the release namespace, so `kubectl create secret` there is authorised.
	role := map[string]any(Find(objs, "Role", "unified-cd-kek-init"))
	assert.Equal(t, []any{map[string]any{
		"apiGroups": []any{""}, "resources": []any{"secrets"}, "verbs": []any{"create", "get"},
	}}, role["rules"])
	rb := map[string]any(Find(objs, "RoleBinding", "unified-cd-kek-init"))
	assert.Equal(t, map[string]any{"kind": "ServiceAccount", "name": "unified-cd-kek-init", "namespace": "cd-system"},
		dig(t, rb, "subjects", 0))
	assert.Equal(t, "unified-cd-kek-init", dig(t, rb, "roleRef", "name"))
	job := map[string]any(Find(objs, "Job", "unified-cd-kek-init"))
	assert.Equal(t, "unified-cd-kek-init", dig(t, job, "spec", "template", "spec", "serviceAccountName"))
	assert.Equal(t, "alpine/k8s:1.31.2", dig(t, job, "spec", "template", "spec", "containers", 0, "image"))

	script := dig(t, job, "spec", "template", "spec", "containers", 0, "command", 2).(string)
	assert.Contains(t, script, "kubectl -n cd-system create secret generic unified-cd-controller-kek")
	assert.Contains(t, script, "kubectl -n cd-system get secret unified-cd-controller-kek")
	assert.NotContains(t, script, "-n unified-cd ")
	assert.NotContains(t, script, "apply")

	objs = Render(t, "cd-system", "controller.kek.secretName=my-kek")
	job = map[string]any(Find(objs, "Job", "unified-cd-kek-init"))
	script = dig(t, job, "spec", "template", "spec", "containers", 0, "command", 2).(string)
	assert.Contains(t, script, "create secret generic my-kek")
	assert.NotContains(t, script, "unified-cd-controller-kek")

	objs = Render(t, "cd-system", "controller.kek.job.helmHook=false")
	for _, n := range names {
		o := Find(objs, n.kind, "unified-cd-kek-init")
		require.NotNilf(t, o, n.kind)
		assert.Empty(t, hookAnnotations(o), n.kind)
	}
}

func TestKEKGenerateOverride(t *testing.T) {
	objs := Render(t, "unified-cd", "bundledStores.enabled=false", "controller.kek.generate=true",
		"agent.server=https://cd.example.com")
	assert.NotNil(t, Find(objs, "Job", "unified-cd-kek-init"))

	objs = Render(t, "unified-cd", "controller.kek.generate=false")
	assert.Nil(t, Find(objs, "Job", "unified-cd-kek-init"))
	assert.Nil(t, Find(objs, "Role", "unified-cd-kek-init"))

	// Without the controller there is nothing to generate a key for.
	objs = Render(t, "unified-cd", "controller.enabled=false", "bundledStores.enabled=false",
		"controller.kek.generate=true", "agent.server=https://cd.example.com")
	assert.Nil(t, Find(objs, "Job", "unified-cd-kek-init"))
}
