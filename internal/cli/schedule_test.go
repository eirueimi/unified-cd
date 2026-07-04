package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

func newTestScheduleCmd(t *testing.T, tr *captureTransport, serverURL string) (*cobra.Command, *strings.Builder) {
	t.Helper()
	cfg := Config{Server: serverURL, Token: "tok"}
	cmd := newScheduleCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestScheduleList(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			b, _ := json.Marshal([]api.ScheduleMeta{
				{Name: "nightly-build", Cron: "0 2 * * *", JobName: "build"},
			})
			return http.StatusOK, b
		},
	}
	cmd, out := newTestScheduleCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(tr.records) != 1 || tr.records[0].path != "/api/v1/schedules/" {
		t.Fatalf("unexpected requests: %+v", tr.records)
	}
	if !strings.Contains(out.String(), "nightly-build") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestScheduleDelete(t *testing.T) {
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			return http.StatusNoContent, nil
		},
	}
	cmd, out := newTestScheduleCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"delete", "nightly-build"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(tr.records) != 1 || tr.records[0].path != "/api/v1/schedules/nightly-build" {
		t.Fatalf("unexpected requests: %+v", tr.records)
	}
	if !strings.Contains(out.String(), "nightly-build") {
		t.Errorf("unexpected output: %s", out.String())
	}
}
