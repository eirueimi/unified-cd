package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/unified-cd/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

func newRunCmd(resolve func() (Config, error)) *cobra.Command {
	return newRunCmdWithClient(resolve, http.DefaultClient)
}

func newRunCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run management commands",
	}
	cmd.AddCommand(newRunTriggerCmd(resolve, httpClient))
	cmd.AddCommand(newRunShowCmd(resolve, httpClient))
	cmd.AddCommand(newRunListCmd(resolve, httpClient))
	cmd.AddCommand(newRunDeleteCmd(resolve, httpClient))
	return cmd
}

func newRunTriggerCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var paramKV []string
	cmd := &cobra.Command{
		Use:   "trigger <job-name>",
		Short: "trigger a run of an applied job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			params := map[string]string{}
			for _, kv := range paramKV {
				idx := strings.Index(kv, "=")
				if idx <= 0 {
					return fmt.Errorf("invalid --param %q (expected k=v)", kv)
				}
				params[kv[:idx]] = kv[idx+1:]
			}
			body, _ := json.Marshal(api.TriggerRunRequest{JobName: args[0], Params: params})
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
				cfg.Server+"/api/v1/runs", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server: %s", string(b))
			}
			var run api.Run
			if err := json.Unmarshal(b, &run); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", run.ID)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&paramKV, "param", nil, "parameter k=v (repeatable)")
	return cmd
}

func newRunListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var jobName string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent runs for a specified job",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/runs?jobName="+jobName, nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server: %s", string(b))
			}
			var list []api.Run
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no runs)")
				return nil
			}
			for _, r := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n",
					r.ID, r.Status, r.CreatedAt.Format("2006-01-02"), r.TriggeredBy)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&jobName, "job", "", "target job name (required)")
	_ = cmd.MarkFlagRequired("job")
	return cmd
}

func newRunShowCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "show <run-id>",
		Short: "Show details of a run including step statuses",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			runID := args[0]
			ctx := context.Background()

			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Server+"/api/v1/runs/"+runID, nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server: %s", string(b))
			}
			var run api.Run
			if err := json.Unmarshal(b, &run); err != nil {
				return err
			}

			req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Server+"/api/v1/runs/"+runID+"/steps", nil)
			req2.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp2, err := httpClient.Do(req2)
			if err != nil {
				return err
			}
			defer resp2.Body.Close()
			b2, _ := io.ReadAll(resp2.Body)
			var steps []api.StepReport
			_ = json.Unmarshal(b2, &steps)

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "ID:          %s\n", run.ID)
			fmt.Fprintf(out, "Job:         %s\n", run.JobName)
			fmt.Fprintf(out, "Status:      %s\n", run.Status)
			fmt.Fprintf(out, "Triggered:   %s\n", run.TriggeredBy)
			fmt.Fprintf(out, "Created:     %s\n", run.CreatedAt.Local().Format("2006-01-02 15:04:05"))
			fmt.Fprintf(out, "Updated:     %s\n", run.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
			if len(run.Params) > 0 {
				fmt.Fprintf(out, "Params:\n")
				for k, v := range run.Params {
					fmt.Fprintf(out, "  %s=%s\n", k, v)
				}
			}
			if len(steps) > 0 {
				fmt.Fprintf(out, "Steps:\n")
				for _, s := range steps {
					name := s.Name
					if name == "" {
						name = fmt.Sprintf("step[%d]", s.Index)
					}
					exitInfo := ""
					if s.ExitCode != nil {
						exitInfo = fmt.Sprintf(" (exit %d)", *s.ExitCode)
					}
					fmt.Fprintf(out, "  [%d] %-20s %s%s\n", s.Index, name, s.Status, exitInfo)
				}
			}
			return nil
		},
	}
}

func newRunDeleteCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <run-id>",
		Short: "Delete a run in a terminal state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete,
				cfg.Server+"/api/v1/runs/"+args[0], nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %s", string(b))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "run %q deleted\n", args[0])
			return nil
		},
	}
}
