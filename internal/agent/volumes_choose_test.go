package agent

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ChooseVolumes is shared by the host agent and the Kubernetes agent, which
// differ only in what "free" means, so a job's candidate list must resolve the
// same way for any predicate.
func TestChooseVolumes(t *testing.T) {
	freeOnly := func(free ...string) func(string) bool {
		return func(n string) bool {
			for _, f := range free {
				if f == n {
					return true
				}
			}
			return false
		}
	}

	t.Run("takes the first free candidate", func(t *testing.T) {
		got, ok, _ := ChooseVolumes([][]string{{"a", "b"}}, freeOnly("b"))
		assert.True(t, ok)
		assert.Equal(t, []string{"b"}, got)
	})
	t.Run("two entries get distinct volumes", func(t *testing.T) {
		got, ok, _ := ChooseVolumes([][]string{{"a", "b"}, {"a", "b"}}, freeOnly("a", "b"))
		assert.True(t, ok)
		assert.Equal(t, []string{"a", "b"}, got)
	})
	t.Run("waits rather than alias when distinct volumes exist but are busy", func(t *testing.T) {
		_, ok, _ := ChooseVolumes([][]string{{"a", "b"}, {"a", "b"}}, freeOnly("a"))
		assert.False(t, ok)
	})
	t.Run("aliases only when distinct volumes cannot exist", func(t *testing.T) {
		got, ok, _ := ChooseVolumes([][]string{{"a"}, {"a"}}, freeOnly("a"))
		assert.True(t, ok)
		assert.Equal(t, []string{"a", "a"}, got)
	})
	t.Run("busy single candidate waits", func(t *testing.T) {
		_, ok, _ := ChooseVolumes([][]string{{"a"}}, freeOnly())
		assert.False(t, ok)
	})
}

// namesN returns n distinct volume names.
func namesN(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// More entries than distinct candidate names cannot be given distinct volumes,
// so ChooseVolumes must go straight to aliasing instead of exhausting the
// distinct search first. Work is counted (calls of the free predicate, which
// the distinct search makes at every node) rather than timed, so a loaded CI
// machine cannot make it flaky: 9 names for 10 entries cost over a million
// calls when the distinct passes ran, and the aliasing pass costs a few dozen.
func TestChooseVolumes_MoreEntriesThanCandidateNamesSkipsTheDistinctSearch(t *testing.T) {
	names := namesN("v", 9)
	declared := make([][]string, 10)
	for i := range declared {
		declared[i] = names
	}
	calls := 0
	free := func(string) bool { calls++; return true }

	got, ok, _ := ChooseVolumes(declared, free)

	require.True(t, ok)
	// The aliasing reading of the job: each entry takes the first candidate that
	// is not yet chosen, and the one left over reuses its first candidate.
	want := append(append([]string{}, names...), names[0])
	assert.Equal(t, want, got)
	assert.Less(t, calls, 1000, "the distinct search ran although no distinct assignment can exist")
}
