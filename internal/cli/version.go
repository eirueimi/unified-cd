package cli

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version can be overridden at build time:
//
//	go build -ldflags "-X github.com/eirueimi/unified-cd/internal/cli.version=v1.2.3"
var version = ""

// buildVersion returns the version string embedded at build time, the module
// version recorded by go install, or "dev" for local untagged builds.
func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// newVersionCmd returns the `unified-cli version` command, which prints the
// build-time (or module/dev) version to stdout. This is distinct from the
// root command's built-in `--version` flag (wired via NewRoot's
// Version: buildVersion()); both report the same value, but `version` as a
// subcommand is easier to script against (e.g. in shell conditionals) than
// parsing --version output.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the unified-cli version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), buildVersion())
			return nil
		},
	}
}
