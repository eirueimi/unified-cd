package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// A heartbeat can recreate this agent's row without its capabilities, and the
// claim poll is what restores them, so every poll must carry the same set the
// agent registered with.
func TestRun_EveryClaimPollCarriesTheRegisteredCapabilities(t *testing.T) {
	srv := newVolumeLoopServer(t, func(n int, _ <-chan string) *api.ClaimResponse {
		return &api.ClaimResponse{}
	})
	a, _ := volumeAgent(t, srv, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	require.Eventually(t, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.claimBody) >= 2
	}, 20*time.Second, 20*time.Millisecond)
	cancel()
	<-done

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, srv.registered.Capabilities, dsl.CapNative)
	assert.NotContains(t, srv.registered.Capabilities, dsl.CapPod, "a host agent never advertises pod")
	for i, c := range srv.claimBody {
		assert.ElementsMatch(t, srv.registered.Capabilities, c.Capabilities, "claim poll %d", i)
	}
}
