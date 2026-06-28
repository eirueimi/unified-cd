package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
	"github.com/eirueimi/unified-cd/internal/api"
)

func newGitCredentialCmd(resolve func() (Config, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gitcredential",
		Short: "manage git credentials for git:// template URIs",
	}
	cmd.AddCommand(newListGitCredentialsCmd(resolve))
	cmd.AddCommand(newDeleteGitCredentialCmd(resolve))
	return cmd
}

func newListGitCredentialsCmd(resolve func() (Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list all git credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/gitcredentials", nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server: %s", string(body))
			}
			var list []api.GitCredentialMeta
			if err := json.Unmarshal(body, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(none)")
				return nil
			}
			for _, gc := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\thost=%s type=%s secretRef=%s\n",
					gc.Name, gc.Host, gc.CredType, gc.SecretRef)
			}
			return nil
		},
	}
}

func newDeleteGitCredentialCmd(resolve func() (Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "delete a git credential by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete,
				cfg.Server+"/api/v1/gitcredentials/"+args[0], nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %s", string(body))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "gitcredential deleted: %s\n", args[0])
			return nil
		},
	}
}
