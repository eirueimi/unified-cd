package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
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
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				cfg.Server+"/api/v1/gitcredentials", nil)
			if err != nil {
				return err
			}
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
				line := fmt.Sprintf("%s\thost=%s secretRef=%s type=%s", gc.Name, gc.Host, gc.SecretRef, gc.CredType)
				if app := gc.GitHubApp; app != nil {
					inst := "auto"
					if app.InstallationID != 0 {
						inst = strconv.FormatInt(app.InstallationID, 10)
					}
					line += fmt.Sprintf(" appId=%d installationId=%s", app.AppID, inst)
					if app.APIURL != "" {
						line += " apiURL=" + app.APIURL
					}
				}
				line += fmt.Sprintf(" controllerUse=%t", gc.ControllerUse)
				if ja := gc.JobAccess; ja != nil {
					perms := make([]string, 0, len(ja.Permissions))
					for k, v := range ja.Permissions {
						perms = append(perms, k+"="+v)
					}
					sort.Strings(perms)
					line += fmt.Sprintf(" jobAccess=folders:%s repos:%s perms:%s",
						strings.Join(ja.Folders, ","), strings.Join(ja.Repositories, ","), strings.Join(perms, ","))
				}
				if gc.KnownHosts != "" {
					line += " knownHosts=pinned"
				}
				fmt.Fprintln(cmd.OutOrStdout(), line)
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
			req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete,
				cfg.Server+"/api/v1/gitcredentials/"+args[0], nil)
			if err != nil {
				return err
			}
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
