# Agent enrollment-first credential resolution + optional `--id` — Design

**Date:** 2026-07-24
**Scope:** VM agent (`unified-cd-agent`) credential bootstrap. No controller, k8s-agent, or token-format changes.

## Problem

Two rough edges in VM agent enrollment, both observed in practice:

1. **Label updates via re-enrollment don't apply.** After a first enrollment, the agent persists a durable credential file (`~/.unified-cd/<id>/credential.json`). On a later start the agent finds that file and authenticates via the refresh path, **ignoring any freshly supplied enrollment token**. So `enrollment create --agent-id mac --label mac,unity` piped to a re-run does not change the agent's authorized labels — the new token is never consumed. (PR #74 made `ConsumeAgentEnrollment` update `authorized_labels` on re-enroll, but that path is unreachable while the cached credential short-circuits enrollment.)

2. **`--id` is redundant and a footgun.** The enrollment token is bound server-side to a fixed agent ID, and the exchange response returns that `AgentID`. Yet the agent requires `--id` to match it (`credentials.go` hard-fails on `response.AgentID != m.agentID`), forcing the operator to re-type an ID the server already knows and producing a confusing `credential response is invalid` on any mismatch. The k8s agent already derives its identity from enrollment; only the VM path carries this legacy requirement.

## Design

### A. Enrollment-first resolution (fixes #1)

The agent's one-time startup credential bootstrap (first `Token()` call, run once per process) resolves in this order:

1. Load the persisted credential if present → `hasCred`.
2. If an enrollment token is **explicitly present** (inline `--enrollment-token`/`UNIFIED_AGENT_ENROLLMENT_TOKEN`, stdin `-`, or `--enrollment-token-file`):
   - Attempt the **enroll** exchange first — even when a credential already exists (an explicit token signals update intent).
   - **Success** → the controller updates the identity's `authorized_labels` (PR #74) and issues a new credential; persist it; **done** (new labels take effect).
   - **Rejected with HTTP 401** (`ErrAgentEnrollmentInvalid`: expired / already consumed / invalid) **and `hasCred`** → log a WARN and **fall back to the existing credential** (refresh). The agent keeps running; labels stay as they were. This is the "don't brick on a stale token" case.
   - **401 and no credential** → return the error (nothing to fall back to).
   - **Any other failure** (transient 503 after retries, 403 disabled, network) → return the error. 503 is deliberately **not** a fallback trigger: the token may still be valid and the refresh backend is the same one that just failed, so retrying enroll is the correct behavior.
3. Else (no enrollment token) → refresh with the existing credential (current behavior).
4. Else (neither) → "agent credentials are required".

After bootstrap, periodic access-token refresh is unchanged; enroll is not re-attempted within the process.

**Fallback trigger is exactly HTTP 401.** The client already surfaces the status via `credentialRequestError{status}`; `exchangeWithRetry` retries 429/5xx and returns 401 immediately.

### B. Optional `--id` (fixes #2)

Identity flows **from** the enrollment/credential, matching the k8s model:

- **`--id` omitted** → adopt the resolved `AgentID` (from the enroll response on first run, or from the persisted credential on restart).
- **`--id` provided** → keep the equality check as an **assertion** (mismatch → error), useful for catching copy-paste mistakes. Backward compatible.
- The credential-file **default path** currently derives from `--id` (`~/.unified-cd/<id>/credential.json`). When `--id` is omitted and `--credential-file` is not set, use an **ID-independent fixed path** `~/.unified-cd/credential.json`. When `--id` is set, the path is unchanged (full backward compat). `--credential-file`, when explicit, always wins.
- The agent's operational ID (used for register/claim/reconcile) comes from the resolved identity, not the `--id` flag directly. The agent resolves its identity at startup (a new `EnsureIdentity(ctx)` that performs the bootstrap and returns the `AgentID`) before constructing the agent loop.

### Tradeoffs / non-goals

- **Multi-agent per host without `--id`/`--credential-file`** would collide on the single default path. That configuration must set `--credential-file` (or `--id`) explicitly; documented. `--id`-set behavior is unchanged, so existing deployments are unaffected.
- No controller change: the 401/403/503 semantics and `ConsumeAgentEnrollment`'s label update already exist. No token-format change. k8s-agent already derives identity from enrollment and is out of scope.
- Fallback is intentionally 401-only (definitive token rejection), not 503 (transient).

## Acceptance

- Given a valid persisted credential AND a new enrollment token, the agent consumes the token (calls `/enroll`, not `/refresh`) and its authorized labels update.
- Given a valid persisted credential AND an expired/consumed (401) enrollment token, the agent logs a WARN and continues via the credential; it does not error out.
- A transient (503) enroll failure is retried and surfaced, not silently swallowed as a fallback.
- With `--id` omitted, the agent adopts the enrollment/credential `AgentID`; with `--id` set, a mismatch is a clear error.
- `--id`-set credential paths and behavior are byte-for-byte unchanged.
