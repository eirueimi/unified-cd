package shim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Run parses script as a shell program, sanitizes unsupported trap
// conditions (see SanitizeTraps), and executes it with the mvdan.cc/sh
// interpreter. It returns the script's exit code, mapped 1:1 from
// interp.ExitStatus.
//
// env follows os/exec conventions: a nil slice means "inherit the
// interpreter's default environment" (mvdan.cc/sh falls back to the host
// process's environment); a non-nil slice, including an empty one, is used
// verbatim as NAME=VALUE pairs.
//
// dir is the working directory; an empty string means "use the process's
// current directory".
//
// Sanitizer warnings are written to stderr, one per line, prefixed
// "[ucd-sh] ".
func Run(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer, env []string, dir string) (int, error) {
	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(script), "")
	if err != nil {
		return 2, fmt.Errorf("parse: %w", err)
	}

	warn := func(msg string) {
		fmt.Fprintf(stderr, "[ucd-sh] %s\n", msg)
	}
	SanitizeTraps(file, warn)

	opts := []interp.RunnerOption{
		interp.StdIO(stdin, stdout, stderr),
		interp.Dir(dir),
	}
	if env != nil {
		opts = append(opts, interp.Env(expand.ListEnviron(env...)))
	}

	runner, err := interp.New(opts...)
	if err != nil {
		return 1, fmt.Errorf("ucd-sh: %w", err)
	}

	runErr := runner.Run(ctx, file)
	if runErr == nil {
		return 0, nil
	}

	var status interp.ExitStatus
	if errors.As(runErr, &status) {
		return int(status), nil
	}

	// Not a plain exit status: most likely the run was cut short by context
	// cancellation (or an interpreter-internal error, e.g. an I/O handler
	// failure). Map cancellation to conventional shell exit codes so
	// callers see something sane instead of a bare error path; still
	// surface the underlying error for logging.
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return 124, ctxErr
		}
		return 130, ctxErr
	}

	return 1, runErr
}

// Install copies the currently running executable to dest (mode 0755),
// via a copy-to-temp-in-the-destination-directory-then-atomic-rename so
// that dest is never observed as a partially written file - even if dest
// already exists and is being replaced.
func Install(dest string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	src, err := os.Open(self)
	if err != nil {
		return fmt.Errorf("open %s: %w", self, err)
	}
	defer src.Close()

	destDir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(destDir, ".ucd-sh-install-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", destDir, err)
	}
	tmpPath := tmp.Name()
	installed := false
	defer func() {
		if !installed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return fmt.Errorf("copy to %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpPath, dest, err)
	}
	installed = true
	return nil
}
