package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// NewRoot creates the root CLI command.
func NewRoot() *cobra.Command {
	var configPath string
	var serverOverride, tokenOverride string
	var headerOverrides []string

	root := &cobra.Command{
		Use:     "unified-cli",
		Short:   "CLI for the unified-cd CI/CD server",
		Version: buildVersion(),
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "config file")
	root.PersistentFlags().StringVar(&serverOverride, "server", "", "override server URL")
	root.PersistentFlags().StringVar(&tokenOverride, "token", "", "override token")
	root.PersistentFlags().StringArrayVarP(&headerOverrides, "header", "H", nil,
		"extra \"Key: Value\" HTTP header sent to the server (repeatable; e.g. an IAP token in Proxy-Authorization)")

	// resolveWith loads the config file and applies env var and flag overrides.
	// Precedence (highest first): flag > env var > config file. For headers,
	// entries ACCUMULATE in that order (config, then $UNIFIED_HEADER, then
	// flags), and later entries win for a repeated key at send time.
	resolveWith := func(requireServer bool) (Config, error) {
		path := configPath
		if path == "" {
			path = DefaultConfigPath()
		}
		c, err := LoadConfig(path)
		if err != nil {
			return c, err
		}
		c = resolveConfig(c, os.Getenv("UNIFIED_SERVER"), os.Getenv("UNIFIED_TOKEN"), serverOverride, tokenOverride)
		if h := os.Getenv("UNIFIED_HEADER"); h != "" {
			c.Headers = append(c.Headers, h)
		}
		c.Headers = append(c.Headers, headerOverrides...)
		if requireServer && c.Server == "" {
			return c, fmt.Errorf("server URL is not set; use --server flag or set 'server' in config file")
		}
		return c, nil
	}
	// resolve is the single place commands obtain their Config. It also
	// refreshes a stored SSO token that is about to expire (never one from
	// --token or $UNIFIED_TOKEN), warning once on failure.
	var refreshWarned bool
	resolve := func() (Config, error) {
		c, err := resolveWith(true)
		if err != nil || refreshWarned {
			return c, err
		}
		path := configPath
		if path == "" {
			path = DefaultConfigPath()
		}
		overridden := os.Getenv("UNIFIED_TOKEN") != "" || tokenOverride != ""
		var warn bytes.Buffer
		c = applyRefresh(context.Background(), path, c, overridden, time.Now(), &warn)
		if warn.Len() > 0 {
			refreshWarned = true
			_, _ = os.Stderr.Write(warn.Bytes())
		}
		return c, nil
	}

	// Install the extra-header transport once, before any command runs. Done
	// leniently (no server required — e.g. `login`, `keygen`) and scoped to the
	// server host inside installHeaderTransport, so a bad header value surfaces
	// early and headers never leak to other hosts.
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		c, err := resolveWith(false)
		if err != nil {
			return err
		}
		return installHeaderTransport(c)
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
	root.AddCommand(newAuthzCmd(resolve))
	root.AddCommand(newLoginCmd())
	root.AddCommand(newKeygenCmd())
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
