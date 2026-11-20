package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/eirueimi/unified-cd/internal/objtransfer"
)

// The k8s agent sets both on the sidecar container: the controller's base URL
// and the path of the projected ServiceAccount token (audience
// api.KubernetesStoreCredentialAudience). Neither is a store credential: the
// sidecar asks the controller for presigned URLs and holds no store secret.
const (
	envControllerURL  = "UNIFIED_CONTROLLER_URL"
	envPodTokenFile   = "UNIFIED_POD_TOKEN_FILE"
	missingEnvMessage = envControllerURL + " and " + envPodTokenFile + " (set by the k8s agent)"
)

// errNoControllerEnv means the sidecar was started without the settings the
// k8s agent provides.
var errNoControllerEnv = errors.New("missing " + missingEnvMessage)

// newClient builds the presign client for the Pod's own run. The token file is
// re-read on every request, so a rotation of the projected token is picked up.
// The token is never logged.
func newClient(runID string) (*objtransfer.Client, error) {
	baseURL := strings.TrimSpace(os.Getenv(envControllerURL))
	tokenFile := os.Getenv(envPodTokenFile)
	if baseURL == "" || tokenFile == "" {
		return nil, errNoControllerEnv
	}
	return &objtransfer.Client{
		BaseURL: baseURL,
		RunID:   runID,
		Auth: func(_ context.Context, r *http.Request) error {
			tok, err := os.ReadFile(tokenFile)
			if err != nil {
				return fmt.Errorf("read pod token file: %w", err)
			}
			r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
			return nil
		},
		// os.TempDir() honors TMPDIR, which the k8s agent points at a dedicated
		// emptyDir mounted into the sidecar (see internal/k8sagent/podbuilder.go),
		// so upload part files never land in the container's writable layer.
		TempDir: os.TempDir(),
	}, nil
}

func main() {
	ctx, stop := rootContext()
	defer stop()
	// The client is built lazily: idle must stay resident even when the
	// controller settings are absent. Only cache/artifact subcommands need it,
	// so they call newClient themselves and fail loudly if it errors.
	os.Exit(run(ctx, newClient, os.Args[1:], os.Stderr))
}

// rootContext returns the process context, cancelled on SIGINT/SIGTERM.
//
// It must have a non-nil Done() channel: the "idle" command blocks on
// <-ctx.Done() to keep the artifact sidecar resident, and a nil-channel receive
// (context.Background().Done() is nil) as the only runnable goroutine makes the
// Go runtime kill the process with "all goroutines are asleep - deadlock!".
// signal.NotifyContext both gives a real Done channel and keeps a signal
// goroutine runnable so the deadlock detector never fires; it also lets the
// sidecar exit cleanly when its pod is terminated.
func rootContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
