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

func TestBuildEnvShellCommand_NoEnv(t *testing.T) {
	cmds := buildEnvShellCommand("echo hello", nil)
	assert.Equal(t, []string{"bash", "-lc", "echo hello"}, cmds,
		"no env pairs should degrade to the plain shell command")
}

func TestBuildEnvShellCommand_WithEnv(t *testing.T) {
	cmds := buildEnvShellCommand("echo $FOO", []string{"FOO=bar", "BAZ=qux"})
	assert.Equal(t, []string{"env", "FOO=bar", "BAZ=qux", "bash", "-lc", "echo $FOO"}, cmds,
		"env pairs must be passed as discrete argv elements via the env binary, never string-concatenated into the script")
}
