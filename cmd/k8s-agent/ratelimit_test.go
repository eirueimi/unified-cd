package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/rest"
)

// The pod-start poll (2 QPS per starting run) is the dominant consumer of the
// clientset's single rate limiter; see clientRateLimits.
func TestClientRateLimits_CoverTheStartPollOfAFullFleet(t *testing.T) {
	for _, mc := range []int{1, 10, 100, 500} {
		qps, burst := clientRateLimits(mc)
		assert.GreaterOrEqual(t, qps, float32(2*mc), "maxConcurrent %d: every starting run polls twice a second", mc)
		assert.GreaterOrEqual(t, burst, int(qps), "maxConcurrent %d: a burst of at least a second's demand", mc)
	}
	qps, _ := clientRateLimits(100)
	assert.Greater(t, qps, float32(200), "the default maxConcurrent of 100 needs more than its start polls alone")
}

func TestClientRateLimits_UnlimitedConcurrencyIsSizedAsTheDefault(t *testing.T) {
	wantQPS, wantBurst := clientRateLimits(defaultMaxConcurrent)
	for _, mc := range []int{0, -1} {
		qps, burst := clientRateLimits(mc)
		assert.Equal(t, wantQPS, qps)
		assert.Equal(t, wantBurst, burst)
	}
}

func TestApplyClientRateLimits_ReplacesClientGoDefaults(t *testing.T) {
	c := &rest.Config{}
	applyClientRateLimits(c, 100)
	qps, burst := clientRateLimits(100)
	assert.Equal(t, qps, c.QPS)
	assert.Equal(t, burst, c.Burst)
	assert.Greater(t, c.QPS, float32(5), "must beat client-go's default")
}
