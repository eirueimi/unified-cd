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

// ExecStep runs a script inside the specified container of a Pod, streaming stdout/stderr.
// If container is empty, the "job" container is used. env is a list of
// "KEY=VALUE" pairs applied to the exec'd process; when non-empty the command
// is wrapped as `env K=V... bash -lc script` so values reach the script
// without any shell-quoting/string-concatenation (Kubernetes exec has no
// native env option — see buildEnvShellCommand).
// Returns (exitCode, nil) when the command exits (including non-zero exit codes).
// Returns (1, err) for infrastructure errors (network, protocol, etc.).
func (e *Executor) ExecStep(ctx context.Context, podName, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return e.execArgv(ctx, podName, container, buildEnvShellCommand(script, env), stdout, stderr)
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

// buildShellCommand converts a script string into a bash command array.
func buildShellCommand(script string) []string {
	return []string{"bash", "-lc", script}
}

// buildEnvShellCommand converts a script string and a list of "KEY=VALUE"
// pairs into an argv that applies the env via the `env` binary before
// invoking bash, e.g. ["env", "FOO=bar", "bash", "-lc", script]. Using `env`
// (rather than string-concatenating `export FOO=bar;` onto the script) avoids
// shell-quoting pitfalls entirely: values are passed as discrete argv
// elements, never re-parsed by a shell (see TODO #30's known quoting-bug
// class). With no env pairs this degrades to the plain buildShellCommand.
func buildEnvShellCommand(script string, env []string) []string {
	if len(env) == 0 {
		return buildShellCommand(script)
	}
	cmd := make([]string, 0, len(env)+3)
	cmd = append(cmd, "env")
	cmd = append(cmd, env...)
	cmd = append(cmd, "bash", "-lc", script)
	return cmd
}
