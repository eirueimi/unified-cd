package agentid

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidatePortable(t *testing.T) {
	for _, id := range []string{
		"agent-a",
		"agent.a_01",
		"k8s-agent-prod",
	} {
		t.Run("accept_"+id, func(t *testing.T) {
			require.NoError(t, ValidatePortable(id))
		})
	}

	for _, id := range []string{
		"",
		"Agent-A",
		"agent-é",
		"agent-e\u0301",
		"agent-a.",
		".agent-a",
		"agent/a",
		`agent\a`,
		"con",
		"nul.json",
		"com1",
		"lpt9.log",
	} {
		t.Run("reject_"+id, func(t *testing.T) {
			require.EqualError(t, ValidatePortable(id), PortableError)
		})
	}
}
