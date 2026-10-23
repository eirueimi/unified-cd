# Contributing to unified-cd

Thanks for your interest in contributing. Bug reports, feature requests, and pull
requests are all welcome.

## License

unified-cd is licensed under the [Apache License, Version 2.0](https://github.com/eirueimi/unified-cd/blob/main/LICENSE). By
submitting a contribution, you agree that it is provided under the same license
(Apache-2.0 §5, "inbound = outbound").

## Developer Certificate of Origin (DCO)

To keep the provenance of every contribution clear, this project uses the
[Developer Certificate of Origin](https://developercertificate.org/). It is a
lightweight, sign-off-based alternative to a CLA: you are not assigning
copyright, only certifying that you have the right to submit the code under the
project's license.

Every commit must carry a `Signed-off-by` line that matches the commit author,
which certifies the text below:

```
Developer Certificate of Origin
Version 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I have the right
    to submit it under the open source license indicated in the file; or
(b) The contribution is based upon previous work that, to the best of my
    knowledge, is covered under an appropriate open source license and I have
    the right under that license to submit that work with modifications; or
(c) The contribution was provided directly to me by some other person who
    certified (a), (b) or (c) and I have not modified it.
(d) I understand and agree that this project and the contribution are public and
    that a record of the contribution (including all personal information I
    submit with it, including my sign-off) is maintained indefinitely and may be
    redistributed consistent with this project or the open source license(s)
    involved.
```

Add the sign-off automatically by committing with `-s`:

```bash
git commit -s -m "your message"
```

which appends, using your `git config user.name` / `user.email`:

```
Signed-off-by: Your Name <you@example.com>
```

If you forget it on the last commit, `git commit --amend -s --no-edit` fixes it;
for older commits use an interactive rebase.

## Development

See the "Development" section in the [README](https://github.com/eirueimi/unified-cd/blob/main/README.md) for build and test
commands (`make build`, `make test`, `make dev-go`, `make dev-ui`, …). Please run
the tests before opening a pull request.

### Integration tests

`go test ./...` (and `make test`) runs the unit tests. Two groups of tests need a
real runtime and sit behind build tags, so they are skipped unless asked for:

| Tag | What it needs | Run |
|---|---|---|
| `integration` | a container runtime (docker, podman or nerdctl) | `go test -tags integration ./internal/agent/ -run 'SecCtx\|ClaimPod\|ProbeSecurityContext' -count=1 -v` |
| `k8s` | a Kubernetes cluster (kind) and a shim image | see the `k8s` job in `.github/workflows/ci.yml` |

Environment variables for the `integration` tests:

- `UCD_TEST_CONTAINER_RUNTIME` selects the runtime: `docker`, `podman` or
  `nerdctl`. Unset, the first one installed is used. CI sets it per matrix leg
  (`host-runtime-integration`: rootful docker, rootless podman, rootless docker),
  because a machine with both docker and podman would otherwise always test docker.
- `UCD_TEST_WSBASE` is a directory the runtime can bind-mount with real Linux
  semantics (setgid inheritance, `chown`). The securityContext tests need it on
  Windows and macOS, where the default (a temp directory on the host disk) goes
  through a file-sharing mount that drops both; there the startup probe fails and
  the tests skip with a message saying so. Under Docker Desktop, use a path inside
  the VM that is not `noexec` (not `/tmp`), for example by running the test binary
  in a container that mounts the Docker socket and a VM directory at the same
  path.
- `UCD_REQUIRE_RUNTIME=1` (implied by `CI`) turns "no runtime / no shim / no
  cluster" from a skip into a failure, because `go test` reports a skip as ok.
  The CI jobs also run `.github/scripts/check-test-json.sh` over `go test -json`,
  which fails on any skipped test of the required set (other than the documented
  host-only cases) and on a wrong number of passed parity subtests.
- `UCD_TEST_EXPECT_ROOTLESS=1` (set by the rootless CI legs) asserts the startup
  probe saw a rootless runtime.

The securityContext parity golden is `paritycases.SecurityContextCases()`. The
same cases run against a real container runtime
(`internal/agent/secctx_parity_integration_test.go`) and a real kubelet
(`internal/k8sagent/secctx_parity_k8s_test.go`, tag `k8s`), and must produce the
same output. A difference between the two is a design question, not a test to
adjust.

Two things the golden does not compare:

- The two volume-drift cases (`secctx-volume-regroup-always`,
  `secctx-volume-onrootmismatch-skips`) are host-only. They need a root-owned
  leftover in a volume that outlives a run, and the kubelet applies `fsGroup` when
  it sets a volume up, before any container can write; kind's `local-path` PVCs
  are never regrouped at all (design 6.5). On Kubernetes they skip, and CI
  allows exactly those two skips.
- The Kubernetes side emulates the `unified-artifact` sidecar with a plain
  container that has only the pod's `securityContext`. That relies on the real
  sidecar never carrying a container-level `securityContext`, which
  `TestBuildPod_ArtifactSidecarHasNoContainerSecurityContext`
  (`internal/k8sagent/podbuilder_test.go`) pins. Without that test, artifact
  case `secctx-artifact-container-level` could not fail on the Kubernetes side.

