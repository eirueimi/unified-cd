package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckContainerMountPath(t *testing.T) {
	var refused []string
	for _, d := range ProtectedMountDirs {
		refused = append(refused, d, d+"/work", d+"/a/b")
		if i := strings.LastIndex(d, "/"); i > 0 {
			refused = append(refused, d[:i]) // an ancestor of a protected directory
		}
	}
	refused = append(refused, "/", "workspace", "./workspace", "/workspace/", "/workspace/../x", "/a//b", "")
	for _, p := range refused {
		assert.Error(t, CheckContainerMountPath(p), "%q must be refused", p)
	}
	for _, p := range []string{"/workspace", "/home/runner/work", "/data", "/var/lib/build", "/opt/ws", "/usrlocal", "/binx"} {
		assert.NoError(t, CheckContainerMountPath(p), "%q must be allowed", p)
	}
	// Caller-supplied directories are protected too.
	assert.Error(t, CheckContainerMountPath("/creds/x", "/creds"))
	assert.Error(t, CheckContainerMountPath("/opt", "/opt/creds"))
}

func yamlWithWorkspace(mp string) string {
	return "apiVersion: unified-cd/v1\nkind: Job\nmetadata: { name: j }\nspec:\n  podTemplate:\n    workspace: { mountPath: " + mp + " }\n  steps:\n    - { name: s, run: env }\n"
}

func TestParse_WorkspaceMountPathProtected(t *testing.T) {
	for _, mp := range []string{"/usr/local/bin", "/usr", "/", "/etc/x", "/.ucd", "/var/run/secrets", "relative/dir", "/workspace/"} {
		_, err := Parse(strings.NewReader(yamlWithWorkspace(`"` + mp + `"`)))
		require.Error(t, err, mp)
		assert.Contains(t, err.Error(), "spec.podTemplate.workspace: ", mp)
	}
	for _, mp := range []string{"/workspace", "/home/runner/work"} {
		_, err := Parse(strings.NewReader(yamlWithWorkspace(mp)))
		require.NoError(t, err, mp)
	}
}

// A backslash is a separator to the Kubernetes agent but not in a POSIX path,
// so a mountPath that holds one is refused at apply time, in a volume and in
// the workspace.
func TestParse_MountPathBackslashRefused(t *testing.T) {
	for _, mp := range []string{`\usr\local\bin`, `/usr\local\bin`, `usr\..\..\usr\local\bin`, `deps\cache`} {
		_, err := Parse(strings.NewReader(yamlWithWorkspace(`'` + mp + `'`)))
		require.Error(t, err, "workspace %s", mp)
		assert.Contains(t, err.Error(), "backslash", mp)

		y := "apiVersion: unified-cd/v1\nkind: Job\nmetadata: { name: j }\nspec:\n  volumes:\n    - { name: v, mountPath: '" + mp + "' }\n  steps:\n    - { name: s, run: env }\n"
		_, err = Parse(strings.NewReader(y))
		require.Error(t, err, "volume %s", mp)
		assert.Contains(t, err.Error(), "mountPath must be a POSIX path", mp)
	}
	assert.Error(t, CheckContainerMountPath(`/usr\local`))
}
