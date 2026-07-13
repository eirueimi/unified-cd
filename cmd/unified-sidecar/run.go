package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// stringSlice collects repeated flag values (e.g. --restore-key a --restore-key b).
type stringSlice []string

func (s *stringSlice) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// storeProvider lazily builds (or obtains) the ObjectStore used by cache and
// artifact subcommands. It is invoked only when a subcommand actually needs
// the store — "idle" never calls it, so the sidecar can stay resident even
// when no S3 configuration is present (degraded mode).
type storeProvider func(context.Context) (objectstore.ObjectStore, error)

// run dispatches the sidecar subcommands. The store is obtained lazily via
// newStore, only for cache/artifact subcommands; "idle" ignores it entirely.
// Cache operations are best-effort (always exit 0 once the store is
// available); restore additionally emits a `UCD_CACHE_RESULT=hit|miss`
// marker on stdout so the caller can distinguish a real hit from a miss,
// without affecting the exit code. Artifact operations exit non-zero on
// failure. If newStore fails (e.g. no S3 configuration in degraded mode),
// cache/artifact subcommands fail loudly with a clear message and a
// non-zero exit code.
func run(ctx context.Context, newStore storeProvider, args []string, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "idle" {
		<-ctx.Done()
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: unified-sidecar <cache|artifact> <subcommand> [flags]")
		return 2
	}
	group, sub, rest := args[0], args[1], args[2:]
	switch group {
	case "cache":
		store, err := newStore(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "cache requires S3 configuration (UNIFIED_S3_*): %v\n", err)
			return 1
		}
		return runCache(ctx, store, sub, rest, os.Stdout, stderr)
	case "artifact":
		store, err := newStore(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "artifact requires S3 configuration (UNIFIED_S3_*): %v\n", err)
			return 1
		}
		return runArtifact(ctx, store, sub, rest, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command group %q\n", group)
		return 2
	}
}

func runCache(ctx context.Context, store objectstore.ObjectStore, sub string, args []string, stdout, stderr io.Writer) int {
	switch sub {
	case "restore":
		fs := flag.NewFlagSet("cache restore", flag.ContinueOnError)
		fs.SetOutput(stderr)
		key := fs.String("key", "", "cache key")
		path := fs.String("path", "", "destination path")
		var restoreKeys stringSlice
		fs.Var(&restoreKeys, "restore-key", "fallback restore key prefix (repeatable)")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		hit, err := cache.Restore(ctx, store, *path, *key, restoreKeys)
		if err != nil && !errors.Is(err, cache.ErrCacheMiss) {
			fmt.Fprintf(stderr, "cache restore error (ignored): %v\n", err)
			// error path: leave no marker → CacheRestore keeps its lenient default (hit=true)
		} else if hit {
			fmt.Fprintf(stderr, "cache hit: %s\n", *key)
			fmt.Fprintln(stdout, "UCD_CACHE_RESULT=hit")
		} else {
			fmt.Fprintf(stderr, "cache miss: %s\n", *key)
			fmt.Fprintln(stdout, "UCD_CACHE_RESULT=miss")
		}
		return 0 // best-effort: never fail the step
	case "save":
		fs := flag.NewFlagSet("cache save", flag.ContinueOnError)
		fs.SetOutput(stderr)
		key := fs.String("key", "", "cache key")
		path := fs.String("path", "", "source path")
		ttlDays := fs.Int("ttl-days", 30, "TTL in days")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if err := cache.Save(ctx, store, *path, *key, *ttlDays); err != nil {
			fmt.Fprintf(stderr, "cache save error (ignored): %v\n", err)
		} else {
			fmt.Fprintf(stderr, "cache saved: %s\n", *key)
		}
		return 0 // best-effort
	default:
		fmt.Fprintf(stderr, "unknown cache subcommand %q\n", sub)
		return 2
	}
}

func runArtifact(ctx context.Context, store objectstore.ObjectStore, sub string, args []string, stderr io.Writer) int {
	switch sub {
	case "upload":
		fs := flag.NewFlagSet("artifact upload", flag.ContinueOnError)
		fs.SetOutput(stderr)
		runID := fs.String("run", "", "run ID")
		name := fs.String("name", "", "artifact name")
		path := fs.String("path", "", "source path")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if err := artifact.Upload(ctx, store, *runID, *name, *path); err != nil {
			fmt.Fprintf(stderr, "artifact upload failed: %v\n", err)
			return 1
		}
		return 0
	case "download":
		fs := flag.NewFlagSet("artifact download", flag.ContinueOnError)
		fs.SetOutput(stderr)
		runID := fs.String("run", "", "run ID")
		name := fs.String("name", "", "artifact name")
		dest := fs.String("dest", ".", "destination directory")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if err := artifact.Download(ctx, store, *runID, *name, *dest); err != nil {
			fmt.Fprintf(stderr, "artifact download failed: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown artifact subcommand %q\n", sub)
		return 2
	}
}
