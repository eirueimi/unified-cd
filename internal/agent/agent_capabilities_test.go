package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAgentCapabilities_NativeAlways(t *testing.T) {
	// runtimeAvailable == false -> only native
	assert.Equal(t, []string{"native"}, agentCapabilities(false))
	// runtimeAvailable == true -> native + container
	assert.ElementsMatch(t, []string{"native", "container"}, agentCapabilities(true))
}
