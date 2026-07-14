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
		Use:     "unified-cli",
		Short:   "CLI for the unified-cd CI/CD server",
		Version: buildVersion(),
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "config file")
	root.PersistentFlags().StringVar(&serverOverride, "server", "", "override server URL")
	root.PersistentFlags().StringVar(&tokenOverride, "token", "", "override token")

	// resolve loads the config file and returns the configuration with env var and
	// flag overrides applied. Precedence (highest first): flag > env var > config file.
	resolve := func() (Config, error) {
		path := configPath
		if path == "" {
			path = DefaultConfigPath()
		}
		c, err := LoadConfig(path)
		if err != nil {
			return c, err
		}
		c = resolveConfig(c, os.Getenv("UNIFIED_SERVER"), os.Getenv("UNIFIED_TOKEN"), serverOverride, tokenOverride)
		if c.Server == "" {
			return c, fmt.Errorf("server URL is not set; use --server flag or set 'server' in config file")
		}
		return c, nil
	}

	root.AddCommand(newApplyCmd(resolve))
	root.AddCommand(newExportCmd(resolve))
	root.AddCommand(newJobsCmd(resolve))
	root.AddCommand(newRunCmd(resolve))
	root.AddCommand(newLogsCmd(resolve))
	root.AddCommand(newAgentCmd(resolve))
	root.AddCommand(newSecretCmd(resolve))
	root.AddCommand(newGitCredentialCmd(resolve))
	root.AddCommand(newScheduleCmd(resolve))
	root.AddCommand(newWebhookCmd(resolve))
	root.AddCommand(newAppSourceCmd(resolve))
	root.AddCommand(newTokenCmd(resolve))
	root.AddCommand(newApproveCmd(resolve))
	root.AddCommand(newRejectCmd(resolve))
	root.AddCommand(newArtifactCmd(resolve))
	root.AddCommand(newAuditCmd(resolve))
	root.AddCommand(newLoginCmd())
	root.AddCommand(newVersionCmd())
	return root
}

// resolveConfig applies env var and flag overrides on top of a config-file-loaded
// Config. Precedence (highest first): flag > env var > config file.
func resolveConfig(c Config, envServer, envToken, serverOverride, tokenOverride string) Config {
	if envServer != "" {
		c.Server = envServer
	}
	if envToken != "" {
		c.Token = envToken
	}
	if serverOverride != "" {
		c.Server = serverOverride
	}
	if tokenOverride != "" {
		c.Token = tokenOverride
	}
	return c
}
