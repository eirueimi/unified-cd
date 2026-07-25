package controller

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateAgentID(t *testing.T) {
	re := regexp.MustCompile(`^agent-[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := generateAgentID()
		assert.Regexp(t, re, id, "generated agent ID must be agent-<8 hex>")
		assert.False(t, seen[id], "generated agent IDs should not repeat: %s", id)
		seen[id] = true
	}
}
