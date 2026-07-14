package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLogsFollow_StopsPromptlyOnContextCancel drives `logs -f` against a
// server that never reports a terminal run status, with a context that gets
// cancelled shortly after Execute starts. Before the fix, newLogsCmd's RunE
// created its own context.Background() internally and ignored cmd.Context(),
// so the follow loop's `case <-ctx.Done()` never fired and the command would
// only return when the (never-terminal) status changed — i.e. it would hang
// here. With the fix (ctx := cmd.Context()), cancellation is observed and the
// loop returns promptly.
func TestLogsFollow_StopsPromptlyOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/logs") {
			w.Write([]byte(`[]`))
			return
		}
		// GET /api/v1/runs/{id}: always report a non-terminal status.
		w.Write([]byte(`{"id":"run-1","status":"Running"}`))
	}))
	defer srv.Close()

	cfg := Config{Server: srv.URL, Token: "tok"}
	cmd := newLogsCmd(func() (Config, error) { return cfg, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"run-1", "-f"})
	var out strings.Builder
	cmd.SetOut(&out)

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("logs -f did not stop promptly after context cancellation")
	}
}
