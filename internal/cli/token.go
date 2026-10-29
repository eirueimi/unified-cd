package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

// newTokenCmd returns the command group for managing PATs.
func newTokenCmd(resolve func() (Config, error)) *cobra.Command {
	return newTokenCmdWithClient(resolve, http.DefaultClient)
}

// newTokenCmdWithClient is newTokenCmd with an injectable HTTP client (for tests).
func newTokenCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage Personal Access Tokens"}
	cmd.AddCommand(newTokenCreateCmd(resolve, httpClient))
	cmd.AddCommand(newTokenListCmd(resolve, httpClient))
	cmd.AddCommand(newTokenDeleteCmd(resolve, httpClient))
	return cmd
}

// newTokenCreateCmd returns the subcommand for creating a new PAT.
func newTokenCreateCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var expiresIn string
	var role string
	var permissions []string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new Personal Access Token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			body, _ := json.Marshal(api.CreatePATRequest{Name: args[0], ExpiresIn: expiresIn, Role: role, Permissions: permissions})
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cfg.Server+"/api/v1/tokens", bytes.NewReader(body))
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
				return fmt.Errorf("server error: %s", string(b))
			}
			var result api.CreatePATResponse
			if err := json.Unmarshal(b, &result); err != nil {
				return err
			}
			if len(permissions) > 0 && !samePermissionSet(permissions, result.Permissions) {
				// An older controller ignores "permissions" and mints a token
				// with the creator's full role. Revoke it; never print the secret.
				const refusal = "the server does not support --permission (it ignored the requested permissions)"
				if err := revokeToken(cmd.Context(), httpClient, cfg, result.ID); err != nil {
					return fmt.Errorf("%s; the token (ID %s) could not be revoked: %v; revoke it manually (unified-cd token delete %s) and upgrade the controller", refusal, result.ID, err, result.ID)
				}
				return fmt.Errorf("%s; the token was revoked; upgrade the controller", refusal)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Token created (shown only once):\n\n  %s\n\nName: %s  ID: %s  Role: %s\n", result.Token, result.Name, result.ID, result.Role)
			if len(result.Permissions) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Permissions: %s\n", strings.Join(result.Permissions, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&expiresIn, "expires-in", "", "expiry duration (e.g. 720h, 8760h)")
	cmd.Flags().StringVar(&role, "role", "", "role ceiling for the token: admin, developer, or viewer (must not exceed your global role)")
	cmd.Flags().StringArrayVar(&permissions, "permission", nil, "permission ceiling for the token, e.g. job.read (repeatable; you must hold each one; cannot be combined with --role)")
	return cmd
}

// newTokenListCmd returns the subcommand for listing PATs.
func newTokenListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List Personal Access Tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, cfg.Server+"/api/v1/tokens", nil)
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
				ceiling := t.Role
				if len(t.Permissions) > 0 {
					ceiling = strings.Join(t.Permissions, ",")
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t(%s)\n", t.ID, t.Name, ceiling, t.CreatedAt.Format("2006-01-02"))
			}
			return nil
		},
	}
}

// newTokenDeleteCmd returns the subcommand for deleting (revoking) a PAT.
func newTokenDeleteCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Revoke a Personal Access Token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, cfg.Server+"/api/v1/tokens/"+args[0], nil)
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
				return fmt.Errorf("server error: %s", string(b))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "token %q revoked\n", args[0])
			return nil
		},
	}
}

// samePermissionSet reports whether a and b hold the same permissions,
// ignoring order and duplicates.
func samePermissionSet(a, b []string) bool {
	set := func(l []string) map[string]bool {
		m := make(map[string]bool, len(l))
		for _, v := range l {
			m[v] = true
		}
		return m
	}
	sa, sb := set(a), set(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if !sb[k] {
			return false
		}
	}
	return true
}

// revokeToken deletes the PAT with the given ID.
func revokeToken(ctx context.Context, httpClient *http.Client, cfg Config, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, cfg.Server+"/api/v1/tokens/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server error: %s", strings.TrimSpace(string(b)))
	}
	return nil
}
