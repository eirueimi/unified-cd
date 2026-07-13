package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crtNoop is a no-op embeddable crt.ContainerRuntime: every method returns a
// zero value / nil error. Tests embed it and override only the method(s)
// they care about (see logFakeRT.Logs below).
type crtNoop struct{}

func (crtNoop) Name() string    { return "noop" }
func (crtNoop) Available() bool { return true }
func (crtNoop) Pull(ctx context.Context, image string) error { return nil }
func (crtNoop) Run(ctx context.Context, spec crt.RunSpec, stdout, stderr io.Writer) (int, error) {
	return 0, nil
}
func (crtNoop) Create(ctx context.Context, spec crt.CreateSpec) (crt.ContainerHandle, error) {
	return crt.ContainerHandle{}, nil
}
func (crtNoop) Exec(ctx context.Context, h crt.ContainerHandle, spec crt.ExecSpec, stdout, stderr io.Writer) (int, error) {
	return 0, nil
}
func (crtNoop) CopyIn(ctx context.Context, h crt.ContainerHandle, hostPath, containerPath string) error {
	return nil
}
func (crtNoop) CopyOut(ctx context.Context, h crt.ContainerHandle, containerPath, hostPath string) error {
	return nil
}
func (crtNoop) Remove(ctx context.Context, h crt.ContainerHandle) error { return nil }
func (crtNoop) Logs(ctx context.Context, h crt.ContainerHandle, stdout, stderr io.Writer) error {
	return nil
}
func (crtNoop) ExitCode(ctx context.Context, h crt.ContainerHandle) (int, error) { return 0, nil }

// logFakeRT.Logs writes one stdout line then returns (container "exited").
type logFakeRT struct{ crtNoop }

func (logFakeRT) Logs(_ context.Context, h crt.ContainerHandle, stdout, _ io.Writer) error {
	io.WriteString(stdout, "sidecar line for "+h.ID+"\n")
	return nil
}

// recordingClient wraps a *Client pointed at an httptest.Server that records
// every logs/bulk request body, keyed by the step index parsed out of the URL
// path — mirroring claim_pod_integration_test.go's claimIntegrationHarness.
// It also records every sidecar status report, in arrival order.
type recordingClient struct {
	client *Client
	srv    *httptest.Server

	mu       sync.Mutex
	lines    map[int][]string
	statuses []api.SidecarStatusRequest
}

func newRecordingClient() *recordingClient {
	rec := &recordingClient{lines: map[int][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/logs/bulk"):
			parts := strings.Split(r.URL.Path, "/")
			stepIndex := 0
			for i, p := range parts {
				if p == "steps" && i+1 < len(parts) {
					if idx, err := strconv.Atoi(parts[i+1]); err == nil {
						stepIndex = idx
					}
				}
			}
			var entries []api.LogAppendRequest
			if err := json.NewDecoder(r.Body).Decode(&entries); err == nil {
				rec.mu.Lock()
				for _, e := range entries {
					if e.Line != "" {
						rec.lines[stepIndex] = append(rec.lines[stepIndex], e.Line)
					}
				}
				rec.mu.Unlock()
			}
		case strings.HasSuffix(r.URL.Path, "/sidecars"):
			var s api.SidecarStatusRequest
			if err := json.NewDecoder(r.Body).Decode(&s); err == nil {
				rec.mu.Lock()
				rec.statuses = append(rec.statuses, s)
				rec.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	rec.srv = httptest.NewServer(mux)
	rec.client = NewClient(rec.srv.URL, "tok")
	return rec
}

func (r *recordingClient) linesForStep(idx int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines[idx]))
	copy(out, r.lines[idx])
	return out
}

func (r *recordingClient) statusReports() []api.SidecarStatusRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]api.SidecarStatusRequest, len(r.statuses))
	copy(out, r.statuses)
	return out
}

func TestSidecarLogPump_ShipsAtSidecarIndex(t *testing.T) {
	rec := newRecordingClient()
	defer rec.srv.Close()
	pump := newSidecarLogPump(logFakeRT{}, rec.client, "agent-1", "run-1", nil,
		[]SidecarHandle{{Name: "mysql", Ordinal: 0, Handle: crt.ContainerHandle{ID: "c1"}}})
	pump.Start(context.Background())
	pump.Stop(context.Background())

	lines := rec.linesForStep(dsl.SidecarLogIndex(0))
	require.NotEmpty(t, lines)
	assert.Contains(t, lines[0], "sidecar line for c1")
}

// exitCodeFakeRT is a logFakeRT variant whose ExitCode returns a
// distinguishable non-zero code, so a test can prove the reported "exited"
// status carries the runtime's real exit code.
type exitCodeFakeRT struct {
	logFakeRT
	code int
}

func (f exitCodeFakeRT) ExitCode(context.Context, crt.ContainerHandle) (int, error) {
	return f.code, nil
}

func TestSidecarLogPump_ReportsRunningThenExited(t *testing.T) {
	rec := newRecordingClient()
	defer rec.srv.Close()
	pump := newSidecarLogPump(exitCodeFakeRT{code: 7}, rec.client, "agent-1", "run-1", nil,
		[]SidecarHandle{{Name: "mysql", Ordinal: 0, Handle: crt.ContainerHandle{ID: "c1"}}})
	pump.Start(context.Background())
	pump.Stop(context.Background())

	statuses := rec.statusReports()
	require.Len(t, statuses, 2)

	assert.Equal(t, "run-1", statuses[0].RunID)
	assert.Equal(t, "mysql", statuses[0].Name)
	assert.Equal(t, dsl.SidecarLogIndex(0), statuses[0].Index)
	assert.Equal(t, "running", statuses[0].Phase)
	assert.Nil(t, statuses[0].ExitCode)

	assert.Equal(t, "exited", statuses[1].Phase)
	require.NotNil(t, statuses[1].ExitCode)
	assert.Equal(t, 7, *statuses[1].ExitCode)
}
