package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// NewRoot creates the root CLI command.
func NewRoot() *cobra.Command {
	var configPath string
	var serverOverride, tokenOverride string

	root := &cobra.Command{
		Use:     "unified-cd",
		Short:   "CLI for the unified-cd CI/CD server",
		Version: buildVersion(),
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "config file")
	root.PersistentFlags().StringVar(&serverOverride, "server", "", "override server URL")
	root.PersistentFlags().StringVar(&tokenOverride, "token", "", "override token")

	// resolve loads the config file and returns the configuration with flag overrides applied.
	resolve := func() (Config, error) {
		path := configPath
		if path == "" {
			path = DefaultConfigPath()
		}
		c, err := LoadConfig(path)
		if err != nil {
			return c, err
		}
		if envServer := os.Getenv("UNIFIED_SERVER"); envServer != "" && c.Server == "" {
			c.Server = envServer
		}
		if serverOverride != "" {
			c.Server = serverOverride
		}
		if envToken := os.Getenv("UNIFIED_TOKEN"); envToken != "" && c.Token == "" {
			c.Token = envToken
		}
		if tokenOverride != "" {
			c.Token = tokenOverride
		}
		if c.Server == "" {
			return c, fmt.Errorf("server URL is not set; use --server flag or set 'server' in config file")
		}
		return c, nil
	}

	root.AddCommand(newApplyCmd(resolve))
	root.AddCommand(newJobsCmd(resolve))
	root.AddCommand(newRunCmd(resolve))
	root.AddCommand(newLogsCmd(resolve))
	root.AddCommand(newAgentCmd(resolve))
	root.AddCommand(newSecretCmd(resolve))
	root.AddCommand(newGitCredentialCmd(resolve))
	root.AddCommand(newTokenCmd(resolve))
	root.AddCommand(newLoginCmd())
	return root
}
