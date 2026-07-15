# Path Hardening (G1, G4, F-PATH-1/2, git-ref) — Design

**Date:** 2026-07-15
**Status:** Approved (design review done in session)
**Source:** 2026-07-15 workspace-path audit (G1, G4) + path-handling audit
(F-PATH-1 Critical, F-PATH-2 Minor, git-ref-injection Important).

## Problems

- **G1** — `hostBackend.ResolveCachePath` leaves a non-scoped native cache
  path **unresolved** (`backend_host.go:300`, `return p`), so `cache.Save`
  tars it relative to the **agent process CWD**, not the workspace where the
  step ran (`cmd.Dir = workDir`). The comment claiming it is "relative to the
  objectstore's own root" is wrong — `path` is the filesystem directory to
  archive, not the object key. `cache: {path: node_modules}` silently saves
  the wrong directory on native; the identical YAML is correct on isolated
  and k8s. Cache failures are warn+skip, so the bug is invisible.
- **G4** — no workspace env var exists in any mode (only `UNIFIED_AGENT_OS`),
  so a job author cannot write a mode-portable absolute path; scripts that
  capture `$PWD` in a native step and feed it to a container step hand over
  an untranslatable host path.
- **F-PATH-1 (Critical)** — `path.Join`/`filepath.Join` **clean** `..`, and
  no resolver checks the result stays under the workspace root. On k8s the
  resolved artifact/cache path is executed **inside the privileged artifact
  sidecar** (which is `EnvFrom` the S3 secret), so
  `uploadArtifact: {name: loot, path: ../../proc/self/environ}` exfiltrates
  the object-store credentials (and the sidecar's ServiceAccount token) —
  a cross-container privilege escalation, since the user's `run:` steps
  execute in a different, unprivileged container. The download / cache-restore
  direction is a write primitive into the sidecar filesystem.
- **F-PATH-2 (Minor)** — `sanitizeJobName` (`workspace.go:23`) passes `.`,
  `..`, all-dot names, and Windows reserved device names (CON/PRN/NUL/…)
  through unchanged; a `..` job name would make `claimWorkDir` escape to
  `wsBase` and `prepareWorkspace`'s cleanup `RemoveAll` the shared base.
  Currently unreachable (`dsl.ValidateName` DNS-1123 rejects these upstream),
  but the function's own doc calls itself "a defensive escape" and fails
  that contract.
- **git-ref injection (Important)** — `gittemplate/fetch.go` passes
  `uri.Ref` as a positional arg to `git fetch`/`git ls-remote` with no `--`
  separator and no validation; a `uses:` URI like `git://h/o/r/p@--upload-pack=x`
  injects a git option, executed **on the controller** during template
  resolution.

## Decisions (from design review)

| Question | Decision |
|---|---|
| G1 | Resolve native cache paths against `workDir` like every other mode (unconditional; absolute paths still pass through). `ResolveCachePath` becomes identical to `ResolveArtifactPath` on the host too. |
| `UNIFIED_WORKSPACE` value | The step's cwd workspace root per mode: native → `workDir`; isolated → host mount path; k8s → pod mount path; scope (host & k8s) → `/workspace` (the scope container's cwd, even though it is a private scratch, so the var always names the current cwd's root). User `env:` may override it (same precedence as `UNIFIED_AGENT_OS`). |
| F-PATH-1 containment | Reject any resolved artifact/cache path that escapes the workspace root — **both** absolute paths and `..` escapes — in every mode (k8s + host + scope), surfaced as a step failure. Resolvers return `(string, error)`. |
| Delivery | One branch `path-hardening`; the F-PATH-1 security fix ships here (branch is unpublished, no disclosure exposure). git-ref fix included. |

## Design

### G1 + F-PATH-1 — containment in the resolve seam

The `ExecBackend` resolve methods change signature to return an error so a
containment violation fails the step instead of silently escaping:

```go
ResolveArtifactPath(scope ScopeHandle, p string) (string, error)
ResolveCachePath(scope ScopeHandle, p string) (string, error)
```

A shared helper (new `internal/agent/pathguard.go`, exported for k8s reuse or
duplicated per the two path flavors) enforces containment:

```go
// containWithinSlash joins a RELATIVE p under root using forward-slash
// semantics (container paths) and rejects escape; an absolute p is rejected.
func containWithinSlash(root, p string) (string, error)
// containWithinOS does the same with filepath (host paths).
func containWithinOS(root, p string) (string, error)
```

Rules (both):
- empty `p` → the root itself (unchanged behavior for the "" case where it
  meant the workspace dir).
- absolute `p` → **error** (previously passed through; this is the escalation
  vector and is now rejected in every mode for symmetry, matching the audit's
  recommendation).
- join, `Clean`, then require the cleaned result equals root or has prefix
  `root + separator`; otherwise **error**.
- error message: `artifact/cache path %q escapes the workspace` (grep-able).

Wiring:
- **host** `resolveWorkspacePath(workDir, p)` → `containWithinOS(workDir, p)`;
  `resolveScopePath(p)` → `containWithinSlash(scopeWorkDir, p)`. Both host
  `ResolveArtifactPath` and `ResolveCachePath` (non-scoped) use
  `containWithinOS(workDir, p)` — **G1 is fixed by cache now taking the same
  path as artifact** (the `return p` branch is deleted).
- **k8s** `ResolveArtifactPath`/`ResolveCachePath` → `containWithinSlash`
  against `mountPath` (non-scoped) / `scopeMountPath` (scoped).
- Callers in `orchestrator.go` (`executeUploadArtifact`,
  `executeDownloadArtifact`, `executeCacheStep`) propagate the error as a step
  failure with the resolver's message.

Note: relative paths — the portable, documented form — are unaffected; only
absolute and `..`-escaping paths change from "silently (mis)resolved" to
"step fails with a clear message".

### G4 — `UNIFIED_WORKSPACE` injection

Add to `ExecBackend`:

```go
// WorkspacePath returns the cwd workspace root for a step in this scope,
// as seen from where the step executes (host dir natively; container mount
// path in isolated/k8s; the scope container's cwd when scoped).
WorkspacePath(scope ScopeHandle) string
```

- host: scope non-zero → `scopeWorkDir`; `b.pod != nil` → `hostNamedMountPath`
  (the isolated mount, default `/workspace`); else `b.workDir`.
- k8s: scope non-zero → `scopeMountPath`; else `b.mountPath`.

In `orchestrator.go` (~:397), alongside the `UNIFIED_AGENT_OS` line:

```go
extraEnv := []string{
    "UNIFIED_AGENT_OS=" + agentOSForStep(step, b.DefaultAgentOS()),
    "UNIFIED_WORKSPACE=" + b.WorkspacePath(scope),
}
```

A user `env:` entry named `UNIFIED_WORKSPACE` appends after and wins (same as
today for `UNIFIED_AGENT_OS`). The k8s agent's separate `imageStepEnv`
(`k8sagent/agent.go:315`) also sets `env["UNIFIED_WORKSPACE"]` = the scope
pod mount for scoped `runsIn.image` steps.

### F-PATH-2 — `sanitizeJobName` hardening

After the existing rune filter, reject a degenerate result: if the sanitized
string is `.`, `..`, or all dots, OR (case-insensitively) a Windows reserved
device name (`CON`, `PRN`, `AUX`, `NUL`, `COM1`–`COM9`, `LPT1`–`LPT9`,
optionally with an extension), return a safe fallback (`"job"` or the name
with a `_` prefix). Keep the existing empty→`"job"` case.

### git-ref injection

In `internal/gittemplate/uri.go` `ParseURI`: validate `ref` against an
allowlist (`^[A-Za-z0-9][A-Za-z0-9._/+-]*$` — must not start with `-`),
returning a parse error otherwise. In `internal/gittemplate/fetch.go` (the
three `git fetch`/`git ls-remote` call sites), insert `--` before the refspec
so even an allowlisted ref can never be parsed as an option. Belt and braces:
the allowlist blocks the input, the `--` blocks the argument position.

### Docs

- `docs/jobs.md`: artifact/cache `path` must be **relative** to the
  workspace (portable across native/isolated/k8s); absolute paths and paths
  escaping the workspace are rejected; `UNIFIED_WORKSPACE` names the current
  workspace root inside a step.
- `docs/configuration.md`: mention `UNIFIED_WORKSPACE` as a step-provided env
  var (not a controller setting — put it wherever `UNIFIED_AGENT_OS` is
  documented, or a new "step environment" note).
- `docs/troubleshooting.md`: entries for `escapes the workspace` (a step used
  an absolute or `..` artifact/cache path) and the git-ref validation error.

## Out of scope

- G2/G3/G5/G6 from the workspace audit (absolute-path policy beyond
  rejection, uses-scope workspace sharing, k8s workingDir desync, Windows
  mount-string normalization) — G2 is partially addressed (absolute paths now
  rejected rather than silently re-rooted), the rest are separate designs.
- F-PATH-3 (native/scope absolute artifact path) — folded into F-PATH-1's
  blanket absolute-path rejection, so it is fixed as a side effect, not a
  separate task.
- Changing the object-store key scheme or the sidecar's credential exposure
  (defense-in-depth beyond the traversal fix).

## Testing

- **G1**: host cache step (native, `pod == nil`) resolves `node_modules`
  against `workDir` — a store-agnostic unit test on the resolver plus an
  integration test asserting the archived cache contains the workspace file,
  not an agent-CWD file.
- **F-PATH-1**: table test over all resolvers (host artifact/cache/scope, k8s
  artifact/cache/scope) — `../../etc/passwd`, `..`, `/etc/passwd`,
  `a/../../b`, `foo/../bar` (in-bounds, allowed), `` (root, allowed),
  `subdir/file` (allowed) → escapes error vs. correct join. HTTP/step-level
  test that an escaping `uploadArtifact.path` fails the step with the
  grep-able message and performs no upload.
- **G4**: a `run:` step reads `$UNIFIED_WORKSPACE` and asserts it equals the
  step cwd (`pwd`) in native, isolated, and k8s (extend the existing
  `UNIFIED_AGENT_OS` parity/integration tests); user `env:` override wins.
- **F-PATH-2**: `sanitizeJobName("..")`, `"."`, `"..."`, `"con"`, `"COM1"`,
  `"nul.txt"` → safe fallback; ordinary names unchanged.
- **git-ref**: `ParseURI` rejects `@-x`, `@--upload-pack=y`; accepts
  `@main`, `@v1.2.3`, `@feature/x`; a `fetch` unit/integration test (if the
  suite has one) confirms `--` precedes the ref.
- Full `go test ./internal/...` green; the parity suite
  (`internal/paritycases`, `internal/k8sagent/parity_k8s_test.go`) still
  passes with the new env var and resolver signatures.
