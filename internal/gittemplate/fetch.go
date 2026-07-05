package gittemplate

import (
	"context"
	"errors"
	"fmt"
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
type Fetcher struct{}

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
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return nil
	}

	if err := run("init"); err != nil {
		return nil, err
	}
	if err := run("fetch", "--depth=1", repoURL, ref); err != nil {
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
	keyDir, err := os.MkdirTemp("", "gitkey-")
	if err != nil {
		return nil, fmt.Errorf("create ssh key dir: %w", err)
	}
	if err := os.Chmod(keyDir, 0o700); err != nil {
		os.RemoveAll(keyDir)
		return nil, fmt.Errorf("chmod ssh key dir: %w", err)
	}
	defer os.RemoveAll(keyDir)

	keyPath := keyDir + "/id"
	if err := os.WriteFile(keyPath, []byte(sshKey), 0o600); err != nil {
		return nil, fmt.Errorf("write ssh key: %w", err)
	}

	sshCmd := fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null", keyPath)
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
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return nil
	}

	if err := run("init"); err != nil {
		return nil, err
	}
	if err := run("fetch", "--depth=1", repoURL, uri.Ref); err != nil {
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
	var env []string
	if sshKey != "" {
		keyDir, cleanup, err := writeTempSSHKey(sshKey)
		if err != nil {
			return "", err
		}
		defer cleanup()
		sshCmd := fmt.Sprintf("ssh -i %s/id -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null", keyDir)
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
	cmd := exec.CommandContext(ctx, "git", "ls-remote", repoURL, ref)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-remote %s %s: %w", repoURL, ref, gitErr(err))
	}
	return parseLsRemoteOutput(string(out), ref)
}

// FetchDir retrieves all *.yaml / *.yml files under the specified path in the repository.
func (f *Fetcher) FetchDir(ctx context.Context, repoURL, ref, path, token, sshKey string) (map[string][]byte, error) {
	var extraEnv []string
	if sshKey != "" {
		keyDir, cleanup, err := writeTempSSHKey(sshKey)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		sshCmd := fmt.Sprintf("ssh -i %s/id -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null", keyDir)
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
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return nil
	}

	if err := runGit("init"); err != nil {
		return nil, err
	}
	if err := runGit("fetch", "--depth=1", repoURL, ref); err != nil {
		return nil, fmt.Errorf("fetch %s@%s: %w", repoURL, ref, err)
	}

	treePath := strings.TrimSuffix(path, "/")
	lsArgs := []string{"ls-tree", "-r", "--name-only", "FETCH_HEAD"}
	if treePath != "" {
		lsArgs = append(lsArgs, treePath)
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

// writeTempSSHKey writes an SSH private key to a temporary directory and returns a cleanup function.
func writeTempSSHKey(sshKey string) (string, func(), error) {
	keyDir, err := os.MkdirTemp("", "gitkey-")
	if err != nil {
		return "", nil, fmt.Errorf("create ssh key dir: %w", err)
	}
	if err := os.Chmod(keyDir, 0o700); err != nil {
		os.RemoveAll(keyDir)
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(keyDir, "id"), []byte(sshKey), 0o600); err != nil {
		os.RemoveAll(keyDir)
		return "", nil, err
	}
	return keyDir, func() { os.RemoveAll(keyDir) }, nil
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
