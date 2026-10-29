package k8sagent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShippedNamespacePodSecurityLabelsOnCI(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "manifests", "base", "namespace", "namespace.yaml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")

	var ci string
	for _, doc := range strings.Split(text, "\n---\n") {
		if strings.Contains(doc, "name: ci") {
			ci = doc
			break
		}
	}
	require.NotEmpty(t, ci, "namespace.yaml must contain the ci Namespace document")
	require.Contains(t, ci, "pod-security.kubernetes.io/enforce: baseline")
	require.Contains(t, ci, "pod-security.kubernetes.io/audit: restricted")
	require.NotContains(t, ci, "pod-security.kubernetes.io/warn")
}
