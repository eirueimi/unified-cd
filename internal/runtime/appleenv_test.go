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

func TestAppleEnvFile_WritesValuesAndCleansUp(t *testing.T) {
	t.Setenv("INHERITED_NAME", "from-agent")
	args, cleanup, err := appleEnvFile([]string{"TOKEN=s3cr3t", "MULTI=a=b", "INHERITED_NAME", "UNSET_NAME_XYZ"})
	require.NoError(t, err)
	require.Len(t, args, 2)
	assert.Equal(t, "--env-file", args[0])
	b, err := os.ReadFile(args[1])
	require.NoError(t, err)
	assert.Equal(t, "TOKEN=s3cr3t\nMULTI=a=b\nINHERITED_NAME=from-agent\n", string(b))
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(args[1])
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
	cleanup()
	_, err = os.Stat(args[1])
	assert.True(t, os.IsNotExist(err))
}

func TestAppleEnvFile_NoEnvNoFile(t *testing.T) {
	args, cleanup, err := appleEnvFile(nil)
	require.NoError(t, err)
	defer cleanup()
	assert.Empty(t, args)
	assert.Equal(t, []string{"exec", "c"}, withAppleEnvFile([]string{"exec", "c"}, args))
}

func TestAppleEnvFile_RejectsNewlineValue(t *testing.T) {
	_, _, err := appleEnvFile([]string{"K=a\nb"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "a\nb")
}

func TestAppleWithEnvFile_InsertsAfterSubcommand(t *testing.T) {
	got := withAppleEnvFile([]string{"run", "--rm", "img"}, []string{"--env-file", "/tmp/f"})
	assert.Equal(t, []string{"run", "--env-file", "/tmp/f", "--rm", "img"}, got)
}
