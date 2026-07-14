package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

// newLogsCmd creates a command that displays logs for a run.
func newLogsCmd(resolve func() (Config, error)) *cobra.Command {
	var follow bool
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

			for {
				lines, status, err := fetchLogs(ctx, cfg, runID, after)
				if err != nil {
					return err
				}
				for _, l := range lines {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\n", l.Line)
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
	return cmd
}

// fetchLogs fetches log lines after the specified sequence number and the run status from the master server.
func fetchLogs(ctx context.Context, cfg Config, runID string, after int64) ([]api.LogLine, api.RunStatus, error) {
	url := cfg.Server + "/api/v1/runs/" + runID + "/logs?after=" + strconv.FormatInt(after, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("server: %s", string(body))
	}
	var lines []api.LogLine
	if err := json.Unmarshal(body, &lines); err != nil {
		return nil, "", err
	}

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Server+"/api/v1/runs/"+runID, nil)
	if err != nil {
		return lines, "", nil
	}
	req2.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return lines, "", nil
	}
	defer resp2.Body.Close()
	b2, _ := io.ReadAll(resp2.Body)
	var run api.Run
	_ = json.Unmarshal(b2, &run)
	return lines, run.Status, nil
}
