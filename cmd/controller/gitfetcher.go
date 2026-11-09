package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/store"
)

// gitKnownHostsPath picks the persistent known_hosts file used for SSH
// trust-on-first-use. An explicit path wins; otherwise the file lives under the
// local data directory when one is configured, and finally under the OS temp
// directory (in which case TOFU state is lost whenever the temp directory is
// cleared, e.g. on a container restart).
func gitKnownHostsPath(explicit, dataDir string) string {
	switch {
	case explicit != "":
		return explicit
	case dataDir != "":
		return filepath.Join(dataDir, ".ssh", "known_hosts")
	default:
		p := filepath.Join(os.TempDir(), "unified-cd-ssh", "known_hosts")
		slog.Warn("no persistent directory for SSH known_hosts; trust-on-first-use state will be lost when the temp directory is cleared. Set --git-known-hosts-file or pin keys with GitCredential spec.knownHosts", "path", p)
		return p
	}
}

// newGitFetcher builds the controller-side git fetcher. SSH host keys are
// verified against the GitCredential's pinned knownHosts when set, and against
// the persistent TOFU file otherwise.
func newGitFetcher(st store.Store, knownHostsFile string) *gittemplate.Fetcher {
	f := gittemplate.NewFetcher()
	f.KnownHostsFile = knownHostsFile
	f.KnownHosts = func(ctx context.Context, host string) (string, error) {
		gc, err := st.GetGitCredentialByHost(ctx, host)
		if err != nil || gc == nil {
			return "", err
		}
		return gc.KnownHosts, nil
	}
	return f
}
