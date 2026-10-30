package shim

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
)

// defaultExecPath is the search path used when the resulting environment has
// no PATH at all, matching the libc execvp default coreutils env relies on.
const defaultExecPath = "/usr/local/bin:/usr/bin:/bin"

// RunEnv implements `ucd-sh env NAME=VALUE... -- <argv>...`: it applies the
// assignments on top of base (an existing name is overridden), resolves
// argv[0] through the PATH of the RESULTING environment (as coreutils env
// does), runs it with the given stdio, and returns its exit code. It exists so
// a Kubernetes exec can set a step's environment without requiring an `env`
// binary in the target image: the ucd-sh shim is already mounted at /.ucd in
// every job-pod container.
//
// Exit codes follow env(1): 2 for a usage error, 127 when the command cannot
// be found, 126 when it is found but cannot be executed, otherwise the
// command's own status (128+N when it dies from signal N).
func RunEnv(args []string, base []string, stdin io.Reader, stdout, stderr io.Writer) int {
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep == len(args)-1 {
		fmt.Fprintln(stderr, "[ucd-sh] env: usage: ucd-sh env [NAME=VALUE]... -- <command> [args]...")
		return 2
	}
	assigns, argv := args[:sep], args[sep+1:]

	env := append([]string(nil), base...)
	for _, a := range assigns {
		name, _, ok := strings.Cut(a, "=")
		if !ok || name == "" {
			fmt.Fprintf(stderr, "[ucd-sh] env: invalid assignment %q (want NAME=VALUE)\n", a)
			return 2
		}
		env = setEnv(env, name, a)
	}

	path, err := lookPathIn(argv[0], env)
	if err != nil {
		fmt.Fprintf(stderr, "[ucd-sh] env: '%s': %s\n", argv[0], errText(err))
		return 127
	}

	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "[ucd-sh] env: '%s': %s\n", argv[0], errText(err))
		if errors.Is(err, fs.ErrNotExist) {
			return 127
		}
		return 126
	}

	// Forward termination signals so the command sees them as if it had been
	// exec'd directly.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, forwardedSignals()...)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()

	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitCodeOf(ee)
		}
		fmt.Fprintf(stderr, "[ucd-sh] env: %v\n", err)
		return 1
	}
	return 0
}

func errText(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "No such file or directory"
	case errors.Is(err, fs.ErrPermission):
		return "Permission denied"
	}
	return err.Error()
}

// setEnv replaces the entry named name in env with entry (NAME=VALUE), or
// appends it, dropping any duplicates.
func setEnv(env []string, name, entry string) []string {
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if k == name {
			if !replaced {
				out = append(out, entry)
				replaced = true
			}
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, entry)
	}
	return out
}

// lookPathIn resolves file like execvp, but against the PATH in env rather
// than this process's own. A name containing a path separator is used as is.
func lookPathIn(file string, env []string) (string, error) {
	if strings.ContainsRune(file, '/') || (runtime.GOOS == "windows" && strings.ContainsRune(file, '\\')) {
		return file, checkExec(file)
	}
	pathVar := defaultExecPath
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			pathVar = v
		}
	}
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = []string{"", ".exe", ".cmd", ".bat"}
	}
	for _, dir := range filepath.SplitList(pathVar) {
		if dir == "" {
			dir = "."
		}
		for _, ext := range exts {
			p := filepath.Join(dir, file+ext)
			if checkExec(p) == nil {
				return p, nil
			}
		}
	}
	return "", fs.ErrNotExist
}

func checkExec(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st.IsDir() || (runtime.GOOS != "windows" && st.Mode()&0o111 == 0) {
		return fs.ErrPermission
	}
	return nil
}
