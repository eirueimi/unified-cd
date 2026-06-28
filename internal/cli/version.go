package cli

import "runtime/debug"

// version can be overridden at build time:
//
//	go build -ldflags "-X github.com/eirueimi/unified-cd/internal/cli.version=v1.2.3"
var version = ""

// buildVersion returns the version string embedded at build time, the module
// version recorded by go install, or "dev" for local untagged builds.
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
