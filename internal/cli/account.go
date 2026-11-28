package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

// newAccountCmd returns the command group for managing accounts.
func newAccountCmd(resolve func() (Config, error)) *cobra.Command {
	return newAccountCmdWithClient(resolve, http.DefaultClient)
}

// newAccountCmdWithClient is newAccountCmd with an injectable HTTP client (for tests).
func newAccountCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{Use: "account", Short: "Manage accounts"}
	cmd.AddCommand(newAccountListCmd(resolve, httpClient, ""))
	cmd.AddCommand(newAccountToggleCmd(resolve, httpClient, "disable", "Disable a human account"))
	cmd.AddCommand(newAccountToggleCmd(resolve, httpClient, "enable", "Enable a human account"))
	return cmd
}

// newServiceAccountCmd returns the "serviceaccount" group, whose only command
// is "list", an alias for "account list --kind service".
func newServiceAccountCmd(resolve func() (Config, error)) *cobra.Command {
	return newServiceAccountCmdWithClient(resolve, http.DefaultClient)
}

func newServiceAccountCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{Use: "serviceaccount", Short: "List service accounts"}
	cmd.AddCommand(newAccountListCmd(resolve, httpClient, "service"))
	return cmd
}

// newAccountListCmd lists accounts. A non-empty fixedKind pins --kind (no flag).
func newAccountListCmd(resolve func() (Config, error), httpClient *http.Client, fixedKind string) *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List accounts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			k := kind
			if fixedKind != "" {
				k = fixedKind
			}
			if k != "" && k != "human" && k != "service" {
				return fmt.Errorf("--kind must be human or service")
			}
			path := "/api/v1/accounts"
			if k != "" {
				path += "?kind=" + url.QueryEscape(k)
			}
			b, status, err := accountRequest(cmd.Context(), httpClient, cfg, http.MethodGet, path)
			if err != nil {
				return err
			}
			if status != http.StatusOK {
				return fmt.Errorf("server error: %s", string(b))
			}
			var list []api.Account
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tNAME\tEMAIL\tDISABLED\tLAST SEEN")
			for _, a := range list {
				seen := "-"
				if a.LastSeenAt != nil {
					seen = a.LastSeenAt.Format(time.DateOnly)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\t%s\n", a.ID, a.Kind, a.Name, a.Email, a.Disabled, seen)
			}
			return tw.Flush()
		},
	}
	if fixedKind == "" {
		cmd.Flags().StringVar(&kind, "kind", "", "only list accounts of this kind: human or service")
	}
	return cmd
}

// newAccountToggleCmd returns "account disable|enable <id>".
func newAccountToggleCmd(resolve func() (Config, error), httpClient *http.Client, verb, short string) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			b, status, err := accountRequest(cmd.Context(), httpClient, cfg, http.MethodPost, "/api/v1/accounts/"+url.PathEscape(args[0])+"/"+verb)
			if err != nil {
				return err
			}
			if status != http.StatusNoContent && status != http.StatusOK {
				return fmt.Errorf("server error: %s", string(b))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "account %s %sd\n", args[0], verb)
			return nil
		},
	}
}

// accountRequest sends an authenticated request and returns the body and status.
func accountRequest(ctx context.Context, httpClient *http.Client, cfg Config, method, path string) ([]byte, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.Server+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}
