//go:build !integration

package agent

import (
	"fmt"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// Unit tests must never touch the host container runtime: with docker on PATH,
// Agent.Run's startup orphan sweep would execute the real `docker ps` and block
// claiming until it returned. Auto-detection therefore finds nothing by
// default; tests that need a runtime pre-seed resolvedRuntime via runtimeOnce.
// Integration-tagged builds exclude this file and use the real detector.
func init() {
	detectRuntime = func(preferred string) (crt.ContainerRuntime, error) {
		return nil, fmt.Errorf("no container runtime available (hermetic unit test; preferred=%q)", preferred)
	}
}
