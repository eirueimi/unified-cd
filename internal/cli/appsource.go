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

func newAppSourceCmd(resolve func() (Config, error)) *cobra.Command {
	return newAppSourceCmdWithClient(resolve, http.DefaultClient)
}

func newAppSourceCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "appsource",
		Short: "manage GitOps AppSources",
	}
	cmd.AddCommand(newAppSourceSyncCmd(resolve, httpClient))
	cmd.AddCommand(newAppSourceListCmd(resolve, httpClient))
	cmd.AddCommand(newAppSourceGetCmd(resolve, httpClient))
	cmd.AddCommand(newAppSourceDeleteCmd(resolve, httpClient))
	return cmd
}

func newAppSourceSyncCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "sync <name>",
		Short: "force a re-sync of an AppSource on the next reconciler tick",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
				cfg.Server+"/api/v1/appsources/"+args[0]+"/sync", nil)
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
			fmt.Fprintf(cmd.OutOrStdout(), "appsource sync scheduled: %s\n", args[0])
			return nil
		},
	}
}

func newAppSourceListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list all registered AppSources",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/appsources", nil)
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
			var list []api.AppSourceMeta
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no appsources)")
				return nil
			}
			for _, a := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\trepo=%s rev=%s lastCommit=%s\n",
					a.Name, a.RepoURL, a.TargetRevision, shortSHA(a.LastCommit))
			}
			return nil
		},
	}
}

func newAppSourceGetCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "show details of an AppSource",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/appsources/"+args[0], nil)
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
			var a api.AppSourceMeta
			if err := json.Unmarshal(b, &a); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "name:           %s\n", a.Name)
			fmt.Fprintf(w, "repoURL:        %s\n", a.RepoURL)
			fmt.Fprintf(w, "targetRevision: %s\n", a.TargetRevision)
			fmt.Fprintf(w, "path:           %s\n", a.Path)
			fmt.Fprintf(w, "lastCommit:     %s\n", a.LastCommit)
			if a.LastSyncedAt != nil {
				fmt.Fprintf(w, "lastSyncedAt:   %s\n", a.LastSyncedAt.Format("2006-01-02 15:04:05"))
			} else {
				fmt.Fprintf(w, "lastSyncedAt:   (never)\n")
			}
			return nil
		},
	}
}

func newAppSourceDeleteCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "delete an AppSource by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete,
				cfg.Server+"/api/v1/appsources/"+args[0], nil)
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
			fmt.Fprintf(cmd.OutOrStdout(), "appsource deleted: %s\n", args[0])
			return nil
		},
	}
}

// shortSHA truncates a commit SHA to 7 characters for display; empty stays empty.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
