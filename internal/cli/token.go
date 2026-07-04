package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

// newTokenCmd returns the command group for managing PATs.
func newTokenCmd(resolve func() (Config, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage Personal Access Tokens"}
	cmd.AddCommand(newTokenCreateCmd(resolve))
	cmd.AddCommand(newTokenListCmd(resolve))
	cmd.AddCommand(newTokenDeleteCmd(resolve))
	return cmd
}

// newTokenCreateCmd returns the subcommand for creating a new PAT.
func newTokenCreateCmd(resolve func() (Config, error)) *cobra.Command {
	var expiresIn string
	var role string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new Personal Access Token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			body, _ := json.Marshal(api.CreatePATRequest{Name: args[0], ExpiresIn: expiresIn, Role: role})
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, cfg.Server+"/api/v1/tokens", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server error: %s", string(b))
			}
			var result api.CreatePATResponse
			if err := json.Unmarshal(b, &result); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Token created (shown only once):\n\n  %s\n\nName: %s  ID: %s  Role: %s\n", result.Token, result.Name, result.ID, result.Role)
			return nil
		},
	}
	cmd.Flags().StringVar(&expiresIn, "expires-in", "", "expiry duration (e.g. 720h, 8760h)")
	cmd.Flags().StringVar(&role, "role", "", "role for the token: admin, developer, or viewer (default: your own role; capped at it)")
	return cmd
}

// newTokenListCmd returns the subcommand for listing PATs.
func newTokenListCmd(resolve func() (Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List Personal Access Tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, cfg.Server+"/api/v1/tokens", nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server error: %s", string(b))
			}
			var list []api.PATMeta
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no tokens)")
				return nil
			}
			for _, t := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t(%s)\n", t.ID, t.Name, t.Role, t.CreatedAt.Format("2006-01-02"))
			}
			return nil
		},
	}
}

// newTokenDeleteCmd returns the subcommand for deleting (revoking) a PAT.
func newTokenDeleteCmd(resolve func() (Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Revoke a Personal Access Token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, cfg.Server+"/api/v1/tokens/"+args[0], nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server error: %s", string(b))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "token %q revoked\n", args[0])
			return nil
		},
	}
}
