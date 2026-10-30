package runtime

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOCICLI_RunArgs_Default(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args := r.runArgs(RunSpec{
		Image:  "golang:1.22",
		Script: "go build",
		Env:    []string{"FOO=bar", "BAZ=qux"},
	})
	assert.Equal(t, []string{
		"run", "--rm",
		"-e", "FOO",
		"-e", "BAZ",
		"golang:1.22",
		"sh", "-c", "go build",
	}, args)
}

func TestOCICLI_RunArgs_CustomShell(t *testing.T) {
	r := &ociCLI{bin: "podman"}
	args := r.runArgs(RunSpec{
		Image:  "alpine",
		Script: "echo hi",
		Shell:  []string{"bash", "-lc"},
	})
	assert.Equal(t, []string{"run", "--rm", "alpine", "bash", "-lc", "echo hi"}, args)
}

func TestDetect_UnknownPreferredIsError(t *testing.T) {
	_, err := Detect("no-such-runtime-xyz")
	assert.Error(t, err)
}

func TestOCICLI_EnvValuesNeverInArgv(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	env := []string{"TOKEN=s3cr3t-value", "OTHER=v2"}
	run := r.runArgs(RunSpec{Image: "i", Script: "x", Env: env})
	cr, err := r.createArgs(CreateSpec{Image: "i", Env: env})
	assert.NoError(t, err)
	ex := r.execArgs(ContainerHandle{ID: "c1"}, ExecSpec{Script: "x", Env: env})
	for _, args := range [][]string{run, cr, ex} {
		for _, a := range args {
			assert.NotContains(t, a, "s3cr3t-value")
			assert.NotContains(t, a, "v2")
		}
		assert.Contains(t, args, "TOKEN")
	}
	assert.Equal(t, []string{"exec", "-e", "TOKEN", "-e", "OTHER", "c1", "sh", "-c", "x"}, ex)
}

func TestPrepareCLIEnv_ChildEnvCarriesValue(t *testing.T) {
	p, cleanup, err := prepareCLIEnv([]string{"TOKEN=s3cr3t"})
	defer cleanup()
	assert.NoError(t, err)
	assert.Contains(t, p.childEnv, "TOKEN=s3cr3t")
	assert.Empty(t, p.extraArgs)
	assert.Equal(t, []string{"exec", "c"}, p.withEnvFile([]string{"exec", "c"}))
}

func TestPrepareCLIEnv_CollisionUsesEnvFile(t *testing.T) {
	env := []string{"PATH=/evil", "docker_host=tcp://x", "TOKEN=s3cr3t"}
	p, cleanup, err := prepareCLIEnv(env)
	assert.NoError(t, err)
	assert.Len(t, p.extraArgs, 2)
	assert.Equal(t, "--env-file", p.extraArgs[0])
	b, rerr := os.ReadFile(p.file)
	assert.NoError(t, rerr)
	assert.Equal(t, "PATH=/evil\ndocker_host=tcp://x\n", string(b))
	// Colliding keys are not in the child env, and not in argv.
	assert.NotContains(t, p.childEnv, "PATH=/evil")
	assert.Contains(t, p.childEnv, "TOKEN=s3cr3t")
	flags := envFlags(env)
	assert.Equal(t, []string{"-e", "TOKEN"}, flags)
	full := p.withEnvFile((&ociCLI{bin: "docker"}).execArgs(ContainerHandle{ID: "c"}, ExecSpec{Script: "x", Env: env}))
	assert.Equal(t, []string{"exec", "--env-file", p.file, "-e", "TOKEN", "c", "sh", "-c", "x"}, full)
	cleanup()
	_, serr := os.Stat(p.file)
	assert.True(t, os.IsNotExist(serr))
}

func TestPrepareCLIEnv_CollisionNewlineRejected(t *testing.T) {
	_, cleanup, err := prepareCLIEnv([]string{"HOME=a\nb"})
	defer cleanup()
	assert.Error(t, err)
}
