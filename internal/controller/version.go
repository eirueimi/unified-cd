package controller

import "runtime/debug"

// Version is the controller build version. It is empty in a plain `go build`
// and stamped at build time via -ldflags:
//
//	go build -ldflags "-X github.com/eirueimi/unified-cd/internal/controller.Version=v1.2.3"
//
// docker/controller.Dockerfile passes it from the VERSION build arg, which
// .github/workflows/release-docker.yml feeds from the release tag.
var Version = ""

// BuildVersion returns the version stamped at build time, the module version
// recorded by `go install`, or "dev" for local untagged builds. It mirrors
// internal/cli.buildVersion so the controller answers the same way the CLI
// does, and so an unstamped binary is visibly "dev" rather than an empty
// string.
//
// This value is reported, never compared: compatibility between a controller
// and an agent is decided by capabilities (see docs/operator-manual/agents.md), not by
// version. The version exists so an operator can *see* the fleet's state
// mid-upgrade.
func BuildVersion() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
