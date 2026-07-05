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

func newWebhookCmd(resolve func() (Config, error)) *cobra.Command {
	return newWebhookCmdWithClient(resolve, http.DefaultClient)
}

func newWebhookCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "WebhookReceiver management commands",
	}
	cmd.AddCommand(newWebhookListCmd(resolve, httpClient))
	cmd.AddCommand(newWebhookDeleteCmd(resolve, httpClient))
	return cmd
}

func newWebhookListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered webhook receivers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/webhooks/", nil)
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
			var list []api.WebhookReceiverMeta
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no webhook receivers)")
				return nil
			}
			for _, wr := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t(%s)\n", wr.Name, wr.UpdatedAt.Format("2006-01-02"))
			}
			return nil
		},
	}
}

func newWebhookDeleteCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a webhook receiver",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete,
				cfg.Server+"/api/v1/webhooks/"+args[0], nil)
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
			fmt.Fprintf(cmd.OutOrStdout(), "webhook receiver %q deleted\n", args[0])
			return nil
		},
	}
}
