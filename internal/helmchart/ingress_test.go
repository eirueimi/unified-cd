package helmchart

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prodIngressSet are production-mode values with an Ingress.
var prodIngressSet = []string{
	"bundledStores.enabled=false",
	"controller.existingSecret=prod",
	"ingress.enabled=true",
	"ingress.className=nginx",
	"ingress.annotations.cert-manager\\.io/cluster-issuer=letsencrypt",
	"ingress.hosts[0].host=cd.example.com",
	"ingress.hosts[0].paths[0].path=/",
	"ingress.hosts[0].paths[0].pathType=Prefix",
	"ingress.tls[0].secretName=cd-tls",
	"ingress.tls[0].hosts[0]=cd.example.com",
}

func TestIngressDisabledByDefault(t *testing.T) {
	for _, o := range Render(t, "unified-cd") {
		assert.NotEqual(t, "Ingress", o.Kind())
	}
}

func TestIngressRendersHostsAndTLS(t *testing.T) {
	objs := Render(t, "unified-cd", prodIngressSet...)
	ing := Find(objs, "Ingress", "unified-cd-controller")
	require.NotNil(t, ing)
	m := map[string]any(ing)
	assert.Equal(t, "networking.k8s.io/v1", m["apiVersion"])
	assert.Equal(t, "unified-cd", ing.Namespace())
	assert.Equal(t, "letsencrypt", dig(t, m, "metadata", "annotations", "cert-manager.io/cluster-issuer"))
	assert.Equal(t, "nginx", dig(t, m, "spec", "ingressClassName"))
	assert.Equal(t, "cd.example.com", dig(t, m, "spec", "rules", 0, "host"))
	path := dig(t, m, "spec", "rules", 0, "http", "paths", 0)
	assert.Equal(t, "/", dig(t, path, "path"))
	assert.Equal(t, "Prefix", dig(t, path, "pathType"))
	assert.Equal(t, "unified-cd-controller", dig(t, path, "backend", "service", "name"))
	assert.EqualValues(t, 80, dig(t, path, "backend", "service", "port", "number"))
	assert.Equal(t, "cd-tls", dig(t, m, "spec", "tls", 0, "secretName"))
	assert.Equal(t, "cd.example.com", dig(t, m, "spec", "tls", 0, "hosts", 0))

	for _, o := range objs {
		assert.NotEqual(t, "Secret", o.Kind(), "production mode must render no Secret (found %s)", o.Name())
	}
}

func TestIngressWithTrialFails(t *testing.T) {
	if !Available() {
		t.Skip("helm not on PATH")
	}
	_, stderr, err := RenderErr("unified-cd", "ingress.enabled=true")
	require.Error(t, err)
	assert.Contains(t, stderr, "ingress.enabled with the bundled trial credentials would expose dev-token-change-me")
}

func TestSchemaRejectsBadTypes(t *testing.T) {
	if !Available() {
		t.Skip("helm not on PATH")
	}
	for _, set := range []string{
		"controller.replicas=two",
		"jobNamespace.podSecurity.enforce=superuser",
		"agent.server=http://insecure.example.com",
	} {
		_, stderr, err := RenderErr("unified-cd", set)
		assert.Errorf(t, err, "--set %s should fail", set)
		assert.NotEmpty(t, stderr)
	}
}

type stubResource struct {
	name, kind string
	namespaced bool
}

// stubAPI lists, per group/version ("v1" is the core group), the resource kinds
// the chart renders, which is all Helm's REST mapper needs to build objects.
var stubAPI = map[string][]stubResource{
	"v1": {
		{"namespaces", "Namespace", false}, {"services", "Service", true},
		{"serviceaccounts", "ServiceAccount", true}, {"configmaps", "ConfigMap", true},
		{"secrets", "Secret", true}, {"persistentvolumeclaims", "PersistentVolumeClaim", true},
	},
	"apps/v1":                      {{"deployments", "Deployment", true}},
	"batch/v1":                     {{"jobs", "Job", true}},
	"networking.k8s.io/v1":         {{"ingresses", "Ingress", true}},
	"rbac.authorization.k8s.io/v1": {{"roles", "Role", true}, {"rolebindings", "RoleBinding", true}, {"clusterroles", "ClusterRole", false}, {"clusterrolebindings", "ClusterRoleBinding", false}},
}

// serveDiscovery answers the discovery requests `helm install --dry-run=client`
// makes, so no cluster is needed to render NOTES.txt.
func serveDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	path := strings.Trim(r.URL.Path, "/")
	switch {
	case path == "version":
		_ = enc.Encode(map[string]string{"major": "1", "minor": "31", "gitVersion": "v1.31.0"})
	case path == "api":
		_ = enc.Encode(map[string]any{"kind": "APIVersions", "versions": []string{"v1"}})
	case path == "apis":
		var groups []map[string]any
		for gv := range stubAPI {
			g, v, ok := strings.Cut(gv, "/")
			if !ok {
				continue
			}
			ver := map[string]string{"groupVersion": gv, "version": v}
			groups = append(groups, map[string]any{"name": g, "versions": []any{ver}, "preferredVersion": ver})
		}
		_ = enc.Encode(map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": groups})
	default:
		gv := strings.TrimPrefix(strings.TrimPrefix(path, "apis/"), "api/")
		res, ok := stubAPI[gv]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var list []map[string]any
		for _, rs := range res {
			list = append(list, map[string]any{"name": rs.name, "kind": rs.kind, "namespaced": rs.namespaced, "verbs": []string{"get", "list", "create", "delete"}})
		}
		_ = enc.Encode(map[string]any{"kind": "APIResourceList", "groupVersion": gv, "resources": list})
	}
}

// install runs `helm install --dry-run=client`, which renders NOTES.txt and
// installs nothing. Helm 3.16 still probes the API server's /version first, so
// the command is pointed at a local stub API server; no cluster is needed.
func install(t *testing.T, ns string, set ...string) string {
	t.Helper()
	if !Available() {
		t.Skip("helm not on PATH")
	}
	api := httptest.NewServer(http.HandlerFunc(serveDiscovery))
	t.Cleanup(api.Close)
	t.Setenv("KUBECONFIG", "")
	a := []string{"install", "--dry-run=client", "unified-cd", ChartDir(), "-n", ns, "--kube-apiserver", api.URL, "--disable-openapi-validation"}
	for _, s := range set {
		a = append(a, "--set", s)
	}
	var out, errb bytes.Buffer
	cmd := exec.Command("helm", a...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	require.NoError(t, cmd.Run(), errb.String())
	return out.String()
}

func TestNotesTrialWarning(t *testing.T) {
	out := install(t, "x")
	assert.Contains(t, out, "TRIAL ONLY")
	assert.Contains(t, out, "kubectl -n x port-forward svc/unified-cd-controller 8080:80")
	assert.Contains(t, out, "dev-token-change-me")

	prod := install(t, "x", "bundledStores.enabled=false", "controller.existingSecret=prod")
	assert.NotContains(t, prod, "TRIAL ONLY")
	assert.NotContains(t, prod, "dev-token-change-me")
	assert.Contains(t, prod, "port-forward")

	withIngress := install(t, "x", prodIngressSet...)
	assert.Contains(t, withIngress, "https://cd.example.com/")
}
