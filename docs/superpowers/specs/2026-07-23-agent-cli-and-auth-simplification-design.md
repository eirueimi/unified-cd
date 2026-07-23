# Agent CLI naming + enrollment/auth-model simplification

**Date:** 2026-07-23
**Status:** Approved (brainstorming)

## Overview

Five requested changes to the agent CLI and the agent authentication model,
decomposed into **four independent PRs** (executed in order, each merged before
the next branches off `main`):

| PR | Scope | Depends on |
|----|-------|-----------|
| **A** | Rename `cmd/agent` → `cmd/unified-cd-agent` (fix binary-name drift) | — |
| **B** | Fix: re-enrollment ignores changed labels; Feature: pass the enrollment token inline (not only via file) | — |
| **C** | Remove the legacy shared-token agent auth path entirely | — |
| **D** | Capabilities become auto-detected only (drop admin-set enrollment capabilities) | C |

A and B are independent of everything. C simplifies the register/authorization
model; D builds on that simplification, so **C before D**.

## Global constraints (all PRs)

- Module path is `github.com/eirueimi/unified-cd`.
- No cgo/gcc in the dev env → run Go tests without `-race` (CI runs `-race`).
- Every removal/rename PR must end with a **repo-wide** reference scan
  (`--include=*.go --include=*.md --include=*.yaml --include=*.yml --include=*.toml`,
  excluding `docs/superpowers/`, `vendor/`, `.superpowers/`) proving no stale
  reference to a removed/renamed symbol survives — a prior task's `docs/`-only
  scan missed live references.
- Each PR: own branch off latest `main`, own implementation plan, own review,
  own PR; merged before the next.

---

## PR A — Rename `cmd/agent` → `cmd/unified-cd-agent`

**Why:** `go install …/cmd/agent` yields a binary named `agent`, while
`make build` and docs use `unified-cd-agent` — a naming inconsistency. Renaming
the command directory makes every path (`go install`, `make build`, release,
docker, docs) converge on `unified-cd-agent`.

**Behavior:** unchanged. This is a directory move + reference updates only.

**Changes:**
- Move `cmd/agent/` → `cmd/unified-cd-agent/` (`package main` unchanged;
  import path becomes `…/cmd/unified-cd-agent`).
- `.goreleaser.yaml`: `main: ./cmd/agent` → `./cmd/unified-cd-agent`;
  `binary: agent` → `unified-cd-agent`.
- `Makefile`: `go build … ./cmd/agent` → `./cmd/unified-cd-agent` (output stays
  `bin/unified-cd-agent`).
- `docker/agent.Dockerfile`: `./cmd/agent` → `./cmd/unified-cd-agent`.
- `.air.agent.toml`: `./cmd/agent` → `./cmd/unified-cd-agent`.
- Docs (`README.md`, `docs/getting-started.md`): `go install …/cmd/agent…` →
  `…/cmd/unified-cd-agent…`; the installed binary is now consistently
  `unified-cd-agent`.
- Code comments referencing `cmd/agent` (e.g. `internal/shim/embedded/embed.go`,
  any others found by the scan).
- `internal/agent` (the library) is NOT moved — only the `cmd` wrapper.

**Tests/verification:** `go build ./...`; `go install …/cmd/unified-cd-agent`
produces `unified-cd-agent`; repo-wide scan for `cmd/agent` (exact) is clean
(only `cmd/unified-cd-agent`, `internal/agent`, and `bin/unified-cd-agent`
remain). `docker build -f docker/agent.Dockerfile` succeeds (or note if Docker
unavailable and rely on `go build`).

---

## PR B — Label re-enrollment fix + inline enrollment token

### B1. Bug: re-enrollment ignores changed labels

**Root cause (confirmed):** `internal/store/postgres_agent_auth.go`
`ConsumeAgentEnrollment` (~line 152). On first enrollment it INSERTs the
identity with the token's `AuthorizedLabels` (~184-186); on re-enrollment of an
EXISTING identity (else branch ~191-195) it checks status/method but does NOT
update `authorized_labels`, so minting a new enrollment token with changed
labels has no effect (identity keeps stale authz). The k8s path
`IssueExternalAgentAccess` (~245) already updates on reuse — the VM path is the
inconsistent one.

**Fix:** in the existing-identity branch, `UPDATE agent_identities SET
authorized_labels = $token_labels` and mirror onto the returned `identity`
struct. (Capabilities are intentionally NOT updated here — PR D removes
enrollment-set capabilities entirely; until D lands, B leaves capabilities
untouched to avoid conflicting with D. B is labels-only.)

- Mirror the fix in any non-Postgres store implementation (e.g. an in-memory
  test store) so the store-interface contract stays consistent.
- Test: enroll agent with `label:a` → re-enroll (new token) with `label:b` →
  the identity's `AuthorizedLabels` is now `label:b` (was the bug: stayed
  `label:a`).

### B2. Feature: pass the enrollment token inline

Currently the agent accepts the enrollment token only as a file
(`--enrollment-token-file` / `UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE` /
`enrollmentTokenFile`). Add a direct-value path so `enrollment create` output
can be handed to the agent without a file step.

- **Agent side:** add `--enrollment-token <value>` flag + env
  `UNIFIED_AGENT_ENROLLMENT_TOKEN`; add `EnrollmentToken` to
  `config.AgentConfig`/`AgentEffective` and `CredentialManagerConfig`. The
  credential manager uses the inline value when set, else reads the file.
  - Support `--enrollment-token -` to read the token from **stdin** (for
    piping).
  - **Conflict rule (decided):** if BOTH a token value (flag/env/stdin) and a
    token file resolve to non-empty, fail at startup with a clear error — do
    not silently pick one.
- **`enrollment create` output (both forms — decided):**
  - When NOT using `--output-file` (token is on stdout), the "next steps"
    suggested command embeds the **actual token inline**:
    `unified-cd-agent --server … --id … --enrollment-token <TOKEN>` — copy-paste
    ready. Include a one-line security note (the token appears in shell
    history/`ps`).
  - When using `--output-file`, keep showing `--enrollment-token-file <file>`.
  - Add a `--quiet` flag to `enrollment create` that prints **only the token**
    (no prose, no next-steps), enabling
    `unified-cli agent enrollment create --quiet … | unified-cd-agent
    --enrollment-token -`.
- Docs: document the inline flag, the env var, stdin (`-`), `--quiet`, and the
  conflict rule; keep the file form as the more-secure default.
- Tests: inline value used when set; stdin `-` read; both-value-and-file →
  error; `--quiet` prints only the token; `nextAgentCommands` inline vs file
  branch.

---

## PR C — Remove the legacy shared-token agent auth path

**Why:** legacy shared-token auth is already off-by-default (controller must set
`UNIFIED_AGENT_LEGACY_TOKEN`), deprecated (runtime warning), inherently weaker
(a shared secret that "proves nothing about which physical agent is calling"),
and restricted (no secret access). Removing it makes enrollment the single auth
model and collapses every `AuthMethod == "legacy"` special case.

**Changes (verify complete via repo-wide scan):**
- `internal/controller/agent_auth.go`: delete the legacy branch (the
  `AuthMethod: "legacy"` path, the `s.cfg.LegacyAgentToken` check) and the
  `if principal.AuthMethod == "legacy"` handling in `requireAgentPathIdentity`
  (~122) and elsewhere.
- `internal/controller/api_agent.go`: the register handler's `if
  principal.AuthMethod != "legacy"` branch (~37-44) collapses — labels/caps
  always come from the authorized set (the `else`/legacy self-declared path and
  the legacy hostname-label branch ~62-72 are removed).
- `internal/controller/api_secrets.go:81`: drop the `|| AuthMethod == "legacy"`
  clause (no legacy principals exist anymore).
- Controller config: remove `LegacySharedToken` (`internal/config/controller.go`
  field, `UNIFIED_AGENT_LEGACY_TOKEN` env, `agentAuth.legacySharedToken` YAML)
  and `LegacyAgentToken` on the server config it feeds.
- Agent side (`cmd/unified-cd-agent`, `internal/config/agent.go`): remove
  `--token` flag, `UNIFIED_AGENT_TOKEN` env, the `Token` config field, and the
  "using deprecated legacy shared agent token" branch in main.
- Metrics: remove `AgentLegacyAuth()` if now unused.
- Docs: remove all legacy-shared-token guidance
  (`docs/configuration.md`, `docs/authentication.md`, `docs/agents.md`, compose
  files' comments) — rewrite to "enrollment is the only agent auth model".
- Tests: delete/adjust legacy-auth tests; add/keep a test asserting a
  shared-token request is now rejected (unauthorized), and that agent
  self-declared labels are ignored (only authorized labels apply).

**Behavior change / breaking:** any deployment still using the shared token
breaks; accepted (deprecated, off-by-default, migration-only). k8s agents use
kubernetes enrollment and are unaffected.

---

## PR D — Capabilities auto-detected only

**Why:** capabilities describe what an agent can technically run (`native`
always; `container` if a container runtime is present), not an authorization
boundary. Today enrolled agents' capabilities are overridden by the enrollment
token's admin-set `AuthorizedCapabilities`, so the agent's own auto-detection is
discarded, and an unset `--capability` yields NULL = unconstrained (an agent
with no runtime can claim container jobs that then fail). Simpler and more
correct: trust the (authenticated) agent's auto-detected capabilities.

**Depends on C** (register/auth model already simplified to a single path).

**Changes:**
- `internal/cli/agent_enrollment.go`: remove the `--capability` flag from
  `agent enrollment create` (and, if agreed, from the enrollment-policy
  command — see open item) so capabilities are no longer admin-set.
- `internal/controller/api_agent.go` register: use the agent-reported
  `req.Capabilities` directly (validated against `dsl.ValidCapability`), not an
  authorized-capabilities override. Keep the NULL/unconstrained-legacy claim
  semantics OR set the agent's detected caps — capabilities now come from
  `agentCapabilities()` on the agent (`internal/agent/agent.go:135`, unchanged:
  `native` + `container` if runtime).
- Store/API: stop populating/reading `authorized_capabilities` on
  enrollment tokens and identities. **Open sub-decision (resolve in D's plan):**
  drop the `authorized_capabilities` columns via a migration, or leave them
  unused/nullable. Default: leave unused (smaller, reversible), remove from
  code paths; optional follow-up migration to drop.
- `api.CreateAgentEnrollmentRequest` / responses / `identityMeta`: drop the
  capabilities field from the enrollment-create request and identity output (or
  keep identity output showing the agent's current caps from the `agents`
  table).
- Docs: `agent enrollment create` no longer documents `--capability`;
  capabilities section explains they are auto-detected from the agent's runtime.
- Tests: enrolling an agent with a container runtime → `container` capability
  present without any `--capability`; no-runtime agent → only `native`;
  `enrollment create` has no `--capability` flag.

**Interaction with B:** B leaves capabilities untouched on re-enroll; D removes
enrollment-set capabilities entirely, so B's "labels-only" scope is correct and
there is no capabilities-on-reenroll bug left to fix.

---

## Testing strategy (all PRs)

- Store-level tests for B1 (label re-enroll) and D (caps from agent report) —
  these need Postgres (`store.NewTestPostgres`); pure parser/CLI tests do not.
- CLI tests for B2 (inline/stdin/quiet/conflict) and D (`--capability` gone).
- Repo-wide reference scan is a hard gate for A, C, D.
- `go build ./...` + `go test ./... -short` green before each PR.

## Risks / notes

- C and D are breaking for legacy/admin-capability users; both are
  deprecated/edge and explicitly requested for removal.
- Ordering C→D avoids reworking the register handler twice in conflicting ways.
- The `authorized_capabilities` column drop (D) is the only schema question;
  defaulting to "leave unused" keeps D reversible.
