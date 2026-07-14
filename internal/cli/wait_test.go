package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	// Keep polling tests fast.
	runWaitPollInterval = time.Millisecond
	runFollowPollInterval = time.Millisecond
}

// statusSeqServer returns each status in seq on successive GET /runs/{id} calls,
// staying on the last one after the sequence is exhausted.
func statusSeqServer(t *testing.T, seq []string) *httptest.Server {
	t.Helper()
	var i int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&i, 1) - 1
		idx := int(n)
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		fmt.Fprintf(w, `{"id":"r1","status":%q}`, seq[idx])
	}))
}

func TestWaitForRun_PollSucceeded(t *testing.T) {
	srv := statusSeqServer(t, []string{"Running", "Running", "Succeeded"})
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	var out, errb bytes.Buffer
	err := waitForRun(context.Background(), cfg, srv.Client(), "r1", 0, false, &out, &errb)
	assert.NoError(t, err)
}

func TestWaitForRun_PollFailed_ExitCode1(t *testing.T) {
	srv := statusSeqServer(t, []string{"Running", "Failed"})
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	err := waitForRun(context.Background(), cfg, srv.Client(), "r1", 0, false, &bytes.Buffer{}, &bytes.Buffer{})
	var ee *ExitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 1, ee.Code)
	assert.Contains(t, ee.Msg, "failed")
}

func TestWaitForRun_PollCancelled_ExitCode2(t *testing.T) {
	srv := statusSeqServer(t, []string{"Cancelled"})
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	err := waitForRun(context.Background(), cfg, srv.Client(), "r1", 0, false, &bytes.Buffer{}, &bytes.Buffer{})
	var ee *ExitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 2, ee.Code)
}

func TestWaitForRun_Timeout_ExitCode124(t *testing.T) {
	srv := statusSeqServer(t, []string{"Running"}) // never terminal
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	err := waitForRun(context.Background(), cfg, srv.Client(), "r1", 30*time.Millisecond, false, &bytes.Buffer{}, &bytes.Buffer{})
	var ee *ExitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 124, ee.Code)
	assert.Contains(t, ee.Msg, "timed out")
}

// followServer serves the two endpoints the --follow path polls: a one-shot
// batch of log lines on the first GET /logs?after=0 (empty thereafter), and a
// run status that turns terminal on the second status read.
func followServer(t *testing.T, lines string, terminalStatus string) *httptest.Server {
	t.Helper()
	var statusReads int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/logs"):
			if r.URL.Query().Get("after") == "0" {
				fmt.Fprint(w, lines)
			} else {
				fmt.Fprint(w, `[]`)
			}
		default: // GET /api/v1/runs/{id}
			if atomic.AddInt64(&statusReads, 1) >= 2 {
				fmt.Fprintf(w, `{"id":"r1","status":%q}`, terminalStatus)
			} else {
				fmt.Fprint(w, `{"id":"r1","status":"Running"}`)
			}
		}
	}))
}

func TestWaitForRun_FollowPrintsLogsAndReturnsStatus(t *testing.T) {
	lines := `[{"seq":1,"stream":"stdout","line":"building"},{"seq":2,"stream":"stderr","line":"a warning"}]`
	srv := followServer(t, lines, "Succeeded")
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	var out, errb bytes.Buffer
	err := waitForRun(context.Background(), cfg, srv.Client(), "r1", 0, true, &out, &errb)
	assert.NoError(t, err)
	assert.Contains(t, out.String(), "building")
	assert.Contains(t, errb.String(), "a warning") // stderr stream routed to errW
	assert.NotContains(t, out.String(), "a warning")
}

func TestWaitForRun_FollowFailed_ExitCode1(t *testing.T) {
	lines := `[{"seq":1,"stream":"stdout","line":"oops"}]`
	srv := followServer(t, lines, "Failed")
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	var out bytes.Buffer
	err := waitForRun(context.Background(), cfg, srv.Client(), "r1", 0, true, &out, &out)
	var ee *ExitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 1, ee.Code)
	assert.Contains(t, out.String(), "oops")
}

func TestExitErrorForStatus(t *testing.T) {
	assert.NoError(t, exitErrorForStatus("r1", "Succeeded"))
	var ee *ExitError
	require.ErrorAs(t, exitErrorForStatus("r1", "Failed"), &ee)
	assert.Equal(t, 1, ee.Code)
	require.ErrorAs(t, exitErrorForStatus("r1", "Cancelled"), &ee)
	assert.Equal(t, 2, ee.Code)
}

// TestRunWaitCmd_EndToEnd drives the `run wait` cobra command against a polling
// server and checks the returned *ExitError propagates.
func TestRunWaitCmd_EndToEnd(t *testing.T) {
	srv := statusSeqServer(t, []string{"Running", "Failed"})
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	cmd := newRunWaitCmd(func() (Config, error) { return cfg, nil }, srv.Client())
	cmd.SetArgs([]string{"r1"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	var ee *ExitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 1, ee.Code)
}

// TestRunTriggerCmd_WaitEndToEnd drives `run trigger --wait`: POST creates the
// run, then the wait polls to a terminal status.
func TestRunTriggerCmd_WaitEndToEnd(t *testing.T) {
	var terminal atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs":
			fmt.Fprint(w, `{"id":"r1","status":"Pending"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/runs/r1"):
			if terminal.Swap(true) {
				fmt.Fprint(w, `{"id":"r1","status":"Succeeded"}`)
			} else {
				fmt.Fprint(w, `{"id":"r1","status":"Running"}`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	cmd := newRunTriggerCmd(func() (Config, error) { return cfg, nil }, srv.Client())
	cmd.SetArgs([]string{"my-job", "--wait"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	assert.NoError(t, err)
	assert.Contains(t, out.String(), "r1") // run id printed before waiting
}
