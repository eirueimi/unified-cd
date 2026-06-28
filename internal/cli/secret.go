package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/unified-cd/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

func newSecretCmd(resolve func() (Config, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage secrets",
	}
	cmd.AddCommand(newSecretSetCmd(resolve))
	cmd.AddCommand(newSecretListCmd(resolve))
	cmd.AddCommand(newSecretDeleteCmd(resolve))
	return cmd
}

func newSecretSetCmd(resolve func() (Config, error)) *cobra.Command {
	var filePath string
	cmd := &cobra.Command{
		Use:   "set <name> [value]",
		Short: "Create or update a secret",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			name := args[0]
			var value string
			if len(args) == 2 {
				value = args[1]
			} else if filePath != "" {
				data, err := os.ReadFile(filePath)
				if err != nil {
					return err
				}
				value = string(data)
			} else {
				data, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				value = string(data)
			}
			body, _ := json.Marshal(api.SetSecretRequest{Name: name, Value: value})
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
				cfg.Server+"/api/v1/secrets/", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %s", string(b))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "secret %q set\n", name)
			return nil
		},
	}
	cmd.Flags().StringVarP(&filePath, "file", "f", "", "read value from file")
	return cmd
}

func newSecretListCmd(resolve func() (Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List secret names (values not shown)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/secrets/", nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server: %s", string(b))
			}
			var list []api.SecretMeta
			if err := json.Unmarshal(b, &list); err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no secrets)")
				return nil
			}
			for _, s := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t(%s)\n", s.Name, s.CreatedAt.Format("2006-01-02"))
			}
			return nil
		},
	}
}

func newSecretDeleteCmd(resolve func() (Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete,
				cfg.Server+"/api/v1/secrets/"+args[0], nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %s", string(b))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "secret %q deleted\n", args[0])
			return nil
		},
	}
}
