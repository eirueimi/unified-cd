package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOCICLI_RunArgs_Resources(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args := r.runArgs(RunSpec{
		Image:    "golang:1.22",
		Script:   "go build",
		CPULimit: "0.5",
		MemLimit: "536870912",
	})
	assert.Equal(t, []string{
		"run", "--rm",
		"--cpus", "0.5",
		"--memory", "536870912",
		"golang:1.22",
		"sh", "-c", "go build",
	}, args)
}

func TestOCICLI_RunArgs_NoResources(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args := r.runArgs(RunSpec{Image: "alpine", Script: "true"})
	assert.Equal(t, []string{"run", "--rm", "alpine", "sh", "-c", "true"}, args)
}
