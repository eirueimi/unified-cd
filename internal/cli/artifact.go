package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/spf13/cobra"
)

func newArtifactCmd(resolve func() (Config, error)) *cobra.Command {
	return newArtifactCmdWithClient(resolve, http.DefaultClient)
}

func newArtifactCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "artifact",
		Short: "List and download run artifacts",
	}
	cmd.AddCommand(newArtifactListCmd(resolve, httpClient))
	cmd.AddCommand(newArtifactDownloadCmd(resolve, httpClient))
	return cmd
}

func newArtifactListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list <run-id>",
		Short: "List artifacts for a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			url := cfg.Server + "/api/v1/runs/" + args[0] + "/artifacts"
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %s (%d)", string(b), resp.StatusCode)
			}
			var list []api.ArtifactInfo
			if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
				return err
			}
			for _, a := range list {
				fmt.Fprintln(cmd.OutOrStdout(), a.Name)
			}
			return nil
		},
	}
}

func newArtifactDownloadCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var dest string
	cmd := &cobra.Command{
		Use:   "download <run-id> <name>",
		Short: "Download and extract a run artifact",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			url := cfg.Server + "/api/v1/runs/" + args[0] + "/artifacts/" + args[1]
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %s (%d)", string(b), resp.StatusCode)
			}
			if dest == "" {
				dest = "."
			}
			if err := artifact.ExtractTarZstd(resp.Body, dest); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "extracted %s of run %s to %s\n", args[1], args[0], dest)
			return nil
		},
	}
	cmd.Flags().StringVar(&dest, "dest", ".", "destination directory")
	return cmd
}
