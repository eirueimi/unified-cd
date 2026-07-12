package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildShellCommand_NilShellUsesShimDefault(t *testing.T) {
	cmds := buildShellCommand(nil, "echo hello")
	assert.Equal(t, []string{"/.ucd/ucd-sh", "-c", "echo hello"}, cmds,
		"a nil/empty shell must fall back to the injected shim's default interpreter argv")
}

func TestBuildShellCommand_EmptyShellUsesShimDefault(t *testing.T) {
	cmds := buildShellCommand([]string{}, "echo hello")
	assert.Equal(t, []string{"/.ucd/ucd-sh", "-c", "echo hello"}, cmds)
}

func TestBuildShellCommand_CustomShellUsedVerbatim(t *testing.T) {
	cmds := buildShellCommand([]string{"bash", "-lc"}, "echo hello")
	require.Len(t, cmds, 3)
	assert.Equal(t, []string{"bash", "-lc", "echo hello"}, cmds)
}

func TestBuildShellCommand_ArbitraryInterpreterArgv(t *testing.T) {
	cmds := buildShellCommand([]string{"python3", "-c"}, `print("hi")`)
	assert.Equal(t, []string{"python3", "-c", `print("hi")`}, cmds,
		"shell argv is exec'd verbatim, never re-parsed or quoted")
}

func TestBuildEnvShellCommand_NoEnv(t *testing.T) {
	cmds := buildEnvShellCommand(nil, "echo hello", nil)
	assert.Equal(t, []string{"/.ucd/ucd-sh", "-c", "echo hello"}, cmds,
		"no env pairs should degrade to the plain shell command")
}

func TestBuildEnvShellCommand_WithEnv_ShimDefault(t *testing.T) {
	cmds := buildEnvShellCommand(nil, "echo $FOO", []string{"FOO=bar", "BAZ=qux"})
	assert.Equal(t, []string{"env", "FOO=bar", "BAZ=qux", "/.ucd/ucd-sh", "-c", "echo $FOO"}, cmds,
		"env pairs must be passed as discrete argv elements via the env binary, never string-concatenated into the script")
}

func TestBuildEnvShellCommand_WithEnv_CustomShell(t *testing.T) {
	cmds := buildEnvShellCommand([]string{"bash", "-lc"}, "echo $FOO", []string{"FOO=bar"})
	assert.Equal(t, []string{"env", "FOO=bar", "bash", "-lc", "echo $FOO"}, cmds)
}

func TestUcdDefaultShell_ReturnsFreshSliceEachCall(t *testing.T) {
	a := ucdDefaultShell()
	b := ucdDefaultShell()
	a[0] = "mutated"
	assert.Equal(t, "/.ucd/ucd-sh", b[0], "callers must not be able to mutate a shared backing array via the returned slice")
}
