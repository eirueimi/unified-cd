package helmchart

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
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

func TestIngressWithoutHostsFails(t *testing.T) {
	if !Available() {
		t.Skip("helm not on PATH")
	}
	_, stderr, err := RenderErr("unified-cd", "bundledStores.enabled=false", "controller.existingSecret=prod", "ingress.enabled=true")
	require.Error(t, err)
	assert.Contains(t, stderr, "ingress.hosts")
}

func TestSchemaRejectsBadTypes(t *testing.T) {
	if !Available() {
		t.Skip("helm not on PATH")
	}
	for set, fragment := range map[string]string{
		"controller.replicas=two":                    "replicas",
		"jobNamespace.podSecurity.enforce=superuser": "enforce",
	} {
		_, stderr, err := RenderErr("unified-cd", set)
		require.Errorf(t, err, "--set %s should fail", set)
		assert.Containsf(t, stderr, fragment, "--set %s", set)
	}
}

// install renders NOTES.txt, which `helm template` never prints, without a
// cluster: it copies the chart, turns NOTES.txt into a named template and
// renders it inside a ConfigMap block scalar (--show-only picks that file).
func install(t *testing.T, ns string, set ...string) string {
	t.Helper()
	if !Available() {
		t.Skip("helm not on PATH")
	}
	dir := filepath.Join(t.TempDir(), "unified-cd")
	require.NoError(t, os.CopyFS(dir, os.DirFS(ChartDir())))
	notes, err := os.ReadFile(filepath.Join(dir, "templates", "NOTES.txt"))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, "templates", "NOTES.txt")))
	tpl := "{{- define \"notes-under-test\" -}}\n" + string(notes) + "\n{{- end -}}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "templates", "_notes.tpl"), []byte(tpl), 0o644))
	holder := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notes\ndata:\n  notes: |\n{{ include \"notes-under-test\" . | indent 4 }}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "templates", "notes-under-test.yaml"), []byte(holder), 0o644))
	a := []string{"template", "unified-cd", dir, "-n", ns, "--show-only", "templates/notes-under-test.yaml"}
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

	// Plain in-cluster agent HTTP outside trial mode warns; an https agent.server does not.
	assert.Contains(t, prod, "WARNING: agent.server is empty")
	assert.Contains(t, prod, "allowInsecureHTTP")
	https := install(t, "x", "bundledStores.enabled=false", "controller.existingSecret=prod", "agent.server=https://cd.example.com")
	assert.NotContains(t, https, "WARNING: agent.server is empty")
	noAgent := install(t, "x", "bundledStores.enabled=false", "controller.existingSecret=prod", "agent.enabled=false")
	assert.NotContains(t, noAgent, "WARNING: agent.server is empty")
	assert.NotContains(t, out, "WARNING: agent.server is empty")

	withIngress := install(t, "x", prodIngressSet...)
	assert.Contains(t, withIngress, "https://cd.example.com/")
}
