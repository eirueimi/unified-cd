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
	"github.com/eirueimi/unified-cd/internal/objtransfer"
)

// stringSlice collects repeated flag values (e.g. --restore-key a --restore-key b).
type stringSlice []string

func (s *stringSlice) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// clientProvider lazily builds the presign client used by cache and artifact
// subcommands. It is invoked only when a subcommand actually needs it: "idle"
// never calls it, so the sidecar stays resident even without controller
// settings (degraded mode).
//
// runID is the Pod's own executing run, taken from --pod-run by extractPodRun
// BEFORE any subcommand-specific flag.FlagSet parses the args, because the
// client (and its presign path /api/v1/runs/{runID}/objects/presign) must
// exist before the FlagSet does.
type clientProvider func(runID string) (*objtransfer.Client, error)

// extractPodRun scans a subcommand's raw args for --pod-run (accepting
// "--pod-run VALUE", "--pod-run=VALUE", and their single-dash spellings),
// independent of the subcommand's own flag.FlagSet.
//
// It is deliberately its OWN flag, not artifact upload/download's --run: that
// flag names which run's ARTIFACT to read or write, which a call: step can
// point at a DIFFERENT run than the one this Pod executes. The presign path
// names the Pod's own run (the one the controller binds the Pod to); a
// cross-run download is sent as sourceRunId.
//
// This is a raw, best-effort scan, not validation: every subcommand's own
// FlagSet declares --pod-run so Parse accepts it. A missing value yields "".
func extractPodRun(args []string) string {
	const flagName = "pod-run"
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

// clientError words a newClient failure for the subcommand group.
func clientError(group string, err error) string {
	if errors.Is(err, errNoControllerEnv) {
		return group + " requires " + missingEnvMessage
	}
	return fmt.Sprintf("%s: %v", group, err)
}

// run dispatches the sidecar subcommands. The presign client is obtained lazily via
// newClient, only for cache/artifact subcommands; "idle" ignores it entirely.
// Cache operations are best-effort (always exit 0 once the client is
// available); restore additionally emits a `UCD_CACHE_RESULT=hit|miss`
// marker on stdout so the caller can distinguish a real hit from a miss,
// without affecting the exit code. Artifact operations exit non-zero on
// failure. If newClient fails (e.g. no controller settings in degraded mode),
// cache/artifact subcommands fail loudly with a clear message and a
// non-zero exit code.
//
// Two gaps in "cache always exits 0" are load-bearing for the caller, because
// between them they are every way a cache can be inert:
//
//   - The newClient failure above happens BEFORE runCache is entered, so a
//     cache subcommand does exit non-zero when the controller settings are
//     missing. It is why k8sBackend.CacheRestore must check the exit code rather than trust
//     the "always 0" half of this contract.
//   - `cache restore`'s swallowed-error path exits 0 and emits NO marker.
//
// So a caller may see any of: non-zero exit, exit 0 with `hit`, exit 0 with
// `miss`, or exit 0 with no marker at all. Only the second is a hit.
func run(ctx context.Context, newClient clientProvider, args []string, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "idle" {
		<-ctx.Done()
		return 0
	}
	// `version` needs no client and no arguments: it is how an operator checks
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
		client, err := newClient(extractPodRun(rest))
		if err != nil {
			fmt.Fprintln(stderr, clientError("cache", err))
			return 1
		}
		return runCache(ctx, client.Caches(), sub, rest, os.Stdout, stderr)
	case "artifact":
		client, err := newClient(extractPodRun(rest))
		if err != nil {
			fmt.Fprintln(stderr, clientError("artifact", err))
			return 1
		}
		return runArtifact(ctx, client.Artifacts(), sub, rest, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command group %q\n", group)
		return 2
	}
}

func runCache(ctx context.Context, remote cache.Remote, sub string, args []string, stdout, stderr io.Writer) int {
	switch sub {
	case "restore":
		fs := flag.NewFlagSet("cache restore", flag.ContinueOnError)
		fs.SetOutput(stderr)
		key := fs.String("key", "", "cache key")
		root := fs.String("root", "", "directory every read and write is confined to (required): the Pod's workspace mount; a symlink or \"..\" that leads outside it is never followed")
		path := fs.String("path", "", "destination path")
		job := fs.String("job", "", "qualified job name (optional, used only in log lines; the controller reads the job from the run)")
		var restoreKeys stringSlice
		fs.Var(&restoreKeys, "restore-key", "fallback restore key prefix (repeatable)")
		var exclude stringSlice
		fs.Var(&exclude, "exclude", "path to leave alone, with everything below it: no archived entry is extracted there (repeatable)")
		// Declared so Parse accepts it; run() already consumed the value (see extractPodRun).
		fs.String("pod-run", "", "the Pod's own executing run, which scopes the controller's presign endpoint")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *root == "" {
			fmt.Fprintln(stderr, "cache restore: --root is required")
			return 2
		}
		hit, matchedKey, skippedLinks, err := cache.RestoreIn(ctx, remote, *root, *path, *key, restoreKeys, exclude)
		if err != nil && !errors.Is(err, cache.ErrCacheMiss) {
			fmt.Fprintf(stderr, "cache restore error (ignored)%s: %v\n", jobLabel(*job), err)
			// This path deliberately exits 0 (cache never fails a step) and
			// emits NO marker. A marker-less exit-0 restore therefore means
			// "the restore errored and nothing came back" — k8sBackend.
			// CacheRestore reads the absence as cacheResultUnknown and reports
			// "not restored", never a hit. Do not add a marker here without
			// changing that reader: `hit` is false in this branch anyway.
		} else if hit {
			fmt.Fprintf(stderr, "cache hit: %s%s\n", *key, matchedSuffix(*key, matchedKey))
			if skippedLinks > 0 {
				fmt.Fprintln(stderr, artifact.SkippedLinksWarning(skippedLinks))
			}
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
		root := fs.String("root", "", "directory every read and write is confined to (required): the Pod's workspace mount; a symlink or \"..\" that leads outside it is never followed")
		path := fs.String("path", "", "source path")
		job := fs.String("job", "", "qualified job name (optional, used only in log lines; the controller reads the job from the run)")
		ttlDays := fs.Int("ttl-days", 30, "TTL in days")
		var exclude stringSlice
		fs.Var(&exclude, "exclude", "path to leave out of the archive, with everything below it (repeatable)")
		fs.String("pod-run", "", "the Pod's own executing run, which scopes the controller's presign endpoint")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *root == "" {
			fmt.Fprintln(stderr, "cache save: --root is required")
			return 2
		}
		if err := cache.SaveIn(ctx, remote, *root, *path, *key, *ttlDays, exclude); err != nil {
			fmt.Fprintf(stderr, "cache save error (ignored)%s: %v\n", jobLabel(*job), err)
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
		// as --pod-run, which may differ for a cross-run call (see
		// extractPodRun's doc comment).
		runID := fs.String("run", "", "run ID")
		name := fs.String("name", "", "artifact name")
		root := fs.String("root", "", "directory every read and write is confined to (required): the Pod's workspace mount; a symlink or \"..\" that leads outside it is never followed")
		path := fs.String("path", "", "source path")
		var exclude stringSlice
		fs.Var(&exclude, "exclude", "path to leave out of the archive, with everything below it (repeatable)")
		fs.String("pod-run", "", "the Pod's own executing run, which scopes the controller's presign endpoint")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *root == "" {
			fmt.Fprintln(stderr, "artifact upload: --root is required")
			return 2
		}
		if err := artifact.UploadIn(ctx, store, *runID, *name, *root, *path, exclude); err != nil {
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
		root := fs.String("root", "", "directory every read and write is confined to (required): the Pod's workspace mount; a symlink or \"..\" that leads outside it is never followed")
		dest := fs.String("dest", ".", "destination directory")
		var exclude stringSlice
		fs.Var(&exclude, "exclude", "path to leave alone, with everything below it: no archived entry is extracted there (repeatable)")
		fs.String("pod-run", "", "the Pod's own executing run, which scopes the controller's presign endpoint")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *root == "" {
			fmt.Fprintln(stderr, "artifact download: --root is required")
			return 2
		}
		if err := artifact.DownloadIn(ctx, store, *runID, *name, *root, *dest, exclude); err != nil {
			fmt.Fprintf(stderr, "artifact download failed: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown artifact subcommand %q\n", sub)
		return 2
	}
}

// jobLabel words the optional --job value for a log line.
func jobLabel(job string) string {
	if job == "" {
		return ""
	}
	return " for job " + job
}

// matchedSuffix names the entry a restore-key fallback actually matched, so a
// hit on an older entry is visible in the step log.
func matchedSuffix(key, matched string) string {
	if matched == "" || matched == key {
		return ""
	}
	return " (matched " + matched + ")"
}
