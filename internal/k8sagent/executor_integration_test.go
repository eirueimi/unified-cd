//go:build k8s

package k8sagent

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecutor_ExecStep_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	ns := newTestNamespace(t, client)
	pm := NewPodManager(client, ns, testImage)
	podName := podReadyOrSkip(t, pm, uniqueRunID("exec"))

	exec := NewExecutor(client, restCfg, ns)
	var stdout bytes.Buffer
	ec, err := exec.ExecStep(ctx, podName, "job", "echo hello-from-k8s", nil, nil, &stdout, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 0, ec)
	assert.Contains(t, stdout.String(), "hello-from-k8s")
}

func TestExecutor_ExecStep_NonZeroExitCode_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	ns := newTestNamespace(t, client)
	pm := NewPodManager(client, ns, testImage)
	podName := podReadyOrSkip(t, pm, uniqueRunID("exitcode"))

	exec := NewExecutor(client, restCfg, ns)
	ec, err := exec.ExecStep(ctx, podName, "job", "exit 1", nil, nil, io.Discard, io.Discard)
	assert.NoError(t, err, "non-zero exit code should not be returned as an error")
	assert.Equal(t, 1, ec)
}

func TestExecutor_ExecStep_MultiLine_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	ns := newTestNamespace(t, client)
	pm := NewPodManager(client, ns, testImage)
	podName := podReadyOrSkip(t, pm, uniqueRunID("multiline"))

	exec := NewExecutor(client, restCfg, ns)
	var stdout bytes.Buffer
	ec, err := exec.ExecStep(ctx, podName, "job", "echo line1 && echo line2", nil, nil, &stdout, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 0, ec)
	assert.Contains(t, stdout.String(), "line1")
	assert.Contains(t, stdout.String(), "line2")
}
