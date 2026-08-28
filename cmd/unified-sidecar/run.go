package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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
//
// runID is the Pod's own executing run — extracted from the raw args by
// extractBrokerRunID BEFORE any subcommand-specific flag.FlagSet parses
// them (see run's doc comment for why that ordering is exactly the point).
// A provider that ignores runID (every branch except the broker) is
// unaffected; see s3ConfigFromEnv in main.go.
type storeProvider func(ctx context.Context, runID string) (objectstore.ObjectStore, error)

// extractBrokerRunID scans a subcommand's raw args for --broker-run-id
// (accepting "--broker-run-id VALUE", "--broker-run-id=VALUE", and their
// single-dash spellings), independent of the subcommand's own
// flag.FlagSet — which is parsed later, deeper in runCache/runArtifact, by
// which point the store (and therefore any store-credentials broker
// request) has already been built. That ordering is why this exists as a
// raw pre-scan rather than a value read off the subcommand's FlagSet: run()
// must know the Pod's run BEFORE calling newStore, and the FlagSet that
// would otherwise parse it does not exist until after that call.
//
// Deliberately its OWN flag, not artifact upload/download's existing --run:
// that flag names which run's ARTIFACT to read or write, which a call: step
// can point at a DIFFERENT run than the one this Pod is executing (see
// api.DownloadArtifactStep) — reusing it here would send the broker the
// wrong run and have it reject a legitimate cross-run download as a
// binding mismatch (see objectstore.BrokerConfig's doc comment). cache
// subcommands have no existing --run concept at all to conflict with, but
// the same dedicated flag is used there too, for one consistent story
// across every subcommand.
//
// This is a raw, best-effort scan, not validation: every subcommand's own
// flag.FlagSet — parsed afterward, inside runCache/runArtifact — is what
// actually enforces correct usage of the flags it consumes; --broker-run-id
// itself is declared (but unused) in each of their FlagSets purely so
// Parse does not reject it as unrecognized. A missing or malformed value
// here only means the broker's StoreCredentialsRequest.RunID goes out
// empty, which degrades to the "unknown binding" case on the controller
// side (see api.StoreCredentialsRequest.RunID's doc comment) rather than a
// hard failure.
func extractBrokerRunID(args []string) string {
	const flagName = "broker-run-id"
	for i, a := range args {
		switch {
		case a == "--"+flagName || a == "-"+flagName:
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		default:
			for _, prefix := range []string{"--" + flagName + "=", "-" + flagName + "="} {
				if v, ok := strings.CutPrefix(a, prefix); ok {
					return v
				}
			}
		}
	}
	return ""
}

// run dispatches the sidecar subcommands. The store is obtained lazily via
// newStore, only for cache/artifact subcommands; "idle" ignores it entirely.
// Cache operations are best-effort (always exit 0 once the store is
// available); restore additionally emits a `UCD_CACHE_RESULT=hit|miss`
// marker on stdout so the caller can distinguish a real hit from a miss,
// without affecting the exit code. Artifact operations exit non-zero on
// failure. If newStore fails (e.g. no S3 configuration in degraded mode),
// cache/artifact subcommands fail loudly with a clear message and a
// non-zero exit code.
//
// Two gaps in "cache always exits 0" are load-bearing for the caller, because
// between them they are every way a cache can be inert:
//
//   - The newStore failure above happens BEFORE runCache is entered, so a
//     cache subcommand does exit non-zero when there is no S3 configuration.
//     This is the common production case (no sidecarS3SecretName), and it is
//     why k8sBackend.CacheRestore must check the exit code rather than trust
//     the "always 0" half of this contract.
//   - `cache restore`'s swallowed-error path exits 0 and emits NO marker.
//
// So a caller may see any of: non-zero exit, exit 0 with `hit`, exit 0 with
// `miss`, or exit 0 with no marker at all. Only the second is a hit.
func run(ctx context.Context, newStore storeProvider, args []string, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "idle" {
		<-ctx.Done()
		return 0
	}
	// `version` needs no store and no arguments: it is how an operator checks
	// that a running sidecar container matches its k8s-agent (docs/operator-manual/operations.md
	// requires the two to be upgraded in lockstep).
	if isVersionCommand(args) {
		fmt.Fprintln(os.Stdout, buildVersion())
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: unified-sidecar <version|idle|cache|artifact> [subcommand] [flags]")
		return 2
	}
	group, sub, rest := args[0], args[1], args[2:]
	switch group {
	case "cache":
		store, err := newStore(ctx, extractBrokerRunID(rest))
		if err != nil {
			fmt.Fprintf(stderr, "cache requires S3 configuration (UNIFIED_S3_*): %v\n", err)
			return 1
		}
		return runCache(ctx, store, sub, rest, os.Stdout, stderr)
	case "artifact":
		store, err := newStore(ctx, extractBrokerRunID(rest))
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
		job := fs.String("job", "", "qualified job name owning this cache entry")
		var restoreKeys stringSlice
		fs.Var(&restoreKeys, "restore-key", "fallback restore key prefix (repeatable)")
		// Declared so Parse accepts it; the value itself is already consumed
		// by run()'s extractBrokerRunID pre-scan, before this FlagSet ever
		// runs — see storeProvider's and extractBrokerRunID's doc comments.
		fs.String("broker-run-id", "", "the Pod's own executing run, used only to scope the store-credentials broker request")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *job == "" {
			fmt.Fprintln(stderr, "cache restore: --job is required")
			return 2
		}
		hit, err := cache.Restore(ctx, store, *job, *path, *key, restoreKeys)
		if err != nil && !errors.Is(err, cache.ErrCacheMiss) {
			fmt.Fprintf(stderr, "cache restore error (ignored): %v\n", err)
			// This path deliberately exits 0 (cache never fails a step) and
			// emits NO marker. A marker-less exit-0 restore therefore means
			// "the restore errored and nothing came back" — k8sBackend.
			// CacheRestore reads the absence as cacheResultUnknown and reports
			// "not restored", never a hit. Do not add a marker here without
			// changing that reader: `hit` is false in this branch anyway.
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
		job := fs.String("job", "", "qualified job name owning this cache entry")
		ttlDays := fs.Int("ttl-days", 30, "TTL in days")
		fs.String("broker-run-id", "", "the Pod's own executing run, used only to scope the store-credentials broker request")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *job == "" {
			fmt.Fprintln(stderr, "cache save: --job is required")
			return 2
		}
		if err := cache.Save(ctx, store, *job, *path, *key, *ttlDays); err != nil {
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
		// --run is the artifact's OWN target run (the object-store key,
		// artifacts/{run}/{name}.tar.gz) — deliberately NOT the same value
		// as --broker-run-id, which may differ for a cross-run call (see
		// extractBrokerRunID's doc comment). Declared here so Parse accepts
		// it; --broker-run-id's value was already consumed before this
		// FlagSet ever ran.
		runID := fs.String("run", "", "run ID")
		name := fs.String("name", "", "artifact name")
		path := fs.String("path", "", "source path")
		fs.String("broker-run-id", "", "the Pod's own executing run, used only to scope the store-credentials broker request")
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
		// See the "upload" case above: --run here names the artifact's
		// SOURCE run, which a cross-run call: download deliberately points
		// at a run other than the one this Pod executes.
		runID := fs.String("run", "", "run ID")
		name := fs.String("name", "", "artifact name")
		dest := fs.String("dest", ".", "destination directory")
		fs.String("broker-run-id", "", "the Pod's own executing run, used only to scope the store-credentials broker request")
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
