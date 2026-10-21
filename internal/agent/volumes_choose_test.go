package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
		got, ok := ChooseVolumes([][]string{{"a", "b"}}, freeOnly("b"))
		assert.True(t, ok)
		assert.Equal(t, []string{"b"}, got)
	})
	t.Run("two entries get distinct volumes", func(t *testing.T) {
		got, ok := ChooseVolumes([][]string{{"a", "b"}, {"a", "b"}}, freeOnly("a", "b"))
		assert.True(t, ok)
		assert.Equal(t, []string{"a", "b"}, got)
	})
	t.Run("waits rather than alias when distinct volumes exist but are busy", func(t *testing.T) {
		_, ok := ChooseVolumes([][]string{{"a", "b"}, {"a", "b"}}, freeOnly("a"))
		assert.False(t, ok)
	})
	t.Run("aliases only when distinct volumes cannot exist", func(t *testing.T) {
		got, ok := ChooseVolumes([][]string{{"a"}, {"a"}}, freeOnly("a"))
		assert.True(t, ok)
		assert.Equal(t, []string{"a", "a"}, got)
	})
	t.Run("busy single candidate waits", func(t *testing.T) {
		_, ok := ChooseVolumes([][]string{{"a"}}, freeOnly())
		assert.False(t, ok)
	})
}
