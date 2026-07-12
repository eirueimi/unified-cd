package k8sagent

import (
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	uexec "k8s.io/client-go/util/exec"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/scheme"
)

// Executor executes steps inside a Kubernetes Pod via the exec API.
type Executor struct {
	client    kubernetes.Interface
	restCfg   *rest.Config
	namespace string
}

// NewExecutor creates a new Executor.
func NewExecutor(client kubernetes.Interface, restCfg *rest.Config, namespace string) *Executor {
	return &Executor{client: client, restCfg: restCfg, namespace: namespace}
}

// ExecStep runs a script inside the specified container of a Pod, streaming
// stdout/stderr. If container is empty, the "job" container is used. shell is
// the effective interpreter argv (api.ClaimStep.Shell / the hook's resolved
// shell — nil/empty means "apply the shim default", ["/.ucd/ucd-sh","-c"],
// injected into the pod at /.ucd by podbuilder.go's injectUcdShim); a non-nil
// value is used verbatim, e.g. ["bash","-lc"]. env is a list of "KEY=VALUE"
// pairs applied to the exec'd process; when non-empty the command is wrapped
// as `env K=V... <shell...> script` so values reach the script without any
// shell-quoting/string-concatenation (Kubernetes exec has no native env
// option — see buildEnvShellCommand).
// Returns (exitCode, nil) when the command exits (including non-zero exit codes).
// Returns (1, err) for infrastructure errors (network, protocol, etc.).
func (e *Executor) ExecStep(ctx context.Context, podName, container, script string, shell []string, env []string, stdout, stderr io.Writer) (int, error) {
	return e.execArgv(ctx, podName, container, buildEnvShellCommand(shell, script, env), stdout, stderr)
}

// ExecStepArgv runs argv directly (no shell) inside the specified container,
// streaming stdout/stderr. If container is empty, the "job" container is used.
// Used for the unified-sidecar binary so values are never shell-interpolated.
func (e *Executor) ExecStepArgv(ctx context.Context, podName, container string, argv []string, stdout, stderr io.Writer) (int, error) {
	return e.execArgv(ctx, podName, container, argv, stdout, stderr)
}

// execArgv is the shared implementation for ExecStep and ExecStepArgv. It runs
// cmd directly inside the specified container via the Kubernetes exec API,
// streaming stdout/stderr. If container is empty, the "job" container is used.
// Returns (exitCode, nil) when the command exits (including non-zero exit codes).
// Returns (1, err) for infrastructure errors (network, protocol, etc.).
func (e *Executor) execArgv(ctx context.Context, podName, container string, cmd []string, stdout, stderr io.Writer) (int, error) {
	if container == "" {
		container = "job"
	}
	req := e.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(e.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.restCfg, "POST", req.URL())
	if err != nil {
		return -1, fmt.Errorf("failed to create exec executor: %w", err)
	}

	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: stdout,
		Stderr: stderr,
	})
	if streamErr != nil {
		var codeErr uexec.CodeExitError
		if errors.As(streamErr, &codeErr) {
			return codeErr.Code, nil
		}
		return 1, streamErr
	}
	return 0, nil
}

// ucdShimPath is the reserved path (see docs/superpowers/specs/2026-07-12-
// step-shell-shim-design.md Component 3) the ucd-sh shim is installed at by
// podbuilder.go's injectUcdShim init container, shared with executor.go so
// the default shell argv below stays in sync with the injection path.
const ucdShimPath = "/.ucd/ucd-sh"

// ucdDefaultShell returns a fresh copy of the shim's default interpreter
// argv, ["/.ucd/ucd-sh", "-c"] — the system default applied whenever a
// step/hook has no effective shell: (nil/empty ClaimStep.Shell). A function
// (rather than a package-level slice) so callers never share/mutate the same
// backing array by appending to it.
func ucdDefaultShell() []string {
	return []string{ucdShimPath, "-c"}
}

// buildShellCommand converts an interpreter argv and a script string into the
// exec argv: shell (or the shim default when shell is nil/empty) followed by
// the script as its final element, verbatim — never re-parsed or quoted (see
// Component 1 of the step-shell-shim design spec).
func buildShellCommand(shell []string, script string) []string {
	if len(shell) == 0 {
		shell = ucdDefaultShell()
	}
	cmd := make([]string, 0, len(shell)+1)
	cmd = append(cmd, shell...)
	cmd = append(cmd, script)
	return cmd
}

// buildEnvShellCommand converts an interpreter argv, a script string, and a
// list of "KEY=VALUE" pairs into an argv that applies the env via the `env`
// binary before invoking the shell, e.g.
// ["env", "FOO=bar", "/.ucd/ucd-sh", "-c", script]. Using `env` (rather than
// string-concatenating `export FOO=bar;` onto the script) avoids
// shell-quoting pitfalls entirely: values are passed as discrete argv
// elements, never re-parsed by a shell (see TODO #30's known quoting-bug
// class). With no env pairs this degrades to the plain buildShellCommand.
func buildEnvShellCommand(shell []string, script string, env []string) []string {
	if len(env) == 0 {
		return buildShellCommand(shell, script)
	}
	cmd := make([]string, 0, len(env)+len(shell)+2)
	cmd = append(cmd, "env")
	cmd = append(cmd, env...)
	cmd = append(cmd, buildShellCommand(shell, script)...)
	return cmd
}
