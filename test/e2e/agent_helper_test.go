package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/agent"
	"github.com/stretchr/testify/require"
)

// newTestAgent builds a host agent that owns a private workspace directory.
//
// An agent's WorkspaceDir defaults to the process's working directory, and two
// agents sharing one workspace base share working<slot>/<job> too. Each run
// wipes and recreates that directory when it starts and again when it ends, so
// one agent's cleanup deletes the directory another agent's native step is
// using as its cwd. The step then fails with
// "fork/exec /usr/bin/bash: no such file or directory" (os/exec reports a
// failed chdir that way when SysProcAttr is set). The agent docs require a
// separate workspaceDir per agent; every e2e agent must therefore be built here.
func newTestAgent(t *testing.T, id string, c *agent.Client) *agent.Agent {
	t.Helper()
	return withPrivateWorkspace(t, agent.New(id, c))
}

// newTestAgentWithLabels is newTestAgent for an agent that advertises labels.
func newTestAgentWithLabels(t *testing.T, id string, labels []string, c *agent.Client) *agent.Agent {
	t.Helper()
	return withPrivateWorkspace(t, agent.NewWithLabels(id, labels, c))
}

func withPrivateWorkspace(t *testing.T, a *agent.Agent) *agent.Agent {
	t.Helper()
	a.WorkspaceDir = t.TempDir()
	return a
}

func TestNewTestAgent_PrivateWorkspaces(t *testing.T) {
	a := newTestAgent(t, "a", nil)
	b := newTestAgentWithLabels(t, "b", []string{"kind:test"}, nil)
	require.NotEmpty(t, a.WorkspaceDir)
	require.NotEmpty(t, b.WorkspaceDir)
	require.NotEqual(t, a.WorkspaceDir, b.WorkspaceDir)
}

// TestE2EAgentsUseHelper fails if a test builds an agent directly, which would
// leave it on the shared default workspace (the process working directory).
func TestE2EAgentsUseHelper(t *testing.T) {
	direct := regexp.MustCompile(`\bagent\.New(WithLabels)?\(`)
	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	for _, f := range files {
		if f == "agent_helper_test.go" {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		for i, line := range strings.Split(string(src), "\n") {
			if direct.MatchString(line) {
				t.Errorf("%s:%d builds an agent with agent.New directly; use newTestAgent/newTestAgentWithLabels so it gets a private workspace", f, i+1)
			}
		}
	}
}
