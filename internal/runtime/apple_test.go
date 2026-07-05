package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDriverFor_AppleContainer(t *testing.T) {
	r := driverFor("container")
	require.NotNil(t, r)
	assert.Equal(t, "container", r.Name())
}

func TestAppleContainer_RunArgs(t *testing.T) {
	r := &appleContainer{}
	args := r.runArgs(RunSpec{Image: "alpine", Script: "echo hi", Env: []string{"A=b"}})
	assert.Equal(t, []string{"run", "--rm", "-e", "A=b", "alpine", "sh", "-c", "echo hi"}, args)
}
