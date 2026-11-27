package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

// newBootstrapCmd returns `bootstrap`, which exchanges the one-time initial
// admin token for a PAT of the built-in admin account. It needs the server URL
// but no credentials: the request never carries an Authorization header.
func newBootstrapCmd(resolve func() (Config, error)) *cobra.Command {
	return newBootstrapCmdWithClient(resolve, http.DefaultClient)
}

// newBootstrapCmdWithClient is newBootstrapCmd with an injectable HTTP client (for tests).
func newBootstrapCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var initialToken, initialTokenFile, name string
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Exchange the one-time initial admin token for an admin PAT",
		Long: "Exchange the one-time initial admin token for a PAT of the built-in admin account " +
			"and print the PAT on stdout. The initial token comes from --initial-token, " +
			"--initial-token-file, or the UNIFIED_INITIAL_ADMIN_TOKEN environment variable.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if initialToken != "" && initialTokenFile != "" {
				return fmt.Errorf("--initial-token and --initial-token-file are mutually exclusive")
			}
			value := initialToken
			switch {
			case initialTokenFile != "":
				b, err := os.ReadFile(initialTokenFile)
				if err != nil {
					return fmt.Errorf("read initial token file: %w", err)
				}
				value = strings.TrimSpace(string(b))
			case value == "":
				value = strings.TrimSpace(os.Getenv("UNIFIED_INITIAL_ADMIN_TOKEN"))
			}
			if value == "" {
				return fmt.Errorf("the initial token is required: use --initial-token, --initial-token-file or UNIFIED_INITIAL_ADMIN_TOKEN")
			}
			cfg, err := resolve()
			if err != nil {
				return err
			}
			body, _ := json.Marshal(map[string]string{"initialToken": value, "name": name})
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, cfg.Server+"/api/v1/bootstrap", bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("server error: %s", strings.TrimSpace(string(b)))
			}
			var result api.CreatePATResponse
			if err := json.Unmarshal(b, &result); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Token)
			return nil
		},
	}
	cmd.Flags().StringVar(&initialToken, "initial-token", "", "the initial admin token (default: $UNIFIED_INITIAL_ADMIN_TOKEN)")
	cmd.Flags().StringVar(&initialTokenFile, "initial-token-file", "", "read the initial admin token from this file")
	cmd.Flags().StringVar(&name, "name", "", "name of the PAT (default \"bootstrap\")")
	return cmd
}
