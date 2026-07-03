package main

import (
	"context"
	"fmt"
	"os"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

func main() {
	ctx := context.Background()
	cfg, err := objectstore.S3ConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	store, err := objectstore.NewS3ObjectStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3 store: %v\n", err)
		os.Exit(2)
	}
	os.Exit(run(ctx, store, os.Args[1:], os.Stderr))
}
