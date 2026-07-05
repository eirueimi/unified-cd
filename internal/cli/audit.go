package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

// newAuditCmd returns the command group for viewing the audit log.
func newAuditCmd(resolve func() (Config, error)) *cobra.Command {
	return newAuditCmdWithClient(resolve, http.DefaultClient)
}

func newAuditCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{Use: "audit", Short: "View the audit log (admin only)"}
	cmd.AddCommand(newAuditListCmdWithClient(resolve, httpClient))
	return cmd
}

// newAuditListCmdWithClient returns the "audit list" subcommand, printing a
// table of time, actor, action, resource, and status, newest first.
func newAuditListCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent audit log entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			url := cfg.Server + "/api/v1/audit"
			if limit > 0 {
				url += "?limit=" + strconv.Itoa(limit)
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
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
			var list []api.AuditLog
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no audit log entries)")
				return nil
			}
			for _, a := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%d\n",
					a.OccurredAt.Format("2006-01-02T15:04:05Z07:00"), a.Actor, a.Action, a.Resource, a.Status)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max number of entries to show (server default: 100)")
	return cmd
}
