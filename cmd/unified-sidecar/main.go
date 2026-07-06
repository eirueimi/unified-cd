package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

func main() {
	ctx, stop := rootContext()
	defer stop()
	// Build the S3 store lazily: idle (degraded mode, no S3 EnvFrom) must
	// stay resident even when S3 configuration is absent. Only cache/artifact
	// subcommands actually need the store, so they invoke this provider
	// themselves and fail loudly if it errors.
	prov := func(ctx context.Context) (objectstore.ObjectStore, error) {
		cfg, err := objectstore.S3ConfigFromEnv()
		if err != nil {
			return nil, err
		}
		return objectstore.NewS3ObjectStore(ctx, cfg)
	}
	os.Exit(run(ctx, prov, os.Args[1:], os.Stderr))
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
