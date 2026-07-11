package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestK8sAgentCapabilities(t *testing.T) {
	assert.ElementsMatch(t, []string{"pod", "container"}, k8sAgentCapabilities())
}
