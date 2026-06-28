package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildShellCommand(t *testing.T) {
	cmds := buildShellCommand("echo hello")
	require.Len(t, cmds, 3)
	assert.Equal(t, "bash", cmds[0])
	assert.Equal(t, "-lc", cmds[1])
	assert.Equal(t, "echo hello", cmds[2])
}
