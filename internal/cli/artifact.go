package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

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
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			if err != nil {
				return err
			}
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

// withoutRedirectCredentials returns a copy of c that drops the Authorization
// header on EVERY redirect. The controller may redirect a download to a
// presigned object-store URL, which carries its own credentials in the query
// string; the redirect target never needs the controller bearer token, and
// Go's default policy would still forward it to a same-host or subdomain
// target. This client is download-only, so no redirect legitimately needs it.
func withoutRedirectCredentials(c *http.Client) *http.Client {
	clone := *c
	prev := c.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		req.Header.Del("Authorization")
		if prev != nil {
			return prev(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &clone
}

// redactURLError removes the query string from the URL in a *url.Error: after
// a redirect it is a presigned URL, which is a bearer credential.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			u.RawQuery = ""
			u.User = nil
			return &url.Error{Op: ue.Op, URL: u.String(), Err: ue.Err}
		}
	}
	return err
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
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := withoutRedirectCredentials(httpClient).Do(req)
			if err != nil {
				return redactURLError(err)
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
