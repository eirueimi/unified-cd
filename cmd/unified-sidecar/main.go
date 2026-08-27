package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// UNIFIED_S3_BROKER_URL / UNIFIED_S3_BROKER_TOKEN_FILE select the §5.6
// controller-brokered credential path (internal/k8sagent's podbuilder.go
// sets both when SidecarS3SecretMode is "broker"). See
// docs/superpowers/specs/2026-08-26-sidecar-credential-delivery-design.md
// §5.6.
const (
	envBrokerURL       = "UNIFIED_S3_BROKER_URL"
	envBrokerTokenFile = "UNIFIED_S3_BROKER_TOKEN_FILE"
)

// s3ConfigFromEnv resolves the sidecar's object-store configuration, broker
// first. This precedence lives here rather than inside
// objectstore.S3ConfigFromEnv itself because the broker path needs a
// context and makes a network call to learn Endpoint/Bucket (see
// objectstore.BrokerConfig's doc comment on why that can't be deferred the
// way the file/static providers' lookups are), while S3ConfigFromEnv
// deliberately stays synchronous and local-only — every one of its existing
// callers and tests keeps working exactly as today when the broker env vars
// are unset, which is every deployment before this mode existed.
//
// Both broker env vars are required together: a deployment that sets one
// without the other has a configuration bug, not a "fall through to the
// next mode" situation — silently falling back to file/static credentials
// when an operator meant to opt into the broker would serve the WRONG
// credential shape without any error at all.
func s3ConfigFromEnv(ctx context.Context) (objectstore.S3Config, error) {
	brokerURL := os.Getenv(envBrokerURL)
	tokenFile := os.Getenv(envBrokerTokenFile)
	switch {
	case brokerURL != "" && tokenFile != "":
		return objectstore.BrokerConfig(ctx, brokerURL, tokenFile)
	case brokerURL != "" || tokenFile != "":
		return objectstore.S3Config{}, fmt.Errorf("%s and %s must both be set to use the store-credential broker (got %s=%q, %s=%q)", envBrokerURL, envBrokerTokenFile, envBrokerURL, brokerURL, envBrokerTokenFile, tokenFile)
	default:
		return objectstore.S3ConfigFromEnv()
	}
}

func main() {
	ctx, stop := rootContext()
	defer stop()
	// Build the S3 store lazily: idle (degraded mode, no S3 EnvFrom) must
	// stay resident even when S3 configuration is absent. Only cache/artifact
	// subcommands actually need the store, so they invoke this provider
	// themselves and fail loudly if it errors.
	prov := func(ctx context.Context) (objectstore.ObjectStore, error) {
		cfg, err := s3ConfigFromEnv(ctx)
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
