package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
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
	cmd.AddCommand(newRunWaitCmd(resolve, httpClient))
	cmd.AddCommand(newRunShowCmd(resolve, httpClient))
	cmd.AddCommand(newRunListCmd(resolve, httpClient))
	cmd.AddCommand(newRunListActiveCmd(resolve, httpClient))
	cmd.AddCommand(newRunDeleteCmd(resolve, httpClient))
	cmd.AddCommand(newRunCancelCmd(resolve, httpClient))
	cmd.AddCommand(newRunOutputsCmd(resolve, httpClient))
	cmd.AddCommand(newRunShowYAMLCmd(resolve, httpClient))
	cmd.AddCommand(newRunApprovalsCmd(resolve, httpClient))
	return cmd
}

func newRunTriggerCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var paramKV []string
	var wait, follow bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "trigger <job-name>",
		Short: "trigger a run of an applied job",
		Args:  cobra.ExactArgs(1),
		// The wait/follow paths return an *ExitError with a distinct exit code
		// (failed=1, cancelled=2, timeout=124); silence the usage dump so a
		// non-zero run outcome doesn't print command help.
		SilenceUsage: true,
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
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
				cfg.Server+"/api/v1/runs", bytes.NewReader(body))
			if err != nil {
				return err
			}
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
			if wait || follow {
				return waitForRun(cmd.Context(), cfg, httpClient, run.ID, timeout, follow, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&paramKV, "param", nil, "parameter k=v (repeatable)")
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the run reaches a terminal state; exit non-zero if it failed (1), was cancelled (2), or timed out (124)")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream the run's step logs while waiting (implies --wait)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "max time to wait (e.g. 30m); 0 means no timeout")
	return cmd
}

func newRunWaitCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var follow bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <run-id>",
		Short: "Wait for a run to finish; exit non-zero if it did not succeed",
		Args:  cobra.ExactArgs(1),
		// See newRunTriggerCmd: silence usage so a non-zero run outcome
		// (returned as an *ExitError) does not print command help.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			return waitForRun(cmd.Context(), cfg, httpClient, args[0], timeout, follow, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "stream the run's step logs while waiting")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "max time to wait (e.g. 30m); 0 means no timeout")
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
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/runs?jobName="+jobName, nil)
			if err != nil {
				return err
			}
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

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Server+"/api/v1/runs/"+runID, nil)
			if err != nil {
				return err
			}
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

			req2, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Server+"/api/v1/runs/"+runID+"/steps", nil)
			if err != nil {
				return err
			}
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
			req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete,
				cfg.Server+"/api/v1/runs/"+args[0], nil)
			if err != nil {
				return err
			}
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

func newRunListActiveCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list-active",
		Short: "List runs in Pending, Queued, or Running state across all jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/runs/active", nil)
			if err != nil {
				return err
			}
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
				fmt.Fprintln(cmd.OutOrStdout(), "(no active runs)")
				return nil
			}
			for _, r := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n",
					r.ID, r.JobName, r.Status, r.CreatedAt.Format("2006-01-02 15:04"), r.TriggeredBy)
			}
			return nil
		},
	}
}

func newRunOutputsCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "outputs <run-id>",
		Short: "Show run-level outputs reported by the job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/runs/"+args[0]+"/outputs", nil)
			if err != nil {
				return err
			}
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
			var outputs api.RunOutputs
			if err := json.Unmarshal(b, &outputs); err != nil {
				return err
			}
			if len(outputs.Outputs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no outputs)")
				return nil
			}
			keys := make([]string, 0, len(outputs.Outputs))
			for k := range outputs.Outputs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", k, outputs.Outputs[k])
			}
			return nil
		},
	}
}

func newRunShowYAMLCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "show-yaml <run-id>",
		Short: "Show the YAML definition the run was executed with",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/runs/"+args[0]+"/yaml", nil)
			if err != nil {
				return err
			}
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
			fmt.Fprint(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
}

func newRunApprovalsCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "approvals <run-id>",
		Short: "List approval gates of a run and their state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/runs/"+args[0]+"/approvals", nil)
			if err != nil {
				return err
			}
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
			var list []api.RunApproval
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no approvals)")
				return nil
			}
			for _, a := range list {
				name := a.StepName
				if name == "" {
					name = fmt.Sprintf("step[%d]", a.StepIndex)
				}
				decided := ""
				if a.DecidedBy != "" {
					decided = "by " + a.DecidedBy
					if a.DecidedAt != nil {
						decided += " at " + a.DecidedAt.Local().Format("2006-01-02 15:04:05")
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "[%d]\t%s\t%s\t%s\n", a.StepIndex, name, a.Status, decided)
			}
			return nil
		},
	}
}

func newRunCancelCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Cancel a hung or in-progress run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
				cfg.Server+"/api/v1/runs/"+args[0]+"/cancel", nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %d: %s", resp.StatusCode, string(b))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "run %q cancelled\n", args[0])
			return nil
		},
	}
}
