package gittemplate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	// Timeout bounds each whole git operation (fetch, ls-remote); the git process
	// tree is killed when it elapses. Zero means DefaultGitTimeout.
	Timeout time.Duration
	// MaxRepoBytes aborts a fetch whose scratch repository grows past this many
	// bytes. Zero disables the limit.
	MaxRepoBytes int64
}

// NewFetcher returns a new Fetcher.
func NewFetcher() *Fetcher { return &Fetcher{} }

// fetch retrieves the file at uri.Path from the git repository at uri.Ref.
// cred.Token: plaintext HTTPS token, supplied to git via GIT_ASKPASS (never embedded in the
// URL or passed as an argument); leave empty for public repos.
// cred.SSHKey: plaintext SSH private key content; leave empty for HTTPS auth.
func (f *Fetcher) fetch(ctx context.Context, cancel context.CancelCauseFunc, uri URI, cred Credential) ([]byte, error) {
	if cred.SSHKey != "" {
		return f.fetchSSH(ctx, cancel, uri, cred.SSHKey)
	}
	repoURL := buildHTTPSURL(uri, cred)
	extraEnv, cleanup, err := authEnv(cred.Token)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return f.fetchWithURL(ctx, cancel, repoURL, uri.Path, uri.Ref, extraEnv)
}

// FetchWithURL fetches filePath at ref from repoURL. Exposed for testing with file:// URLs.
func (f *Fetcher) FetchWithURL(ctx context.Context, repoURL, filePath, ref string) ([]byte, error) {
	ctx, cancel := f.opContext(ctx)
	defer cancel(nil)
	data, err := f.fetchWithURL(ctx, cancel, repoURL, filePath, ref, nil)
	return data, opErr(ctx, f, err)
}

func (f *Fetcher) fetchWithURL(ctx context.Context, cancel context.CancelCauseFunc, repoURL, filePath, ref string, extraEnv []string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "gittemplate-")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(dir)

	env := gitEnv(extraEnv)

	run := func(args ...string) error {
		cmd := gitCommand(ctx, args...)
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
	stopSize := f.watchSize(ctx, dir, cancel)
	if err := run("fetch", "--depth=1", repoURL, "--", ref); err != nil {
		return nil, fmt.Errorf("fetch %s@%s: %w", repoURL, ref, err)
	}
	if err := stopSize(); err != nil {
		return nil, err
	}
	data, err := f.gitShow(ctx, dir, append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), "FETCH_HEAD:"+filePath)
	if err != nil {
		return nil, fmt.Errorf("git show FETCH_HEAD:%s: %w", filePath, gitErr(err))
	}
	return data, nil
}

func (f *Fetcher) fetchSSH(ctx context.Context, cancel context.CancelCauseFunc, uri URI, sshKey string) ([]byte, error) {
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
		cmd := gitCommand(ctx, args...)
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
	stopSize := f.watchSize(ctx, dir, cancel)
	if err := run("fetch", "--depth=1", repoURL, "--", uri.Ref); err != nil {
		return nil, fmt.Errorf("fetch SSH %s@%s: %w", repoURL, uri.Ref, err)
	}
	if err := stopSize(); err != nil {
		return nil, err
	}
	data, err := f.gitShow(ctx, dir, append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND="+sshCmd,
	), "FETCH_HEAD:"+uri.Path)
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

// GitHubAppUser is the HTTP Basic auth username GitHub documents for
// installation access tokens.
const GitHubAppUser = "x-access-token"

// authUser returns the HTTP Basic auth username to send with cred.Token,
// URL-escaped for use as URL userinfo.
func authUser(cred Credential) string {
	name := cred.Username
	if name == "" {
		name = authPlaceholderUser
	}
	return url.User(name).String()
}

// buildHTTPSURL constructs the clone URL. When a token is present it only embeds the
// username (cred.Username, default the placeholder), never the token itself; the token
// is supplied separately via GIT_ASKPASS (see authEnv) so it never appears in the URL,
// in process arguments, or in error messages built from the URL.
func buildHTTPSURL(uri URI, cred Credential) string {
	if cred.Token != "" {
		return fmt.Sprintf("https://%s@%s/%s/%s.git", authUser(cred), uri.Host, uri.Owner, uri.Repo)
	}
	return fmt.Sprintf("https://%s/%s/%s.git", uri.Host, uri.Owner, uri.Repo)
}

// insertUser adds the auth username for cred to a caller-supplied HTTPS URL that
// doesn't already carry credentials.
func insertUser(repoURL string, cred Credential) string {
	return strings.Replace(repoURL, "https://", "https://"+authUser(cred)+"@", 1)
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

// resolveCommitSHA uses git ls-remote to return the commit SHA for the given ref in the repository.
func (f *Fetcher) resolveCommitSHA(ctx context.Context, repoURL, ref string, cred Credential) (string, error) {
	if strings.HasPrefix(repoURL, "-") {
		return "", fmt.Errorf("repo URL %q must not start with '-' (git option injection)", repoURL)
	}
	var env []string
	if cred.SSHKey != "" {
		sshCmd, cleanup, err := f.sshSetup(ctx, sshHostFromURL(repoURL), cred.SSHKey)
		if err != nil {
			return "", err
		}
		defer cleanup()
		env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND="+sshCmd)
	} else {
		if cred.Token != "" {
			repoURL = insertUser(repoURL, cred)
		}
		extraEnv, cleanup, err := authEnv(cred.Token)
		if err != nil {
			return "", err
		}
		defer cleanup()
		env = gitEnv(extraEnv)
	}
	cmd := gitCommand(ctx, "ls-remote", repoURL, "--", ref)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-remote %s %s: %w%s", repoURL, ref, gitErr(err), hostKeyHintErr(err))
	}
	return parseLsRemoteOutput(string(out), ref)
}

// fetchDir retrieves all *.yaml / *.yml files under the specified path in the repository.
func (f *Fetcher) fetchDir(ctx context.Context, cancel context.CancelCauseFunc, repoURL, ref, path string, cred Credential) (map[string][]byte, error) {
	var extraEnv []string
	if cred.SSHKey != "" {
		sshCmd, cleanup, err := f.sshSetup(ctx, sshHostFromURL(repoURL), cred.SSHKey)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		extraEnv = []string{"GIT_SSH_COMMAND=" + sshCmd}
	} else if cred.Token != "" {
		repoURL = insertUser(repoURL, cred)
		tokenEnv, cleanup, err := authEnv(cred.Token)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		extraEnv = tokenEnv
	}
	return f.fetchDirWithURL(ctx, cancel, repoURL, ref, path, extraEnv)
}

func (f *Fetcher) fetchDirWithURL(ctx context.Context, cancel context.CancelCauseFunc, repoURL, ref, path string, extraEnv []string) (map[string][]byte, error) {
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
		cmd := gitCommand(ctx, args...)
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
	stopSize := f.watchSize(ctx, dir, cancel)
	if err := runGit("fetch", "--depth=1", repoURL, "--", ref); err != nil {
		return nil, fmt.Errorf("fetch %s@%s: %w", repoURL, ref, err)
	}
	if err := stopSize(); err != nil {
		return nil, err
	}

	treePath := strings.TrimSuffix(path, "/")
	lsArgs := []string{"ls-tree", "-r", "--name-only", "FETCH_HEAD"}
	if treePath != "" {
		lsArgs = append(lsArgs, "--", treePath)
	}
	lsCmd := gitCommand(ctx, lsArgs...)
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
		data, err := f.gitShow(ctx, dir, env, "FETCH_HEAD:"+line)
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
		cmd, err := buildSSHCommand(keyPath, khPath, true)
		if err != nil {
			cleanup()
			return "", nil, err
		}
		return cmd, cleanup, nil
	}

	khPath := f.KnownHostsFile
	if khPath == "" {
		khPath = filepath.Join(keyDir, "known_hosts")
	} else if err := ValidateKnownHostsFile(khPath); err != nil {
		cleanup()
		return "", nil, err
	}
	cmd, err := buildSSHCommand(keyPath, khPath, false)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return cmd, cleanup, nil
}

// buildSSHCommand builds the GIT_SSH_COMMAND value. strict selects
// StrictHostKeyChecking=yes (pinned keys) over accept-new (trust on first use).
// The system-wide known_hosts is ignored so the result depends only on the
// configured keys.
func buildSSHCommand(keyPath, knownHostsPath string, strict bool) (string, error) {
	mode := "accept-new"
	if strict {
		mode = "yes"
	}
	key, err := sshOptionPath(keyPath)
	if err != nil {
		return "", err
	}
	kh, err := sshOptionPath(knownHostsPath)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ssh -o %s -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=%s -o %s -o GlobalKnownHostsFile=/dev/null",
		shellQuote("IdentityFile="+key), mode, shellQuote("UserKnownHostsFile="+kh)), nil
}

// sshOptionPath renders a file path as an ssh_config value. ssh splits
// UserKnownHostsFile on whitespace and expands "~" and "%" tokens in file
// options, so the path is double-quoted and "%" doubled; otherwise a path with a
// space would silently point ssh at a different file (and ssh accepts any host
// key when it cannot write its known_hosts). A path containing a double quote or
// newline cannot be represented and is rejected.
func sshOptionPath(p string) (string, error) {
	if err := CheckSSHPath(p); err != nil {
		return "", err
	}
	return `"` + strings.ReplaceAll(filepath.ToSlash(p), "%", "%%") + `"`, nil
}

// CheckSSHPath reports whether p can be passed to ssh as a key or known_hosts
// path (see sshOptionPath).
func CheckSSHPath(p string) error {
	if strings.ContainsAny(p, "\"\n\r\x00") {
		return fmt.Errorf("ssh path %q must not contain a double quote or newline", p)
	}
	return nil
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
		return "\nSSH host key verification failed: the server's host key does not match the pinned or previously trusted key; if the key legitimately changed, update the GitCredential knownHosts (use the [host]:port form for a non-default port; or remove the stale entry from the controller's known_hosts file)"
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

// DefaultGitTimeout bounds one controller-side git operation (a whole fetch or
// ls-remote, not each subprocess) when Fetcher.Timeout is zero.
const DefaultGitTimeout = 5 * time.Minute

var (
	errGitTimeout   = errors.New("git operation timed out")
	errRepoTooLarge = errors.New("repository exceeds the size limit")
)

// sizePollInterval is how often an in-flight fetch's scratch directory is
// measured against Fetcher.MaxRepoBytes. A variable so tests can shorten it.
var sizePollInterval = 250 * time.Millisecond

// opContext derives the context for one public operation: it carries the
// per-operation deadline (Timeout, default DefaultGitTimeout) and a cancel
// function used to abort when the size limit is exceeded. The returned cancel
// must always be called.
func (f *Fetcher) opContext(parent context.Context) (context.Context, context.CancelCauseFunc) {
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultGitTimeout
	}
	tctx, tcancel := context.WithTimeoutCause(parent, timeout, errGitTimeout)
	cctx, ccancel := context.WithCancelCause(tctx)
	return cctx, func(cause error) {
		ccancel(cause)
		tcancel()
	}
}

// opErr rewrites err into a clear timeout / size-limit error when that is what
// aborted the operation (git itself only reports "signal: killed").
func opErr(ctx context.Context, f *Fetcher, err error) error {
	if err == nil {
		return nil
	}
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errGitTimeout):
		timeout := f.Timeout
		if timeout <= 0 {
			timeout = DefaultGitTimeout
		}
		return fmt.Errorf("%w after %s: %v", errGitTimeout, timeout, err)
	case errors.Is(cause, errRepoTooLarge):
		return fmt.Errorf("%w (limit %d bytes): %v", errRepoTooLarge, f.MaxRepoBytes, err)
	}
	return err
}

// gitCommand builds a git subprocess that is killed together with its children
// when ctx ends. WaitDelay bounds how long Wait lingers on output pipes still
// held open by an orphaned helper.
func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	killTree(cmd)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// dirSize sums the sizes of all regular files under dir.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// watchSize aborts the operation (via cancel) as soon as the scratch directory
// dir grows past MaxRepoBytes while git is fetching into it. The returned
// function stops the watcher and reports a final verdict. With no limit
// configured it is a no-op.
func (f *Fetcher) watchSize(ctx context.Context, dir string, cancel context.CancelCauseFunc) func() error {
	if f.MaxRepoBytes <= 0 {
		return func() error { return nil }
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(sizePollInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if dirSize(dir) > f.MaxRepoBytes {
					cancel(errRepoTooLarge)
					return
				}
			}
		}
	}()
	return func() error {
		close(done)
		if size := dirSize(dir); size > f.MaxRepoBytes {
			return fmt.Errorf("%w (limit %d bytes, fetched %d)", errRepoTooLarge, f.MaxRepoBytes, size)
		}
		return nil
	}
}

// Fetch retrieves the file at uri.Path from the git repository at uri.Ref.
// cred.Token: plaintext HTTPS token, supplied to git via GIT_ASKPASS (never embedded in the
// URL or passed as an argument); leave empty for public repos.
// cred.SSHKey: plaintext SSH private key content; leave empty for HTTPS auth.
// The whole operation is bounded by Fetcher.Timeout and Fetcher.MaxRepoBytes.
func (f *Fetcher) Fetch(ctx context.Context, uri URI, cred Credential) ([]byte, error) {
	ctx, cancel := f.opContext(ctx)
	defer cancel(nil)
	data, err := f.fetch(ctx, cancel, uri, cred)
	return data, opErr(ctx, f, err)
}

// ResolveCommitSHA uses git ls-remote to return the commit SHA for the given ref
// in the repository. Bounded by Fetcher.Timeout.
func (f *Fetcher) ResolveCommitSHA(ctx context.Context, repoURL, ref string, cred Credential) (string, error) {
	ctx, cancel := f.opContext(ctx)
	defer cancel(nil)
	sha, err := f.resolveCommitSHA(ctx, repoURL, ref, cred)
	return sha, opErr(ctx, f, err)
}

// FetchDir retrieves all *.yaml / *.yml files under the specified path in the
// repository. Bounded by Fetcher.Timeout and Fetcher.MaxRepoBytes.
func (f *Fetcher) FetchDir(ctx context.Context, repoURL, ref, path string, cred Credential) (map[string][]byte, error) {
	ctx, cancel := f.opContext(ctx)
	defer cancel(nil)
	files, err := f.fetchDir(ctx, cancel, repoURL, ref, path, cred)
	return files, opErr(ctx, f, err)
}

// gitShow returns the content of obj (e.g. "FETCH_HEAD:path") in the scratch
// repo at dir. When MaxRepoBytes is set, a blob larger than it is rejected
// (checked with git cat-file -s) before its content is read into memory.
func (f *Fetcher) gitShow(ctx context.Context, dir string, env []string, obj string) ([]byte, error) {
	if f.MaxRepoBytes > 0 {
		sc := gitCommand(ctx, "cat-file", "-s", obj)
		sc.Dir = dir
		sc.Env = env
		out, err := sc.Output()
		if err != nil {
			return nil, err
		}
		if n, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); perr == nil && n > f.MaxRepoBytes {
			return nil, fmt.Errorf("%w: %s is %d bytes (limit %d)", errRepoTooLarge, obj, n, f.MaxRepoBytes)
		}
	}
	cmd := gitCommand(ctx, "show", obj)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.Output()
}

// ValidateKnownHostsFile prepares and vets the persistent known_hosts path used
// for SSH trust-on-first-use: the path must be representable to ssh, and its
// directory is created (0700) and must be a real directory owned by the current
// user and not group/other-writable (Unix). The controller calls it at startup
// so a bad configuration fails closed.
func ValidateKnownHostsFile(path string) error {
	if err := CheckSSHPath(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create known_hosts dir: %w", err)
	}
	if err := checkOwnedPrivateDir(dir); err != nil {
		return fmt.Errorf("unsafe known_hosts dir: %w", err)
	}
	return nil
}
