package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
)

func TestFormatLogLine(t *testing.T) {
	ts := time.Date(2026, 7, 14, 10, 24, 1, 0, time.Local)
	l := api.LogLine{StepIndex: 1, Line: "building", Timestamp: ts}
	names := map[int]string{1: "build"}

	assert.Equal(t, "building", formatLogLine(l, false, false, names))
	assert.Equal(t, "10:24:01 building", formatLogLine(l, true, false, names))
	assert.Equal(t, "[build] building", formatLogLine(l, false, true, names))
	assert.Equal(t, "10:24:01 [build] building", formatLogLine(l, true, true, names))
}

func TestStepLabel(t *testing.T) {
	names := map[int]string{2: "deploy"}
	assert.Equal(t, "System", stepLabel(-1, names))
	assert.Equal(t, "deploy", stepLabel(2, names))
	assert.Equal(t, "step 9", stepLabel(9, names)) // unknown index falls back
	assert.Equal(t, "step 3", stepLabel(3, nil))   // nil map falls back
}

func TestFetchStepNames_BestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"index":0,"name":"build"},{"index":1,"name":"test"}]`)
	}))
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}
	names := fetchStepNames(t.Context(), cfg, srv.Client(), "r1")
	assert.Equal(t, "build", names[0])
	assert.Equal(t, "test", names[1])

	// A server error yields a nil map (best-effort), not a panic.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	assert.Nil(t, fetchStepNames(t.Context(), Config{Server: bad.URL}, bad.Client(), "r1"))
}

// TestLogsCmd_TimestampsAndStep drives `logs --timestamps --step` end to end.
func TestLogsCmd_TimestampsAndStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/steps"):
			fmt.Fprint(w, `[{"index":0,"name":"build"}]`)
		case strings.Contains(r.URL.Path, "/logs"):
			fmt.Fprint(w, `[{"seq":1,"stepIndex":0,"line":"hello","timestamp":"2026-07-14T10:24:01Z"}]`)
		default: // GET /runs/{id}
			fmt.Fprint(w, `{"id":"r1","status":"Succeeded"}`)
		}
	}))
	defer srv.Close()
	cfg := Config{Server: srv.URL, Token: "tok"}

	// newLogsCmd uses http.DefaultClient (no injected client), so point it at
	// the test server via cfg and rely on the default client reaching it.
	cmd := newLogsCmd(func() (Config, error) { return cfg, nil })
	cmd.SetArgs([]string{"r1", "--timestamps", "--step"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("logs: %v", err)
	}
	got := out.String()
	assert.Contains(t, got, "[build] hello")
	// The timestamp prefix is present (local-formatted; assert the "hello" is
	// prefixed by a HH:MM:SS-looking token and the step tag).
	assert.Regexp(t, `\d\d:\d\d:\d\d \[build\] hello`, strings.TrimSpace(got))
}
