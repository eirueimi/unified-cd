package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

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
		// A fixed temp path would be predictable and could be pre-created by
		// another local user, so use a fresh private directory per process.
		dir, err := os.MkdirTemp("", "unified-cd-ssh-")
		if err != nil {
			slog.Error("cannot create a temporary directory for SSH known_hosts", "error", err)
			os.Exit(1)
		}
		p := filepath.Join(dir, "known_hosts")
		slog.Info("no persistent directory for SSH known_hosts; using a per-process temporary file, so trust-on-first-use state is lost on restart. Set --git-known-hosts-file or pin keys with GitCredential spec.knownHosts", "path", p)
		return p
	}
}

// newGitFetcher builds the controller-side git fetcher. SSH host keys are
// verified against the GitCredential's pinned knownHosts when set, and against
// the persistent TOFU file otherwise.
func newGitFetcher(st store.Store, knownHostsFile string, timeout time.Duration, maxRepoBytes int64) *gittemplate.Fetcher {
	f := gittemplate.NewFetcher()
	f.KnownHostsFile = knownHostsFile
	f.Timeout = timeout
	f.MaxRepoBytes = maxRepoBytes
	f.KnownHosts = func(ctx context.Context, host string) (string, error) {
		gc, err := st.GetGitCredentialByHost(ctx, host)
		if err != nil || gc == nil {
			return "", err
		}
		return gc.KnownHosts, nil
	}
	return f
}

// gitTimeoutDefault resolves the --git-timeout default from UNIFIED_GIT_TIMEOUT
// (a Go duration): the maximum time one controller-side git operation (an
// AppSource fetch, ls-remote, or git:// template fetch) may take before its
// process tree is killed. Unset, invalid, or non-positive values use 5m; a
// timeout cannot be disabled because a hung repository would stall the serial
// AppSource reconciler.
func gitTimeoutDefault() time.Duration {
	const def = gittemplate.DefaultGitTimeout
	v := os.Getenv("UNIFIED_GIT_TIMEOUT")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("invalid UNIFIED_GIT_TIMEOUT, using default", "value", v, "default", def)
		return def
	}
	return d
}

// defaultGitMaxRepoBytes is the default cap on a fetched repository's size.
const defaultGitMaxRepoBytes = int64(1) << 30

// gitMaxRepoBytesDefault resolves the --git-max-repo-bytes default from
// UNIFIED_GIT_MAX_REPO_BYTES. 0 disables the limit; unset or invalid values use
// 1 GiB.
func gitMaxRepoBytesDefault() int64 {
	v := os.Getenv("UNIFIED_GIT_MAX_REPO_BYTES")
	if v == "" {
		return defaultGitMaxRepoBytes
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		slog.Warn("invalid UNIFIED_GIT_MAX_REPO_BYTES, using default", "value", v, "default", defaultGitMaxRepoBytes)
		return defaultGitMaxRepoBytes
	}
	return n
}
