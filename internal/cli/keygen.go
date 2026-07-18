package cli

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/spf13/cobra"
)

func newKeygenCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate a controller encryption key (KEK)",
		Long: "Generate a 32-byte key as 64 hex characters, for UNIFIED_CONTROLLER_KEY_FILE.\n\n" +
			"With --out the key is written directly with mode 0600. Prefer that over\n" +
			"shell redirection: `keygen > file` creates the file under your umask,\n" +
			"commonly 0644, which leaves the key readable by every user on the host.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			key := hex.EncodeToString(secrets.GenerateKey())
			if out == "" {
				fmt.Fprintln(cmd.OutOrStdout(), key)
				return nil
			}
			// O_EXCL: silently replacing a key would make every existing
			// secret undecryptable with no warning.
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				if os.IsExist(err) {
					return fmt.Errorf("%s already exists; remove it first if you intend to replace the key "+
						"(every secret encrypted with the old key becomes unreadable)", out)
				}
				return err
			}
			defer f.Close()
			if _, err := fmt.Fprintln(f, key); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote key to %s (mode 0600)\n", out)
			fmt.Fprintf(cmd.OutOrStdout(), "set UNIFIED_CONTROLLER_KEY_FILE=%s\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write the key to this path with mode 0600 instead of printing it")
	return cmd
}
