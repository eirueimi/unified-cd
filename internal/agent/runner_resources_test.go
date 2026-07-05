package agent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
)

func TestHostContainerLimits(t *testing.T) {
	// nil / no-limits → empty
	c, m := hostContainerLimits(nil)
	assert.Equal(t, "", c)
	assert.Equal(t, "", m)

	// limits only; requests ignored on host
	rs := &dsl.ResourceSpec{
		Requests: &dsl.ResourceList{CPU: "250m", Memory: "128Mi"},
		Limits:   &dsl.ResourceList{CPU: "500m", Memory: "512Mi"},
	}
	c, m = hostContainerLimits(rs)
	assert.Equal(t, "0.5", c)       // 500m -> 0.5 cores
	assert.Equal(t, "536870912", m) // 512Mi -> bytes
}
