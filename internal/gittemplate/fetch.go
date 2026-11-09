package gittemplate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitErr augments an exec error with git's stderr. cmd.Output()/Run() capture
// stderr into (*exec.ExitError).Stderr when cmd.Stderr is nil, but the default
// error string is only "exit status N" — the actual reason (auth required, TLS
// failure, unknown ref) lives in stderr. This surfaces it so callers and logs
// see what git actually said.
func gitErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}

// Fetcher retrieves a file from a git repository using the system git binary.
type Fetcher struct {
	// KnownHosts, when non-nil, returns the pinned OpenSSH known_hosts lines
	// configured for host (the GitCredential knownHosts field). A non-empty
	// result makes ssh use StrictHostKeyChecking=yes against exactly those keys.
	KnownHosts func(ctx context.Context, host string) (string, error)
	// KnownHostsFile is the persistent known_hosts file used for trust-on-first-use
	// when no keys are pinned for a host. When empty, TOFU state lives in a
	// per-operation temporary file and is therefore lost after each fetch.
	KnownHostsFile string
}

// NewFetcher returns a new Fetcher.
func NewFetcher() *Fetcher { return &Fetcher{} }

// Fetch retrieves the file at uri.Path from the git repository at uri.Ref.
// token: plaintext HTTPS token, supplied to git via GIT_ASKPASS (never embedded in the
// URL or passed as an argument); leave empty for public repos.
// sshKey: plaintext SSH private key content; leave empty for HTTPS auth.
func (f *Fetcher) Fetch(ctx context.Context, uri URI, token, sshKey string) ([]byte, error) {
	if sshKey != "" {
		return f.fetchSSH(ctx, uri, sshKey)
	}
	repoURL := buildHTTPSURL(uri, token)
	extraEnv, cleanup, err := authEnv(token)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return f.fetchWithURL(ctx, repoURL, uri.Path, uri.Ref, extraEnv)
}

// FetchWithURL fetches filePath at ref from repoURL. Exposed for testing with file:// URLs.
func (f *Fetcher) FetchWithURL(ctx context.Context, repoURL, filePath, ref string) ([]byte, error) {
	return f.fetchWithURL(ctx, repoURL, filePath, ref, nil)
}

func (f *Fetcher) fetchWithURL(ctx context.Context, repoURL, filePath, ref string, extraEnv []string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "gittemplate-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	env := gitEnv(extraEnv)

	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w\n%s%s", strings.Join(args, " "), err, out, hostKeyHint(out))
		}
		return nil
	}

	if err := run("init"); err != nil {
		return nil, err
	}
	if err := run("fetch", "--depth=1", repoURL, "--", ref); err != nil {
		return nil, fmt.Errorf("fetch %s@%s: %w", repoURL, ref, err)
	}
	cmd := exec.CommandContext(ctx, "git", "show", "FETCH_HEAD:"+filePath)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show FETCH_HEAD:%s: %w", filePath, gitErr(err))
	}
	return data, nil
}

func (f *Fetcher) fetchSSH(ctx context.Context, uri URI, sshKey string) ([]byte, error) {
	sshCmd, cleanup, err := f.sshSetup(ctx, uri.Host, sshKey)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	repoURL := fmt.Sprintf("git@%s:%s/%s.git", uri.Host, uri.Owner, uri.Repo)

	dir, err := os.MkdirTemp("", "gittemplate-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_SSH_COMMAND="+sshCmd,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w\n%s%s", strings.Join(args, " "), err, out, hostKeyHint(out))
		}
		return nil
	}

	if err := run("init"); err != nil {
		return nil, err
	}
	if err := run("fetch", "--depth=1", repoURL, "--", uri.Ref); err != nil {
		return nil, fmt.Errorf("fetch SSH %s@%s: %w", repoURL, uri.Ref, err)
	}
	cmd := exec.CommandContext(ctx, "git", "show", "FETCH_HEAD:"+uri.Path)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND="+sshCmd,
	)
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show FETCH_HEAD:%s (SSH): %w", uri.Path, gitErr(err))
	}
	return data, nil
}

// authPlaceholderUser is the HTTP Basic auth username sent alongside a token.
// GitLab requires the token in the password slot, not the username slot (a bare
// "https://<token>@host" URL is rejected with "HTTP Basic: Access denied"); GitHub
// accepts any username here too, so a fixed placeholder works for both.
const authPlaceholderUser = "oauth2"

// buildHTTPSURL constructs the clone URL. When a token is present it only embeds the
// placeholder username, never the token itself; the token is supplied separately via
// GIT_ASKPASS (see authEnv) so it never appears in the URL, in process arguments, or in
// error messages built from the URL.
func buildHTTPSURL(uri URI, token string) string {
	if token != "" {
		return fmt.Sprintf("https://%s@%s/%s/%s.git", authPlaceholderUser, uri.Host, uri.Owner, uri.Repo)
	}
	return fmt.Sprintf("https://%s/%s/%s.git", uri.Host, uri.Owner, uri.Repo)
}

// insertPlaceholderUser adds the auth placeholder username to a caller-supplied HTTPS
// URL that doesn't already carry credentials.
func insertPlaceholderUser(repoURL string) string {
	return strings.Replace(repoURL, "https://", "https://"+authPlaceholderUser+"@", 1)
}

// authEnv returns the extra environment variables needed for git to authenticate with
// token, plus a cleanup function. The token is written to an askpass helper script that
// reads it from an environment variable known only to the git subprocess, so it never
// appears in argv or in any URL/error string the caller might log. If token is empty,
// GIT_ASKPASS is set to a no-op so git doesn't hang waiting for interactive input.
func authEnv(token string) (env []string, cleanup func(), err error) {
	if token == "" {
		return []string{"GIT_ASKPASS=true"}, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "gitaskpass-")
	if err != nil {
		return nil, nil, fmt.Errorf("create askpass dir: %w", err)
	}
	cleanup = func() { os.RemoveAll(dir) }
	scriptPath := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho \"$GIT_TEMPLATE_TOKEN\"\n"), 0o700); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("write askpass script: %w", err)
	}
	return []string{
		"GIT_ASKPASS=" + scriptPath,
		"GIT_TEMPLATE_TOKEN=" + token,
	}, cleanup, nil
}

// gitEnv builds the environment for a git subprocess, falling back to a no-op
// GIT_ASKPASS when extraEnv doesn't provide one.
func gitEnv(extraEnv []string) []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if len(extraEnv) == 0 {
		return append(env, "GIT_ASKPASS=true")
	}
	return append(env, extraEnv...)
}

// ResolveCommitSHA uses git ls-remote to return the commit SHA for the given ref in the repository.
func (f *Fetcher) ResolveCommitSHA(ctx context.Context, repoURL, ref, token, sshKey string) (string, error) {
	if strings.HasPrefix(repoURL, "-") {
		return "", fmt.Errorf("repo URL %q must not start with '-' (git option injection)", repoURL)
	}
	var env []string
	if sshKey != "" {
		sshCmd, cleanup, err := f.sshSetup(ctx, sshHostFromURL(repoURL), sshKey)
		if err != nil {
			return "", err
		}
		defer cleanup()
		env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND="+sshCmd)
	} else {
		if token != "" {
			repoURL = insertPlaceholderUser(repoURL)
		}
		extraEnv, cleanup, err := authEnv(token)
		if err != nil {
			return "", err
		}
		defer cleanup()
		env = gitEnv(extraEnv)
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", repoURL, "--", ref)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-remote %s %s: %w%s", repoURL, ref, gitErr(err), hostKeyHintErr(err))
	}
	return parseLsRemoteOutput(string(out), ref)
}

// FetchDir retrieves all *.yaml / *.yml files under the specified path in the repository.
func (f *Fetcher) FetchDir(ctx context.Context, repoURL, ref, path, token, sshKey string) (map[string][]byte, error) {
	var extraEnv []string
	if sshKey != "" {
		sshCmd, cleanup, err := f.sshSetup(ctx, sshHostFromURL(repoURL), sshKey)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		extraEnv = []string{"GIT_SSH_COMMAND=" + sshCmd}
	} else if token != "" {
		repoURL = insertPlaceholderUser(repoURL)
		tokenEnv, cleanup, err := authEnv(token)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		extraEnv = tokenEnv
	}
	return f.fetchDirWithURL(ctx, repoURL, ref, path, extraEnv)
}

func (f *Fetcher) fetchDirWithURL(ctx context.Context, repoURL, ref, path string, extraEnv []string) (map[string][]byte, error) {
	if strings.HasPrefix(repoURL, "-") {
		return nil, fmt.Errorf("repo URL %q must not start with '-' (git option injection)", repoURL)
	}
	dir, err := os.MkdirTemp("", "gitfetchdir-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	env := gitEnv(extraEnv)

	runGit := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w\n%s%s", strings.Join(args, " "), err, out, hostKeyHint(out))
		}
		return nil
	}

	if err := runGit("init"); err != nil {
		return nil, err
	}
	if err := runGit("fetch", "--depth=1", repoURL, "--", ref); err != nil {
		return nil, fmt.Errorf("fetch %s@%s: %w", repoURL, ref, err)
	}

	treePath := strings.TrimSuffix(path, "/")
	lsArgs := []string{"ls-tree", "-r", "--name-only", "FETCH_HEAD"}
	if treePath != "" {
		lsArgs = append(lsArgs, "--", treePath)
	}
	lsCmd := exec.CommandContext(ctx, "git", lsArgs...)
	lsCmd.Dir = dir
	lsCmd.Env = env
	lsOut, err := lsCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree: %w", gitErr(err))
	}

	files := map[string][]byte{}
	for _, line := range strings.Split(strings.TrimSpace(string(lsOut)), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, ".yaml") && !strings.HasSuffix(line, ".yml") {
			continue
		}
		showCmd := exec.CommandContext(ctx, "git", "show", "FETCH_HEAD:"+line)
		showCmd.Dir = dir
		showCmd.Env = env
		data, err := showCmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git show FETCH_HEAD:%s: %w", line, gitErr(err))
		}
		files[line] = data
	}
	return files, nil
}

// sshSetup writes the SSH private key (and the pinned known_hosts keys for host,
// when configured) into a private temporary directory and returns the
// GIT_SSH_COMMAND value that verifies the server's host key, plus a cleanup
// function. host is the hostname the credential was matched on.
//
// Host key policy:
//   - pinned keys configured for host: StrictHostKeyChecking=yes against exactly
//     those keys; any other key is rejected.
//   - otherwise: trust-on-first-use (StrictHostKeyChecking=accept-new) against the
//     persistent KnownHostsFile, so a key that later changes is rejected.
func (f *Fetcher) sshSetup(ctx context.Context, host, sshKey string) (string, func(), error) {
	keyDir, err := os.MkdirTemp("", "gitkey-")
	if err != nil {
		return "", nil, fmt.Errorf("create ssh key dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(keyDir) }
	if err := os.Chmod(keyDir, 0o700); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("chmod ssh key dir: %w", err)
	}
	keyPath := filepath.Join(keyDir, "id")
	if err := os.WriteFile(keyPath, []byte(sshKey), 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write ssh key: %w", err)
	}

	var pinned string
	if f.KnownHosts != nil && host != "" {
		pinned, err = f.KnownHosts(ctx, host)
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("look up known hosts for %q: %w", host, err)
		}
	}
	if strings.TrimSpace(pinned) != "" {
		khPath := filepath.Join(keyDir, "known_hosts")
		if err := os.WriteFile(khPath, []byte(strings.TrimSpace(pinned)+"\n"), 0o600); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("write known_hosts: %w", err)
		}
		return buildSSHCommand(keyPath, khPath, true), cleanup, nil
	}

	khPath := f.KnownHostsFile
	if khPath == "" {
		khPath = filepath.Join(keyDir, "known_hosts")
	} else if err := os.MkdirAll(filepath.Dir(khPath), 0o700); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("create known_hosts dir: %w", err)
	}
	return buildSSHCommand(keyPath, khPath, false), cleanup, nil
}

// buildSSHCommand builds the GIT_SSH_COMMAND value. strict selects
// StrictHostKeyChecking=yes (pinned keys) over accept-new (trust on first use).
// The system-wide known_hosts is ignored so the result depends only on the
// configured keys.
func buildSSHCommand(keyPath, knownHostsPath string, strict bool) string {
	mode := "accept-new"
	if strict {
		mode = "yes"
	}
	return fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=%s -o UserKnownHostsFile=%s -o GlobalKnownHostsFile=/dev/null",
		shellQuote(filepath.ToSlash(keyPath)), mode, shellQuote(filepath.ToSlash(knownHostsPath)))
}

// shellQuote single-quotes s for the POSIX shell git uses to run GIT_SSH_COMMAND.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshHostFromURL extracts the hostname from an ssh:// URL or scp-like
// (user@host:path) repository address. It returns "" when none can be found.
func sshHostFromURL(repoURL string) string {
	if strings.Contains(repoURL, "://") {
		if u, err := url.Parse(repoURL); err == nil {
			return u.Hostname()
		}
		return ""
	}
	rest := repoURL
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if colon := strings.Index(rest, ":"); colon > 0 {
		return strings.Trim(rest[:colon], "[]")
	}
	return ""
}

// hostKeyHint returns an operator-oriented explanation when ssh's output shows a
// host key verification failure, so a changed key is not mistaken for a
// transient network error.
func hostKeyHint(out []byte) string {
	msg := string(out)
	if strings.Contains(msg, "Host key verification failed") || strings.Contains(msg, "REMOTE HOST IDENTIFICATION HAS CHANGED") {
		return "\nSSH host key verification failed: the server's host key does not match the pinned or previously trusted key; if the key legitimately changed, update the GitCredential knownHosts (or remove the stale entry from the controller's known_hosts file)"
	}
	return ""
}

func hostKeyHintErr(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return hostKeyHint(ee.Stderr)
	}
	return ""
}

// parseLsRemoteOutput extracts the first commit SHA from the output of git ls-remote.
func parseLsRemoteOutput(output, ref string) (string, error) {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 1 && len(parts[0]) == 40 {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("ref %q not found in ls-remote output", ref)
}
