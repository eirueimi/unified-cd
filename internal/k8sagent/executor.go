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
// If container is empty, the "job" container is used.
// Returns (exitCode, nil) when the command exits (including non-zero exit codes).
// Returns (1, err) for infrastructure errors (network, protocol, etc.).
func (e *Executor) ExecStep(ctx context.Context, podName, container, script string, stdout, stderr io.Writer) (int, error) {
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
			Command:   buildShellCommand(script),
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
