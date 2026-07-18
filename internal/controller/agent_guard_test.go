package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGuardStore serves GetRun from a map and counts calls (to prove the
// claimed-by cache short-circuits repeat lookups on the hot path).
type fakeGuardStore struct {
	store.Store
	runs     map[string]*api.Run
	getCalls int
}

func (f *fakeGuardStore) GetRun(ctx context.Context, id string) (*api.Run, error) {
	f.getCalls++
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return nil, store.ErrRunNotFound
}

func guardServer(st store.Store) *Server {
	return NewServer(Config{LegacyAgentToken: "agent-secret"}, st)
}

func TestAgentRunGuard_Verdicts(t *testing.T) {
	st := &fakeGuardStore{runs: map[string]*api.Run{
		"live":      {ID: "live", Status: api.RunRunning, ClaimedBy: "a1"},
		"done":      {ID: "done", Status: api.RunSucceeded, ClaimedBy: "a1"},
		"unclaimed": {ID: "unclaimed", Status: api.RunPending},
	}}
	s := guardServer(st)
	ctx := context.Background()

	cases := []struct {
		name           string
		agent, run     string
		rejectTerminal bool
		want           runWriteVerdict
	}{
		{"owner live", "a1", "live", true, runWriteOK},
		{"owner live no-terminal-check", "a1", "live", false, runWriteOK},
		{"wrong agent", "a2", "live", true, runWriteNotOwned},
		{"missing run", "a1", "nope", true, runWriteNotFound},
		{"unclaimed run", "a1", "unclaimed", true, runWriteNotOwned},
		{"terminal rejected", "a1", "done", true, runWriteTerminal},
		{"terminal allowed when not rejecting", "a1", "done", false, runWriteOK},
	}
	for _, c := range cases {
		v, err := s.agentRunGuard(ctx, c.agent, c.run, c.rejectTerminal)
		require.NoError(t, err, c.name)
		assert.Equal(t, c.want, v, c.name)
	}
}

func TestAgentRunGuard_CachesClaimedBy(t *testing.T) {
	st := &fakeGuardStore{runs: map[string]*api.Run{
		"live": {ID: "live", Status: api.RunRunning, ClaimedBy: "a1"},
	}}
	s := guardServer(st)
	ctx := context.Background()

	// First call populates the cache; subsequent non-terminal-checking calls
	// must not hit the store again (this is the hot log path).
	_, err := s.agentRunGuard(ctx, "a1", "live", false)
	require.NoError(t, err)
	after := st.getCalls
	for i := 0; i < 5; i++ {
		v, err := s.agentRunGuard(ctx, "a1", "live", false)
		require.NoError(t, err)
		assert.Equal(t, runWriteOK, v)
	}
	assert.Equal(t, after, st.getCalls, "cached ownership must not re-query")

	// Ownership mismatch is also answerable from cache.
	v, err := s.agentRunGuard(ctx, "a2", "live", false)
	require.NoError(t, err)
	assert.Equal(t, runWriteNotOwned, v)
	assert.Equal(t, after, st.getCalls)

	// rejectTerminal always needs live status: it must re-query.
	_, err = s.agentRunGuard(ctx, "a1", "live", true)
	require.NoError(t, err)
	assert.Greater(t, st.getCalls, after)
}

func TestAgentRunGuard_NilCacheIsSafe(t *testing.T) {
	// Tests in this package build Server via bare struct literals that skip
	// NewServer, leaving claimedBy nil. The guard must still work (uncached).
	st := &fakeGuardStore{runs: map[string]*api.Run{
		"live": {ID: "live", Status: api.RunRunning, ClaimedBy: "a1"},
	}}
	s := &Server{store: st}
	ctx := context.Background()

	v, err := s.agentRunGuard(ctx, "a1", "live", false)
	require.NoError(t, err)
	assert.Equal(t, runWriteOK, v)

	v, err = s.agentRunGuard(ctx, "a2", "live", true)
	require.NoError(t, err)
	assert.Equal(t, runWriteNotOwned, v)

	// Every call must hit the store since nothing is cached.
	assert.Equal(t, 2, st.getCalls)
}

func TestClaimedByCache_EvictsPastCap(t *testing.T) {
	c := newClaimedByCache(3)
	for i := 0; i < 5; i++ {
		c.put(fmt.Sprintf("run-%d", i), "a1")
	}
	assert.Equal(t, 3, c.len())
	_, ok := c.get("run-0")
	assert.False(t, ok, "oldest entries must be evicted")
	_, ok = c.get("run-4")
	assert.True(t, ok)
}
