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

func newScheduleCmd(resolve func() (Config, error)) *cobra.Command {
	return newScheduleCmdWithClient(resolve, http.DefaultClient)
}

func newScheduleCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "manage cron Schedules",
	}
	cmd.AddCommand(newScheduleListCmd(resolve, httpClient))
	cmd.AddCommand(newScheduleDeleteCmd(resolve, httpClient))
	return cmd
}

func newScheduleListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list all registered schedules",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/schedules/", nil)
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
			var list []api.ScheduleMeta
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no schedules)")
				return nil
			}
			for _, sc := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tcron=%s job=%s\n", sc.Name, sc.Cron, sc.JobName)
			}
			return nil
		},
	}
}

func newScheduleDeleteCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "delete a schedule by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete,
				cfg.Server+"/api/v1/schedules/"+args[0], nil)
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
			fmt.Fprintf(cmd.OutOrStdout(), "schedule deleted: %s\n", args[0])
			return nil
		},
	}
}
