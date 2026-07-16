# Path Hardening Wave 2 — AppSource Git Argv, workingDir Desync, Windows Mounts, Docs — Design

**Status:** Approved 2026-07-16 (Branch B of the A→B→C→D hardening program; decisions locked via Q&A — G5 = reject workingDir on step-targeted containers).

**Goal:** Close the path/argv gaps left open by PR #48: the AppSource git option-injection (**RCE on the controller**), the k8s `workingDir` cwd-vs-resolution desync (G5), the fragile Windows `-v` mount concatenation (G6), and the two remaining doc gaps (G2 migration note, G3 scoped-uses troubleshooting).

## 1. AppSource git argv hardening (SECURITY — controller RCE)

**Problem (verified):** `internal/gittemplate/fetch.go` passes AppSource-controlled values into git argv unguarded:
- `fetch.go:230` `git ls-remote <repoURL> -- <ref>` and `fetch.go:285` `git fetch --depth=1 <repoURL> -- <ref>` — **`repoURL` sits before `--`**: `repoURL: "--upload-pack=<cmd>"` executes `<cmd>` on the controller host (classic git option-injection RCE). The `insertPlaceholderUser` rewrite only fires for `https://` prefixes, so `-`-prefixed URLs pass through raw.
- `fetch.go:289-297` `git ls-tree -r --name-only FETCH_HEAD <treePath>` — **`treePath` (AppSource `spec.Path`) appended with no `--`**: option injection into ls-tree (e.g. `--output=...`).
- `TargetRevision` is positionally guarded by `--` today but never run through the `refAllowed` allowlist (`uri.go:9`, applied only in `ParseURI`) — inconsistent, and fragile against future call sites.

**Design (validate at the boundary + defend at the argv):**
- `AppSource.Validate` (`internal/dsl/appsource_parse.go`) gains:
  - `RepoURL`: must match a scheme allowlist — `https://`, `http://`, `ssh://`, or `git@host:` (scp-like) — and must not start with `-`. Reject anything else with a clear message.
  - `TargetRevision`: must match the same allowlist rule as uses refs (start alphanumeric; charset `[A-Za-z0-9._/+-]`). Export a shared helper from `gittemplate`… no — dsl cannot import gittemplate cleanly? (check: gittemplate imports dsl, so dsl must NOT import gittemplate). Put the shared regexp in `internal/dsl` (e.g. `dsl.ValidateGitRef(ref) error`) and have `gittemplate/uri.go`'s `refAllowed` delegate to it (single source).
  - `Path`: already rejects `..`; additionally reject a leading `-`.
- Defense-in-depth at the argv sites in `fetch.go`: insert `--` before `treePath` in the ls-tree argv; before `ls-remote`/`fetch`, reject a `-`-prefixed repoURL with an explicit error (belt-and-braces even though Validate now blocks it — these functions are also reachable from other callers).

## 2. G5 — reject `workingDir` on step-targeted containers

**Problem (verified):** the k8s executor never `cd`s (exec cwd = the container's `WorkingDir`; podbuilder only defaults it to the workspace mount when EMPTY), while `ResolveArtifactPath`/`ResolveCachePath`/`UNIFIED_WORKSPACE` all use the workspace mount path. A container with `workingDir: /app` makes steps run in `/app` while artifact/cache/downloads resolve to `/workspace` — silent wrong-file behavior; the artifact sidecar physically cannot reach paths outside the workspace volume.

**Design (locked decision — option c):** a `workingDir` on a **step-targeted** container is a validation error:
- Step-targeted = the primary `job` container, plus any container named by a step's `container:` field.
- Enforced in `dsl.Job.Validate` (walk podTemplate `containers` + `override.containers`; if a def has a `workingDir` key AND its name is `job` or referenced by any step's `container:` — error naming the container and pointing at the workspace-mount invariant). Named agent-side templates (`podTemplate.name`) can't be checked at apply — enforce the same rule at pod build in the k8s agent (`podbuilder.go`: when building, if a step-targetable container from an agent template carries `workingDir`, fail the run with the same message) — pragmatically: podbuilder does not know which containers steps target; enforce on the `job` container only at build time, and rely on apply-time for inline specs. Keep scope minimal: apply-time check for inline podTemplates (+override); build-time check only for the primary `job` container from named templates.
- Sidecars (non-step-targeted containers) keep `workingDir` freely.
- Docs: state the invariant (steps always run at the workspace mount) and that sidecar workingDir is fine.

## 3. G6 — `--mount` form for OCI CLI bind mounts (Windows-safe)

**Problem (verified):** `internal/runtime/ocicli.go:104-110` (and `apple.go:79`) build `-v HostPath:ContainerPath[:ro]` by string concat. Windows `C:\ws` contains a colon — survives only via Docker Desktop's drive-letter heuristics; podman/nerdctl/wslc differ; zero Windows-path tests.

**Design:** switch `createArgs` to `--mount type=bind,source=<HostPath>,target=<ContainerPath>[,readonly]` in `ocicli.go`; keep `apple.go` on `-v` only if the `container` CLI lacks `--mount` (check; if unsupported, leave apple as-is with a comment — macOS paths have no drive-letter colon problem). Escape rule: `--mount` fields are comma-separated key=value; a HostPath containing a comma is exotic — reject mounts whose paths contain `,` or `=` with a clear error rather than attempting quoting (document). Add table tests covering Windows (`C:\ws`), POSIX, and readonly variants asserting the exact argv.

## 4. Docs — G2 migration note + G3 troubleshooting entry

- G2: add a short "behavior change" note (docs/jobs.md or the path-hardening migration doc if one exists — else `docs/migration-2026-07-job-isolation.md` adjacent new file is overkill; put it in troubleshooting's existing "escapes the workspace" entry): absolute artifact/cache paths used to be silently re-rooted (k8s) or host-resolved; since #48 they hard-fail.
- G3: new `docs/troubleshooting.md` entry: "scoped `uses` step can't see workspace files" — scope starts from a FRESH filesystem; silent absence, not an error; pass inputs via `with:`/`downloadArtifact`.

## Components / files
- `internal/dsl/appsource_parse.go` + `internal/dsl/gitref.go` (new: `ValidateGitRef`); `internal/gittemplate/uri.go` delegates; `internal/gittemplate/fetch.go` argv defense.
- `internal/dsl/parse.go` (workingDir rejection) + `internal/k8sagent/podbuilder.go` (named-template job-container build-time check).
- `internal/runtime/ocicli.go` (+ tests `ocicli_lifecycle_test.go` additions); `apple.go` reviewed.
- `docs/troubleshooting.md`, `docs/jobs.md`, `docs/resources.md` (AppSource field constraints).

## Testing
- AppSource validate: `--upload-pack=` repoURL rejected; `-`-prefixed ref/path rejected; happy https/ssh/scp-like pass; ls-tree argv contains `--` before path (unit on the argv builder if factored, else fetch-level test with a local repo fixture if one exists — check existing fetch tests' harness).
- workingDir: inline podTemplate job-container workingDir → apply error; step-targeted named container workingDir → apply error; sidecar workingDir → OK; override-container workingDir targeted by step → error.
- Mounts: argv table tests incl. `C:\ws` and readonly; reject `,`/`=` in paths.
- Full suite + generated-artifact no-drift.

## Out of scope
- Resolving artifact paths against workingDir (option b — rejected in design Q&A).
- Rewriting the host claim-pod mount mechanism (it already can't express workingDir).
