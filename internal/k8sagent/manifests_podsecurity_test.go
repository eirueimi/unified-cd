package k8sagent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/helmchart"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShippedNamespacePodSecurityLabelsOnCI(t *testing.T) {
	objs := helmchart.RenderFile(t, "unified-cd", bundleValues("install"))
	ci := helmchart.Find(objs, "Namespace", "ci")
	require.NotNil(t, ci, "the install bundle must render the ci Namespace")
	labels, _ := objectField(t, ci, "metadata", "labels").(map[string]any)
	assert.Equal(t, "baseline", labels["pod-security.kubernetes.io/enforce"])
	assert.Equal(t, "restricted", labels["pod-security.kubernetes.io/audit"])
	assert.NotContains(t, labels, "pod-security.kubernetes.io/warn")
}
