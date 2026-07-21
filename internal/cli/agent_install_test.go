package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

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

func TestAgentGet_PrintsDetails(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			a := api.AgentInfo{
				ID:       "agent-1",
				Hostname: "host-1",
				OS:       "linux",
				Labels:   []string{"kind:linux", "pool:default"},
				Version:  "1.2.3",
				Env:      map[string]string{"REGION": "us-east"},
			}
			b, _ := json.Marshal(a)
			return http.StatusOK, b
		},
	}
	cmd, out := newTestAgentCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"get", "agent-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/agents/agent-1" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	s := out.String()
	if !strings.Contains(s, "agent-1") || !strings.Contains(s, "host-1") || !strings.Contains(s, "kind:linux,pool:default") || !strings.Contains(s, "REGION=us-east") {
		t.Errorf("unexpected output: %s", s)
	}
}

func TestAgentGet_RequiresAgentID(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("{}") }}
	cmd, _ := newTestAgentCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"get"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when agent-id is omitted")
	}
}

func TestAgentGet_NotFoundPropagatesMessage(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusNotFound, []byte("agent not found") },
	}
	cmd, _ := newTestAgentCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"get", "missing"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "agent not found") {
		t.Fatalf("expected not-found error, got: %v", err)
	}
}

func TestAgentRuns_PrintsTabSeparatedRows(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			runs := []api.Run{
				{ID: "run-1", JobName: "build", Status: api.RunSucceeded, CreatedAt: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC), TriggeredBy: "manual"},
			}
			b, _ := json.Marshal(runs)
			return http.StatusOK, b
		},
	}
	cmd, out := newTestAgentCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"runs", "agent-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/agents/agent-1/runs" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	if !strings.Contains(out.String(), "run-1") || !strings.Contains(out.String(), "build") || !strings.Contains(out.String(), "Succeeded") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestAgentRuns_EmptyShowsMessage(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("[]") }}
	cmd, out := newTestAgentCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"runs", "agent-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "(no runs)") {
		t.Errorf("expected empty message, got: %s", out.String())
	}
}

func TestAgentRuns_RequiresAgentID(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("[]") }}
	cmd, _ := newTestAgentCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"runs"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when agent-id is omitted")
	}
}
