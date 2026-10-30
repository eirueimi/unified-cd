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

// chartObject returns the rendered object without its namespace, standard
// labels and checksum annotation, so that only the store content itself is
// compared.
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

var storeObjects = []struct {
	kind, name, want string
}{
	{"Deployment", "postgres", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  labels:
    app: postgres
spec:
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
        - name: postgres
          image: postgres:16-alpine
          env:
            - name: POSTGRES_USER
              value: unified
            - name: POSTGRES_PASSWORD
              value: unified
            - name: POSTGRES_DB
              value: unified
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              memory: 1Gi
          ports:
            - containerPort: 5432
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", "unified"]
            periodSeconds: 5
          livenessProbe:
            exec:
              command: ["pg_isready", "-U", "unified"]
            periodSeconds: 10
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: postgres-data
`},
	{"Service", "postgres", `
apiVersion: v1
kind: Service
metadata:
  name: postgres
spec:
  selector:
    app: postgres
  ports:
    - port: 5432
      targetPort: 5432
`},
	{"PersistentVolumeClaim", "postgres-data", `
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: postgres-data
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 5Gi
`},
	{"Deployment", "garage", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: garage
  labels:
    app: garage
spec:
  replicas: 1
  selector:
    matchLabels:
      app: garage
  template:
    metadata:
      labels:
        app: garage
    spec:
      containers:
        - name: garage
          image: dxflrs/garage:v2.3.0
          command: ["/garage", "server", "--single-node", "--default-bucket"]
          env:
            - name: GARAGE_RPC_SECRET
              value: "0000000000000000000000000000000000000000000000000000000000000000"
            - name: GARAGE_DEFAULT_ACCESS_KEY
              value: garageadmin
            - name: GARAGE_DEFAULT_SECRET_KEY
              value: garageadmin12345
            - name: GARAGE_DEFAULT_BUCKET
              value: unified-cd-logs
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
            limits:
              memory: 512Mi
          ports:
            - containerPort: 3900
              name: api
            - containerPort: 3901
              name: rpc
            - containerPort: 3903
              name: admin
          volumeMounts:
            - name: data
              mountPath: /data
            - name: config
              mountPath: /etc/garage.toml
              subPath: garage.toml
          readinessProbe:
            exec:
              command: ["/garage", "status"]
            periodSeconds: 5
          livenessProbe:
            exec:
              command: ["/garage", "status"]
            periodSeconds: 10
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: garage-data
        - name: config
          configMap:
            name: garage-config
`},
	{"Service", "garage", `
apiVersion: v1
kind: Service
metadata:
  name: garage
spec:
  selector:
    app: garage
  ports:
    - name: api
      port: 3900
      targetPort: 3900
`},
	{"PersistentVolumeClaim", "garage-data", `
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: garage-data
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 10Gi
`},
}

func TestStoresRenderExpectedObjects(t *testing.T) {
	objs := Render(t, "unified-cd")
	// chartObject normalises the rendered objects in place, so read the
	// checksum first.
	assert.NotEmpty(t, podChecksum(t, objs, "garage"))
	for _, c := range storeObjects {
		t.Run(c.kind+"/"+c.name, func(t *testing.T) {
			var want map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(c.want), &want))
			assert.Equal(t, want, chartObject(t, objs, c.kind, c.name))
		})
	}

	// The garage ConfigMap carries the same configuration as the Compose
	// stack's garage.toml, and the Deployment mounts it under that fixed name.
	cm := map[string]any(Find(objs, "ConfigMap", "garage-config"))
	require.NotNil(t, cm)
	got, _ := dig(t, cm, "data", "garage.toml").(string)
	raw, err := os.ReadFile(filepath.Join(ChartDir(), "..", "..", "deployments", "garage", "garage.toml"))
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
		assert.Equal(t, "pre-install,pre-upgrade", a["helm.sh/hook"], n.kind)
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
