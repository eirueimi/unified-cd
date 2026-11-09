package runtime

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppleEnvValuesNeverInArgv(t *testing.T) {
	a := &appleContainer{}
	env := []string{"TOKEN=s3cr3t-value", "OTHER=x"}
	run := a.runArgs(RunSpec{Image: "i", Script: "x", Env: env})
	cr, err := a.createArgs(CreateSpec{Image: "i", Env: env})
	require.NoError(t, err)
	for _, argv := range [][]string{run, cr} {
		joined := strings.Join(argv, " ")
		assert.NotContains(t, joined, "s3cr3t-value")
		assert.NotContains(t, argv, "-e")
	}
}

func TestPrepareAppleEnv_WritesValuesAndCleansUp(t *testing.T) {
	t.Setenv("INHERITED_NAME", "from-agent")
	p, cleanup, err := prepareAppleEnv([]string{"TOKEN=s3cr3t", "MULTI=a=b", "INHERITED_NAME", "UNSET_NAME_XYZ"})
	require.NoError(t, err)
	require.Len(t, p.extraArgs, 2)
	assert.Equal(t, "--env-file", p.extraArgs[0])
	assert.Nil(t, p.childEnv)
	b, err := os.ReadFile(p.extraArgs[1])
	require.NoError(t, err)
	assert.Equal(t, "TOKEN=s3cr3t\nMULTI=a=b\nINHERITED_NAME=from-agent\n", string(b))
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p.extraArgs[1])
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
	cleanup()
	_, err = os.Stat(p.extraArgs[1])
	assert.True(t, os.IsNotExist(err))
}

func TestPrepareAppleEnv_NoEnvNoFile(t *testing.T) {
	p, cleanup, err := prepareAppleEnv(nil)
	require.NoError(t, err)
	defer cleanup()
	assert.Empty(t, p.extraArgs)
	assert.Equal(t, []string{"exec", "c"}, p.withEnv([]string{"exec", "c"}))
}

// A multi-line value (PEM key, kubeconfig) cannot go through the line-based
// env-file: it is passed as `-e NAME` and supplied via the CLI's environment.
func TestPrepareAppleEnv_MultilineValueUsesInheritedName(t *testing.T) {
	pem := "-----BEGIN KEY-----\nabc\n-----END KEY-----"
	p, cleanup, err := prepareAppleEnv([]string{"PLAIN=x", "KEY=" + pem})
	require.NoError(t, err)
	defer cleanup()
	assert.NotContains(t, strings.Join(p.extraArgs, " "), "BEGIN KEY")
	assert.Contains(t, strings.Join(p.extraArgs, " "), "-e KEY")
	assert.Contains(t, p.childEnv, "KEY="+pem)
	b, err := os.ReadFile(p.extraArgs[1])
	require.NoError(t, err)
	assert.Equal(t, "PLAIN=x\n", string(b), "the multi-line value must not be in the file")
}

func TestPrepareAppleEnv_MultilineReservedNameIsAnError(t *testing.T) {
	_, _, err := prepareAppleEnv([]string{"HOME=a\nb"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with a container CLI variable")
	assert.NotContains(t, err.Error(), "a\nb")
}

func TestAppleWithEnv_InsertsAfterSubcommand(t *testing.T) {
	got := appleEnvPrep{extraArgs: []string{"--env-file", "/tmp/f"}}.withEnv([]string{"run", "--rm", "img"})
	assert.Equal(t, []string{"run", "--env-file", "/tmp/f", "--rm", "img"}, got)
}
