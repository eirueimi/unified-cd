package runtime

import (
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
		"-e", "FOO=bar",
		"-e", "BAZ=qux",
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
