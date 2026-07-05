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

func newTestWebhookCmd(t *testing.T, tr *captureTransport, serverURL string) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cfg := Config{Server: serverURL, Token: "tok"}
	cmd := newWebhookCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestWebhookList_PrintsRows(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			list := []api.WebhookReceiverMeta{
				{ID: "id-1", Name: "github-push", UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
				{ID: "id-2", Name: "gitlab-push", UpdatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
			}
			b, _ := json.Marshal(list)
			return http.StatusOK, b
		},
	}
	cmd, out := newTestWebhookCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tr.records[0].path != "/api/v1/webhooks/" {
		t.Errorf("unexpected path: %s", tr.records[0].path)
	}
	if !strings.Contains(out.String(), "github-push") || !strings.Contains(out.String(), "gitlab-push") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestWebhookList_EmptyShowsMessage(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusOK, []byte("[]") }}
	cmd, out := newTestWebhookCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "(no webhook receivers)") {
		t.Errorf("expected empty message, got: %s", out.String())
	}
}

func TestWebhookDelete_Success(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusNoContent, nil }}
	cmd, out := newTestWebhookCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"delete", "github-push"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(tr.records) != 1 || tr.records[0].path != "/api/v1/webhooks/github-push" {
		t.Fatalf("unexpected requests: %+v", tr.records)
	}
	if tr.records[0].method != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", tr.records[0].method)
	}
	if !strings.Contains(out.String(), "github-push") || !strings.Contains(out.String(), "deleted") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestWebhookDelete_RequiresName(t *testing.T) {
	tr := &captureTransport{responseFor: func(path string) (int, []byte) { return http.StatusNoContent, nil }}
	cmd, _ := newTestWebhookCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"delete"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when name is omitted")
	}
}

func TestWebhookDelete_ServerErrorPropagatesMessage(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) { return http.StatusInternalServerError, []byte("boom") },
	}
	cmd, _ := newTestWebhookCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"delete", "github-push"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error containing 'boom', got: %v", err)
	}
}
