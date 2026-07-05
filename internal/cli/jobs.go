package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

func newJobsCmd(resolve func() (Config, error)) *cobra.Command {
	return newJobsCmdWithClient(resolve, http.DefaultClient)
}

func newJobsCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Job management commands",
	}
	cmd.AddCommand(newJobsListCmd(resolve, httpClient))
	cmd.AddCommand(newJobsGetCmd(resolve, httpClient))
	cmd.AddCommand(newJobsShowYAMLCmd(resolve, httpClient))
	cmd.AddCommand(newJobsDeleteCmd(resolve, httpClient))
	return cmd
}

func newJobsGetCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show details of a job including its input parameters",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/jobs/"+args[0], nil)
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
			var job api.Job
			if err := json.Unmarshal(b, &job); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Name:        %s\n", job.Name)
			fmt.Fprintf(out, "ID:          %s\n", job.ID)
			fmt.Fprintf(out, "APIVersion:  %s\n", job.APIVersion)
			fmt.Fprintf(out, "Updated:     %s\n", job.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
			if len(job.Inputs) > 0 {
				fmt.Fprintf(out, "Inputs:\n")
				for _, in := range job.Inputs {
					attrs := in.Type
					if in.Required {
						attrs += ", required"
					}
					if in.Default != nil {
						attrs += fmt.Sprintf(", default=%v", in.Default)
					}
					desc := ""
					if in.Description != "" {
						desc = "  " + in.Description
					}
					fmt.Fprintf(out, "  %-20s (%s)%s\n", in.Name, attrs, desc)
				}
			}
			return nil
		},
	}
}

func newJobsShowYAMLCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "show-yaml <name>",
		Short: "Show the YAML definition of a job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/jobs/"+args[0]+"/yaml", nil)
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

func newJobsListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/jobs", nil)
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
			var list []api.Job
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no jobs)")
				return nil
			}
			for _, j := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t(%s)\n", j.Name, j.UpdatedAt.Format("2006-01-02"))
			}
			return nil
		},
	}
}

func newJobsDeleteCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a job (run history is also cascade-deleted)",
		Long: "Delete a job. All run history, step execution records, and logs associated with this job are also cascade-deleted and cannot be recovered.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete,
				cfg.Server+"/api/v1/jobs/"+args[0], nil)
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
			fmt.Fprintf(cmd.OutOrStdout(), "job %q deleted\n", args[0])
			return nil
		},
	}
}
