package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unified-cd/unified-cd/internal/api"
)

func TestGenerateSystemdUnit(t *testing.T) {
	unit := generateSystemdUnit(AgentConfig{
		Server:  "https://master.example.com",
		Token:   "secret",
		AgentID: "agent-1",
		BinPath: "/usr/local/bin/unified-cd",
		Labels:  []string{"kind:linux", "pool:default"},
	})
	assert.Contains(t, unit, "ExecStart=/usr/local/bin/unified-cd")
	assert.Contains(t, unit, "--server=https://master.example.com")
	assert.Contains(t, unit, "--id=agent-1")
	assert.Contains(t, unit, "kind:linux,pool:default")
}

func TestGenerateLaunchdPlist(t *testing.T) {
	plist := generateLaunchdPlist(AgentConfig{
		Server:  "https://master.example.com",
		Token:   "secret",
		AgentID: "agent-1",
		BinPath: "/usr/local/bin/unified-cd",
		Labels:  []string{"kind:mac"},
	})
	assert.Contains(t, plist, "dev.unified-cd.agent")
	assert.Contains(t, plist, "--server=https://master.example.com")
	assert.Contains(t, plist, "--id=agent-1")
	assert.Contains(t, plist, "kind:mac")
}

func TestWriteAgentConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := AgentConfig{
		Server:  "https://master.example.com",
		Token:   "token123",
		AgentID: "agent-1",
		Labels:  []string{"kind:linux"},
	}
	path := filepath.Join(dir, "agent.yaml")
	require.NoError(t, writeAgentConfig(path, cfg))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "https://master.example.com")
	assert.Contains(t, string(data), "token123")
	assert.Contains(t, string(data), "agent-1")
}

func TestNewAgentInstallCmd_FlagsExist(t *testing.T) {
	cmd := newAgentInstallCmd()
	assert.NotNil(t, cmd.Flags().Lookup("server"))
	assert.NotNil(t, cmd.Flags().Lookup("token"))
	assert.NotNil(t, cmd.Flags().Lookup("id"))
	assert.NotNil(t, cmd.Flags().Lookup("label"))
}

func newTestAgentCmd(t *testing.T, tr *captureTransport, serverURL string) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cfg := Config{Server: serverURL, Token: "tok"}
	cmd := newAgentCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestAgentList_PrintsTabSeparatedRows(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			agents := []api.AgentInfo{
				{ID: "agent-1", Hostname: "host-1", OS: "linux", Labels: []string{"kind:linux", "pool:default"}},
			}
			b, _ := json.Marshal(agents)
			return http.StatusOK, b
		},
	}
	cmd, out := newTestAgentCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "agent-1") || !strings.Contains(out.String(), "kind:linux,pool:default") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestAgentList_EmptyShowsMessage(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("[]") }}
	cmd, out := newTestAgentCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "(no agents)") {
		t.Errorf("expected empty message, got: %s", out.String())
	}
}
