package cli

import (
	"context"
	"errors"
	"io"
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

// cancelOnRoundTrip is a fake http.RoundTripper that cancels the caller's
// context as it "handles" a request and then completes the request normally
// with a canned status/body. This deterministically reproduces "the context
// was cancelled at the exact moment this request's real outcome (a non-200
// response) became known" without relying on a timing race — the whole point
// being that this outcome must NOT be swallowed as a clean cancellation.
type cancelOnRoundTrip struct {
	cancel context.CancelFunc
	status int
	body   string
}

func (c *cancelOnRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	c.cancel()
	return &http.Response{
		StatusCode: c.status,
		Status:     http.StatusText(c.status),
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Header:     make(http.Header),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Request:    req,
	}, nil
}

// TestLogsFollow_ContextCancelledDoesNotSwallowRealError is the regression
// test for the ctx.Err()-based discriminator bug: `ctx.Err() != nil` asks "is
// the context done *right now*", not "was this error caused by
// cancellation", so it swallowed any error that merely coincided with a
// cancelled context. Here the context is cancelled as a side effect of the
// HTTP round trip completing with a genuine, unrelated failure (a 500
// response) — with the bug, `logs` would exit 0 as if the user had cleanly
// interrupted it (e.g. Ctrl-C landing while the server was returning a
// non-200); with the fix, that failure must still be reported.
func TestLogsFollow_ContextCancelledDoesNotSwallowRealError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orig := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = orig })
	http.DefaultClient.Transport = &cancelOnRoundTrip{cancel: cancel, status: http.StatusInternalServerError, body: "boom"}

	cfg := Config{Server: "http://example.invalid", Token: "tok"}
	cmd := newLogsCmd(func() (Config, error) { return cfg, nil })
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"run-1"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected the real server error to propagate, got nil (swallowed by cancellation check)")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("expected the real server error, not the context-cancellation error: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the underlying server error to surface, got: %v", err)
	}
}
