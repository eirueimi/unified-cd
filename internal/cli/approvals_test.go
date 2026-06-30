package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

func newTestApproveCmd(t *testing.T, tr *captureTransport, serverURL string) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cfg := Config{Server: serverURL, Token: "tok"}
	cmd := newApproveCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func newTestRejectCmd(t *testing.T, tr *captureTransport, serverURL string) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cfg := Config{Server: serverURL, Token: "tok"}
	cmd := newRejectCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestApprove_PostsToCorrectPath(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			return http.StatusNoContent, nil
		},
	}
	cmd, out := newTestApproveCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"run-123", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(tr.records) != 1 {
		t.Fatalf("expected 1 request, got %d", len(tr.records))
	}
	if tr.records[0].path != "/api/v1/runs/run-123/approvals/2" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	var req api.ApprovalDecisionRequest
	if err := json.Unmarshal(tr.records[0].body, &req); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if req.Decision != "approve" {
		t.Errorf("expected decision 'approve', got %q", req.Decision)
	}
	if !strings.Contains(out.String(), "approved") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestApprove_WithComment(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			return http.StatusNoContent, nil
		},
	}
	cmd, _ := newTestApproveCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"run-abc", "0", "--comment", "lgtm"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var req api.ApprovalDecisionRequest
	if err := json.Unmarshal(tr.records[0].body, &req); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if req.Comment != "lgtm" {
		t.Errorf("expected comment 'lgtm', got %q", req.Comment)
	}
}

func TestApprove_ServerErrorPropagates(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			return http.StatusConflict, []byte("already decided")
		},
	}
	cmd, _ := newTestApproveCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"run-123", "0"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already decided") {
		t.Fatalf("expected conflict error, got: %v", err)
	}
}

func TestReject_PostsToCorrectPath(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			return http.StatusNoContent, nil
		},
	}
	cmd, out := newTestRejectCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"run-456", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/runs/run-456/approvals/1" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	var req api.ApprovalDecisionRequest
	if err := json.Unmarshal(tr.records[0].body, &req); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if req.Decision != "reject" {
		t.Errorf("expected decision 'reject', got %q", req.Decision)
	}
	if !strings.Contains(out.String(), "rejected") {
		t.Errorf("unexpected output: %s", out.String())
	}
}
