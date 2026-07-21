package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

func newAgentCmd(resolve func() (Config, error)) *cobra.Command {
	return newAgentCmdWithClient(resolve, http.DefaultClient)
}

func newAgentCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Agent management commands",
	}
	cmd.AddCommand(newAgentListCmd(resolve, httpClient))
	cmd.AddCommand(newAgentGetCmd(resolve, httpClient))
	cmd.AddCommand(newAgentRunsCmd(resolve, httpClient))
	cmd.AddCommand(newAgentEnrollmentCmd(resolve, httpClient))
	cmd.AddCommand(newAgentEnrollmentPolicyCmd(resolve, httpClient))
	cmd.AddCommand(newAgentIdentityCmd(resolve, httpClient))
	return cmd
}

func newAgentListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/agents", nil)
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
			var list []api.AgentInfo
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no agents)")
				return nil
			}
			for _, a := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n",
					a.ID, a.Hostname, a.OS, strings.Join(a.Labels, ","), a.LastSeenAt.Format("2006-01-02 15:04"))
			}
			return nil
		},
	}
}

func newAgentGetCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "get <agent-id>",
		Short: "Show details of a registered agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/agents/"+args[0], nil)
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
			var a api.AgentInfo
			if err := json.Unmarshal(b, &a); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "ID:         %s\n", a.ID)
			fmt.Fprintf(out, "Hostname:   %s\n", a.Hostname)
			fmt.Fprintf(out, "OS:         %s\n", a.OS)
			fmt.Fprintf(out, "Labels:     %s\n", strings.Join(a.Labels, ","))
			fmt.Fprintf(out, "Version:    %s\n", a.Version)
			fmt.Fprintf(out, "LastSeenAt: %s\n", a.LastSeenAt.Local().Format("2006-01-02 15:04:05"))
			if len(a.Env) > 0 {
				fmt.Fprintf(out, "Env:\n")
				for k, v := range a.Env {
					fmt.Fprintf(out, "  %s=%s\n", k, v)
				}
			}
			return nil
		},
	}
}

func newAgentRunsCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "runs <agent-id>",
		Short: "List recent runs claimed by an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/agents/"+args[0]+"/runs", nil)
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
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n",
					r.ID, r.JobName, r.Status, r.CreatedAt.Format("2006-01-02"), r.TriggeredBy)
			}
			return nil
		},
	}
}
