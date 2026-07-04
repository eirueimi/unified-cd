package main

import (
	"context"
	"os"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

func main() {
	ctx := context.Background()
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
