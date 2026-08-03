package agent

// Version is the agent build version, reported to the controller at
// registration (api.AgentRegisterRequest.Version) and surfaced in
// GET /api/v1/agents. It is stamped at build time via -ldflags:
//
//	go build -ldflags "-X github.com/eirueimi/unified-cd/internal/agent.Version=v1.2.3"
//
// Stamped by: .goreleaser.yaml (release binaries), the Makefile's `build`
// target (local builds), and docker/agent.Dockerfile +
// docker/k8s-agent.Dockerfile from the VERSION build arg, which
// .github/workflows/release-docker.yml feeds from the release tag. Anything
// else leaves it "dev".
//
// This value is reported, never compared: controller/agent compatibility is
// decided by capabilities (see docs/agents.md), not by version.
var Version = "dev"
