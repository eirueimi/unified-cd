// Package srchash computes a platform-independent "source hash" for the
// ucd-sh shim: a single value that changes if and only if something able to
// change the compiled shim's behaviour has changed, without depending on the
// compiled bytes themselves. See internal/shim/embedded/embed.go's package
// doc for why byte-exact comparison of the committed binaries across build
// machines is a dead end for this artifact (file mode and non-reproducible
// compiler output, verified against three independent build environments).
//
// cmd/shimgen writes Compute's result to
// internal/shim/embedded/ucd-sh-source.sha256 every time it regenerates the
// committed shim binaries; internal/shim/embedded's test suite recomputes
// the hash from the CURRENT source tree and compares it against that
// recorded value, so a source edit nobody regenerated for is caught by
// `go test`, not left to be noticed by luck in code review.
package srchash

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ShimPackage is the import path cmd/shimgen cross-compiles to produce the
// committed ucd-sh-<arch> binaries. Its transitive dependency graph — not
// just the one file at this path — is what determines the shim's compiled
// behaviour, so Compute walks `go list -deps` from here rather than hashing
// cmd/ucd-sh alone; internal/shim (pause.go, run.go, sanitize.go — where
// most of the actual shim logic lives) is reached that way, and so would
// any other internal package cmd/ucd-sh starts importing in the future.
const ShimPackage = "github.com/eirueimi/unified-cd/cmd/ucd-sh"

// RecordedHashPath is where cmd/shimgen writes Compute's result and where
// internal/shim/embedded's freshness test reads it back from, relative to
// the module root.
var RecordedHashPath = filepath.Join("internal", "shim", "embedded", "ucd-sh-source.sha256")

// shimGOARCHes lists every GOARCH cmd/shimgen actually cross-compiles
// ShimPackage for (GOOS is always linux; see embed.go's package doc on why
// the target OS never varies). A GOARCH-conditioned source file — a
// `_arm64.go` suffix, a `//go:build amd64` tag, and so on — added for one
// arch and not the other must still be covered, so Compute unions the
// dependency graph across both GOARCHes instead of asking `go list` for
// just one and hoping the file set doesn't diverge.
var shimGOARCHes = []string{"amd64", "arm64"}

// moduleImportPrefix identifies which packages `go list -deps` reports are
// this repo's own source (hashed byte-for-byte below) versus a third-party
// dependency (already content-addressed by go.sum, so pinning its resolved
// "module@version" string is enough — see Compute's doc comment).
const moduleImportPrefix = "github.com/eirueimi/unified-cd"

// Compute returns the hex-encoded SHA-256 source hash for the ucd-sh shim,
// derived from moduleRoot (the directory containing this repo's go.mod).
//
// What goes into the hash, and why:
//
//   - Every .go file `go list` says is actually compiled into ShimPackage's
//     dependency graph for GOOS=linux, for each of shimGOARCHes, in a
//     package under this module (moduleImportPrefix). Deliberately NOT
//     "every .go file under cmd/ucd-sh and internal/shim": that would also
//     hash files a build tag excludes from the linux build (e.g.
//     internal/shim/pause_other.go, which is `//go:build !unix`), and
//     falsely demand regeneration for an edit that can never change the
//     committed bytes.
//   - The resolved "module@version" of every third-party package in that
//     same dependency graph (e.g. mvdan.cc/sh/v3@v3.13.1, the shell
//     interpreter cmd/ucd-sh is built on). A dependency bump changes the
//     shim's behaviour exactly like editing cmd/ucd-sh does, even though no
//     file under cmd/ucd-sh or internal/shim changed. Its content is
//     already integrity-checked by go.sum, so the resolved version string
//     is enough to pin it — hashing the dependency's own source would just
//     duplicate what go.sum already guarantees, for no extra safety.
//   - Standard-library packages are excluded entirely: they are pinned by
//     the `go` directive in go.mod, a change to which is already its own
//     highly visible diff. This guard exists to catch silent drift, and a
//     go.mod edit is the opposite of silent.
//
// File content is hashed after normalising CRLF line endings to LF, because
// this repo sets core.autocrlf=true (it is developed on Windows) while a
// Linux CI checkout typically does not: the exact same commit produces
// different on-disk bytes for the exact same Go source file depending on
// which platform checked it out. PR #153 shipped a freshness test that
// hashed raw file bytes read from the working tree and it failed on
// Windows CI for precisely this reason. Normalising first means Compute
// returns the identical value on Windows, macOS and Linux, from this
// repo's own checkout, always — the hash tracks source content, not the
// accident of which OS `git checkout` last ran on.
func Compute(moduleRoot string) (string, error) {
	localFiles := map[string]map[string]struct{}{} // importPath -> file name set
	localDir := map[string]string{}                 // importPath -> absolute package dir
	externalDeps := map[string]struct{}{}           // "module@version" set

	for _, arch := range shimGOARCHes {
		entries, err := listDeps(moduleRoot, arch)
		if err != nil {
			return "", fmt.Errorf("go list -deps %s (GOOS=linux GOARCH=%s): %w", ShimPackage, arch, err)
		}
		for _, e := range entries {
			if e.standard {
				continue
			}
			if strings.HasPrefix(e.importPath, moduleImportPrefix) {
				set := localFiles[e.importPath]
				if set == nil {
					set = map[string]struct{}{}
					localFiles[e.importPath] = set
				}
				for _, f := range e.files {
					set[f] = struct{}{}
				}
				localDir[e.importPath] = e.dir
				continue
			}
			if e.module != "" {
				externalDeps[e.module] = struct{}{}
			}
		}
	}

	// An empty result here means the `go list` query itself broke (wrong
	// working directory, wrong import prefix, `go` not on PATH producing a
	// silently-empty parse) — not that the shim genuinely has no source. Fail
	// loudly rather than returning a hash of nothing that would spuriously
	// "match" whatever happened to be recorded.
	if len(localFiles) == 0 {
		return "", fmt.Errorf("go list -deps found no %s-prefixed package in %s's dependency graph in %s; the query is broken, not the shim", moduleImportPrefix, ShimPackage, moduleRoot)
	}

	var manifest bytes.Buffer

	pkgs := make([]string, 0, len(localFiles))
	for p := range localFiles {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, pkg := range pkgs {
		fileSet := localFiles[pkg]
		files := make([]string, 0, len(fileSet))
		for f := range fileSet {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, f := range files {
			h, err := hashFile(filepath.Join(localDir[pkg], f))
			if err != nil {
				return "", fmt.Errorf("hash %s/%s: %w", pkg, f, err)
			}
			fmt.Fprintf(&manifest, "pkg\t%s\t%s\t%s\n", pkg, f, h)
		}
	}

	deps := make([]string, 0, len(externalDeps))
	for d := range externalDeps {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	for _, d := range deps {
		fmt.Fprintf(&manifest, "dep\t%s\n", d)
	}

	sum := sha256.Sum256(manifest.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// hashFile returns the hex-encoded SHA-256 of path's content with CRLF line
// endings normalised to LF first. See Compute's doc comment for why that
// normalisation is required on this repo specifically.
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// depsEntry is one decoded line of `go list -deps` output; see listDeps.
type depsEntry struct {
	importPath string
	standard   bool
	module     string   // "path@version"; empty for the main module or std
	dir        string   // absolute package directory
	files      []string // GoFiles + CgoFiles actually selected for this build
}

// depsFieldSep separates fields within one listDeps output line. Chosen as
// the ASCII unit separator specifically because it cannot appear in any
// field `go list` produces here (import paths, module versions, and
// filesystem paths are all printable text), so splitting on it can never be
// ambiguous the way splitting on a comma or tab could be.
const depsFieldSep = "\x1f"

// listDeps runs `go list -deps` for ShimPackage exactly as cmd/shimgen
// builds it for arch (GOOS=linux, CGO_ENABLED=0), decoding one depsEntry
// per output line.
func listDeps(moduleRoot, arch string) ([]depsEntry, error) {
	tmpl := strings.Join([]string{
		"{{.ImportPath}}",
		"{{.Standard}}",
		"{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{end}}",
		"{{.Dir}}",
		`{{join .GoFiles ","}}{{if .CgoFiles}},{{join .CgoFiles ","}}{{end}}`,
	}, depsFieldSep)

	cmd := exec.Command("go", "list", "-deps", "-f", tmpl, ShimPackage)
	cmd.Dir = moduleRoot
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+arch,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}

	out := strings.TrimRight(stdout.String(), "\n")
	if out == "" {
		return nil, nil
	}

	var entries []depsEntry
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, depsFieldSep)
		if len(fields) != 5 {
			return nil, fmt.Errorf("unexpected `go list -deps` output line (%d fields, want 5): %q", len(fields), line)
		}
		e := depsEntry{
			importPath: fields[0],
			standard:   fields[1] == "true",
			module:     fields[2],
			dir:        fields[3],
		}
		if fields[4] != "" {
			e.files = strings.Split(fields[4], ",")
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// FindModuleRoot walks up from startDir looking for the directory
// containing go.mod, the way `go` itself locates the current module. Both
// cmd/shimgen (whose working directory is wherever `go generate` was
// invoked from) and internal/shim/embedded's freshness test (whose working
// directory is fixed to its own package directory by `go test`) need this:
// neither can assume it is already running from the module root.
func FindModuleRoot(startDir string) (string, error) {
	dir := startDir
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found walking up from %s", startDir)
		}
		dir = parent
	}
}
