package main

import "runtime/debug"

// version is the sidecar build version, stamped at build time via -ldflags:
//
//	go build -ldflags "-X main.version=v1.2.3" ./cmd/unified-sidecar
//
// docker/artifact-sidecar.Dockerfile passes it from the VERSION build arg,
// which .github/workflows/release-docker.yml feeds from the release tag.
//
// The sidecar has no wire to the controller, so this version is not reported
// anywhere automatically — it is readable via `unified-sidecar version`.
// That matters because docs/operator-manual/operations.md requires the sidecar image and the
// k8s-agent to be upgraded in lockstep, and until now there was no way to
// check which sidecar build a pod was actually running.
var version = ""

// isVersionCommand reports whether args is a bare version request. The
// sidecar dispatches on positional subcommands ("cache save", "idle"), so it
// accepts the subcommand spelling and the two flag spellings alike.
func isVersionCommand(args []string) bool {
	if len(args) != 1 {
		return false
	}
	switch args[0] {
	case "version", "--version", "-version":
		return true
	}
	return false
}

// buildVersion mirrors internal/cli.buildVersion and
// internal/controller.BuildVersion so every unified-cd binary answers the
// same way.
func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
