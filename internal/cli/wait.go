package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

// runWaitPollInterval is how often waitForRun re-checks a run's status in the
// non-follow (polling) path. runFollowPollInterval is the same for the --follow
// path (which also fetches new log lines each tick). Both are vars so tests can
// shrink them. The follow interval matches `unified-cli logs -f` so the two
// commands use one consistent follow mechanism (poll the run's logs+status).
var (
	runWaitPollInterval   = 2 * time.Second
	runFollowPollInterval = 300 * time.Millisecond
)

// ExitError carries a specific process exit code up to main(). Commands return
// it so that a distinct outcome (a failed run vs. a cancelled run vs. a wait
// timeout) maps to a distinct CLI exit code for CI/scripting.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// exitCodeForStatus maps a terminal run status to a CLI exit code:
// Succeeded → 0 (nil), Failed → 1, Cancelled → 2. Any other terminal status
// maps to 1. Timeout is handled separately (124) by waitForRun.
func exitErrorForStatus(runID, status string) error {
	switch status {
	case string(api.RunSucceeded):
		return nil
	case string(api.RunFailed):
		return &ExitError{Code: 1, Msg: fmt.Sprintf("run %s failed", runID)}
	case string(api.RunCancelled):
		return &ExitError{Code: 2, Msg: fmt.Sprintf("run %s cancelled", runID)}
	default:
		return &ExitError{Code: 1, Msg: fmt.Sprintf("run %s ended in status %s", runID, status)}
	}
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case string(api.RunSucceeded), string(api.RunFailed), string(api.RunCancelled):
		return true
	}
	return false
}

// waitForRun blocks until runID reaches a terminal status, then returns nil on
// Succeeded or an *ExitError (with a distinct code) otherwise. When follow is
// true it streams the run's step logs to outW/errW while waiting (via the
// server's SSE events endpoint); otherwise it polls the run status every
// runWaitPollInterval. A non-zero timeout bounds the wait; a timeout returns an
// *ExitError with code 124 (mirroring GNU `timeout`).
func waitForRun(ctx context.Context, cfg Config, httpClient *http.Client, runID string, timeout time.Duration, follow bool, outW, errW io.Writer) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var status string
	var err error
	if follow {
		status, err = followRun(ctx, cfg, httpClient, runID, outW, errW)
	} else {
		status, err = pollRun(ctx, cfg, httpClient, runID)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &ExitError{Code: 124, Msg: fmt.Sprintf("timed out after %s waiting for run %s", timeout, runID)}
		}
		return err
	}
	return exitErrorForStatus(runID, status)
}

// pollRun GETs the run every runWaitPollInterval until it reaches a terminal
// status, returning that status.
func pollRun(ctx context.Context, cfg Config, httpClient *http.Client, runID string) (string, error) {
	ticker := time.NewTicker(runWaitPollInterval)
	defer ticker.Stop()
	for {
		run, err := getRun(ctx, cfg, httpClient, runID)
		if err != nil {
			return "", err
		}
		if isTerminalRunStatus(string(run.Status)) {
			return string(run.Status), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// getRun fetches a single run's current state.
func getRun(ctx context.Context, cfg Config, httpClient *http.Client, runID string) (api.Run, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Server+"/api/v1/runs/"+runID, nil)
	if err != nil {
		return api.Run{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return api.Run{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return api.Run{}, fmt.Errorf("server: %s", strings.TrimSpace(string(b)))
	}
	var run api.Run
	if err := json.Unmarshal(b, &run); err != nil {
		return api.Run{}, err
	}
	return run, nil
}

// followRun polls the run's logs (GET /logs?after=N) every
// runFollowPollInterval, printing new lines to outW (stdout stream) / errW
// (stderr stream), until the run reaches a terminal status, which it returns.
// This is the same follow mechanism `unified-cli logs -f` uses; waitForRun adds
// the exit-code semantics on top.
func followRun(ctx context.Context, cfg Config, httpClient *http.Client, runID string, outW, errW io.Writer) (string, error) {
	var after int64
	for {
		lines, err := fetchRunLogsAfter(ctx, cfg, httpClient, runID, after)
		if err != nil {
			return "", err
		}
		for _, l := range lines {
			w := outW
			if l.Stream == "stderr" {
				w = errW
			}
			fmt.Fprintln(w, l.Line)
			if l.Seq > after {
				after = l.Seq
			}
		}
		run, err := getRun(ctx, cfg, httpClient, runID)
		if err != nil {
			return "", err
		}
		if isTerminalRunStatus(string(run.Status)) {
			// Drain any final lines emitted between the log fetch and the status
			// read so no trailing output is lost.
			final, ferr := fetchRunLogsAfter(ctx, cfg, httpClient, runID, after)
			if ferr == nil {
				for _, l := range final {
					w := outW
					if l.Stream == "stderr" {
						w = errW
					}
					fmt.Fprintln(w, l.Line)
				}
			}
			return string(run.Status), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(runFollowPollInterval):
		}
	}
}

// fetchRunLogsAfter fetches log lines with seq > after for a run.
func fetchRunLogsAfter(ctx context.Context, cfg Config, httpClient *http.Client, runID string, after int64) ([]api.LogLine, error) {
	url := cfg.Server + "/api/v1/runs/" + runID + "/logs?after=" + strconv.FormatInt(after, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server: %s", strings.TrimSpace(string(b)))
	}
	var lines []api.LogLine
	if err := json.Unmarshal(b, &lines); err != nil {
		return nil, err
	}
	return lines, nil
}
