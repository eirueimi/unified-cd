package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/rest"
)

// client-go's defaults (5 QPS, burst 10) are shared by every call the agent
// makes; see clientQPS.
func TestApplyClientRateLimits_RaisesTheDefaults(t *testing.T) {
	c := &rest.Config{}
	applyClientRateLimits(c)
	assert.Equal(t, float32(clientQPS), c.QPS)
	assert.Equal(t, clientBurst, c.Burst)
	assert.Greater(t, c.QPS, float32(5), "must beat client-go's default")
	assert.GreaterOrEqual(t, c.Burst, 100, "a burst of at least the default maxConcurrent")
}

func TestApplyClientRateLimits_KeepsWhatTheKubeconfigSet(t *testing.T) {
	c := &rest.Config{QPS: 7, Burst: 9}
	applyClientRateLimits(c)
	assert.Equal(t, float32(7), c.QPS)
	assert.Equal(t, 9, c.Burst)
}
