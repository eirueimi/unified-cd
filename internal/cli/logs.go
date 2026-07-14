package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

// newLogsCmd creates a command that displays logs for a run.
func newLogsCmd(resolve func() (Config, error)) *cobra.Command {
	var follow, timestamps, showStep bool
	cmd := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "print logs for a run (with -f to follow)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			runID := args[0]
			var after int64
			ctx := cmd.Context()

			// When prefixing lines with their step, fetch the step list once to
			// resolve indices to names (best-effort — nil map falls back to
			// "step N" / "System").
			var stepNames map[int]string
			if showStep {
				stepNames = fetchStepNames(ctx, cfg, http.DefaultClient, runID)
			}

			for {
				lines, status, err := fetchLogs(ctx, cfg, runID, after)
				if err != nil {
					return err
				}
				for _, l := range lines {
					fmt.Fprintln(cmd.OutOrStdout(), formatLogLine(l, timestamps, showStep, stepNames))
					if l.Seq > after {
						after = l.Seq
					}
				}
				if !follow {
					return nil
				}
				switch status {
				case api.RunSucceeded, api.RunFailed, api.RunCancelled:
					return nil
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(300 * time.Millisecond):
				}
			}
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow output (poll every 300ms)")
	cmd.Flags().BoolVarP(&timestamps, "timestamps", "t", false, "prefix each line with its local HH:MM:SS timestamp")
	cmd.Flags().BoolVar(&showStep, "step", false, "prefix each line with its step name (e.g. [build])")
	return cmd
}

// formatLogLine renders one log line with the optional timestamp and step-name
// prefixes. Order is "HH:MM:SS [step] line".
func formatLogLine(l api.LogLine, timestamps, showStep bool, stepNames map[int]string) string {
	prefix := ""
	if timestamps {
		prefix += l.Timestamp.Local().Format("15:04:05") + " "
	}
	if showStep {
		prefix += "[" + stepLabel(l.StepIndex, stepNames) + "] "
	}
	return prefix + l.Line
}

// stepLabel resolves a log line's step index to a display name: -1 is the
// run-level "System" stream; a known step index uses its name; anything else
// falls back to "step N" (mirrors the web UI's stepName lookup).
func stepLabel(idx int, stepNames map[int]string) string {
	if idx == -1 {
		return "System"
	}
	if n, ok := stepNames[idx]; ok && n != "" {
		return n
	}
	return "step " + strconv.Itoa(idx)
}

// fetchStepNames fetches the run's steps and returns an index→name map.
// Best-effort: returns nil on any error (callers fall back to "step N").
func fetchStepNames(ctx context.Context, cfg Config, httpClient *http.Client, runID string) map[int]string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Server+"/api/v1/runs/"+runID+"/steps", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var steps []api.StepReport
	if err := json.NewDecoder(resp.Body).Decode(&steps); err != nil {
		return nil
	}
	names := make(map[int]string, len(steps))
	for _, s := range steps {
		if _, seen := names[s.Index]; !seen { // first (non-variant) name wins
			names[s.Index] = s.Name
		}
	}
	return names
}

// fetchLogs fetches log lines after the given sequence number and the run's
// current status. Reuses the shared run-log helpers (see wait.go). The status
// fetch is best-effort: a status error yields the lines with an empty status
// (the caller's follow loop simply keeps polling), matching prior behavior.
func fetchLogs(ctx context.Context, cfg Config, runID string, after int64) ([]api.LogLine, api.RunStatus, error) {
	lines, err := fetchRunLogsAfter(ctx, cfg, http.DefaultClient, runID, after)
	if err != nil {
		return nil, "", err
	}
	run, err := getRun(ctx, cfg, http.DefaultClient, runID)
	if err != nil {
		return lines, "", nil
	}
	return lines, run.Status, nil
}
