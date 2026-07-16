package agent

import (
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRunSet_AddRemoveSnapshot verifies the basic add/remove/snapshot
// contract: Snapshot reflects exactly the currently-added IDs, and never
// returns nil (an agent with zero active runs must still be able to send a
// non-nil empty slice in its heartbeat body — see client_test.go).
func TestRunSet_AddRemoveSnapshot(t *testing.T) {
	s := NewRunSet()

	empty := s.Snapshot()
	assert.NotNil(t, empty)
	assert.Empty(t, empty)

	s.Add("r1")
	s.Add("r2")
	got := s.Snapshot()
	sort.Strings(got)
	assert.Equal(t, []string{"r1", "r2"}, got)

	s.Remove("r1")
	got = s.Snapshot()
	assert.Equal(t, []string{"r2"}, got)

	s.Remove("r2")
	got = s.Snapshot()
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

// TestRunSet_ConcurrentAccess exercises Add/Remove/Snapshot from many
// goroutines at once under -race to prove the set is safe for the
// concurrent slot goroutines that will drive it in agent.go.
func TestRunSet_ConcurrentAccess(t *testing.T) {
	s := NewRunSet()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "r"
			s.Add(id)
			_ = s.Snapshot()
			s.Remove(id)
		}(i)
	}
	wg.Wait()
	assert.Empty(t, s.Snapshot())
}
