# Agent ID-Scoped Credential Path Design

**Date:** 2026-07-26

## Context

The host agent supports omitting `--id`. A valid enrollment token is bound to
the canonical agent ID, and the agent adopts that ID from the enrollment
response.

The current startup flow derives a shared default credential path before the
agent ID is known:

```text
$HOME/.unified-cd/credential.json
```

That shared path creates two problems:

1. multiple agents running as the same operating-system user can read or
   overwrite the same credential; and
2. a persisted credential can supply an old ID before a newly supplied
   enrollment token is exchanged, causing the token's different server-bound
   ID to be rejected even though `--id` was omitted.

The credential path must be ID-scoped without making `--id` mandatory.

## Goals

- Keep `--id` optional.
- Treat a valid explicit enrollment token as the authority for the effective
  agent ID.
- Store every default credential at
  `$HOME/.unified-cd/<agent-id>/credential.json`.
- Prevent one agent process from deleting, replacing, or selecting another
  agent's credential.
- Preserve strict ID validation when the operator explicitly supplies `--id`.
- Preserve explicit `--credential-file` behavior.
- Provide deterministic behavior when more than one ID-scoped credential is
  present.

## Non-Goals

- Automatically migrate or delete the legacy shared
  `$HOME/.unified-cd/credential.json`.
- Automatically choose among multiple ID-scoped credentials.
- Add a shared "current agent" pointer file.
- Coordinate multiple agents through a process-wide or filesystem lock.

## Identity and Credential Precedence

Startup uses the following precedence.

### 1. Explicit enrollment token

When `--enrollment-token`, `UNIFIED_AGENT_ENROLLMENT_TOKEN`, stdin token input,
or `--enrollment-token-file` supplies a token, the agent exchanges that token
before loading an implicitly selected persisted credential.

On a successful exchange:

1. the controller response supplies the canonical `AgentID`;
2. if `--id` was explicitly supplied, the response ID must match it;
3. otherwise, the response ID becomes the effective agent ID;
4. if `--credential-file` was explicitly supplied, the credential is persisted
   there;
5. otherwise, the credential is persisted at
   `$HOME/.unified-cd/<response-agent-id>/credential.json`.

The agent does not inspect, modify, retire, or delete credentials belonging to
other IDs during this flow.

### 2. Explicit local identity

Without an enrollment token:

- `--credential-file` selects exactly that file.
- Otherwise, `--id <id>` selects
  `$HOME/.unified-cd/<id>/credential.json`.

Existing validation continues to reject a credential whose embedded agent ID
does not match an explicitly supplied `--id`.

### 3. Implicit local identity

Without an enrollment token, `--id`, or `--credential-file`, the agent scans
only immediate child paths matching:

```text
$HOME/.unified-cd/<agent-id>/credential.json
```

- No candidates: startup fails with the existing missing-credentials error.
- One candidate: the agent loads it and adopts its embedded agent ID.
- More than one candidate: startup fails without modifying any candidate and
  instructs the operator to set `--id` or `--credential-file`.

The legacy shared `$HOME/.unified-cd/credential.json` is not a candidate.

## Rejected Enrollment Token Fallback

The existing fallback for an expired or consumed enrollment token remains
available only when a persisted credential can be selected unambiguously.

After the controller rejects the token with HTTP 401:

- an explicit `--credential-file` may be used;
- an explicit `--id` may select its ID-scoped default credential;
- with neither flag, exactly one discovered ID-scoped credential may be used;
- with zero or multiple discovered credentials, startup fails instead of
  selecting an unrelated identity.

HTTP 403, rate-limit responses, server errors, and network errors retain their
existing non-fallback behavior.

## Credential Path Resolution

Default path resolution has two separate operations:

1. deriving an ID-scoped path from a known canonical agent ID; and
2. discovering a single existing ID-scoped credential when no identity input
   is available.

Deriving a default path from an empty ID is invalid. The configuration layer no
longer returns the shared credential path for an empty ID.

Discovery ignores:

- the legacy shared credential;
- files outside the immediate ID directory level;
- backup, temporary, or unrelated files; and
- directories without an active `credential.json`.

The existing credential reader remains responsible for owner and permission
validation after selection.

## Persistence and Failure Handling

Once enrollment returns an ID, the agent creates the corresponding ID directory
with owner-only permissions and uses the existing atomic credential replacement
logic.

The effective ID and access token are not exposed to the run loop until the new
refresh credential is durable.

If persistence fails, startup fails and no other credential is changed. A
successful write for one ID never triggers cleanup of another ID's path.

## Concurrency

Processes with explicit IDs or credential files operate on disjoint paths.

Two first-time processes with different valid enrollment tokens can start
concurrently. Each token response determines a different ID-scoped destination,
so neither process selects or removes the other's credential.

When multiple ID-scoped credentials already exist, an ID-less process without a
valid enrollment token fails as ambiguous. This is deliberate: there is no safe
local signal that identifies which agent the process intends to run.

## Breaking Change

The legacy shared path is no longer read automatically. Operators must either:

- enroll again with a valid token;
- move an existing credential to its ID-scoped path; or
- temporarily select it with `--credential-file`.

The migration documentation will show how to read the non-secret `agentId`
field, create the owner-only destination directory, and move the file without
printing its refresh token.

## Error Messages

The implementation should provide stable, actionable errors for these cases:

```text
agent ID is required to derive the default credential file path
multiple default agent credential files found; set --id or --credential-file
```

Existing explicit-ID mismatch errors remain unchanged.

## Test Strategy

Regression tests will cover:

1. an ID-less first enrollment persists to the ID returned by the controller;
2. a valid enrollment token with a different ID takes precedence over an
   existing implicitly discoverable credential;
3. an explicitly supplied ID still rejects a mismatched enrollment response;
4. one ID-scoped credential is discovered and adopted without `--id`;
5. multiple ID-scoped credentials fail without modifying either file;
6. the legacy shared credential is ignored by discovery;
7. HTTP 401 falls back only when the local credential is unambiguous;
8. explicit credential paths retain their current behavior; and
9. concurrent enrollment destinations are isolated by returned agent ID.

Tests will be added before production changes and observed failing for the
missing behavior.

## Documentation

The implementation updates:

- `docs/agents.md`;
- `docs/configuration.md`;
- `docs/troubleshooting.md`;
- a breaking-change migration guide;
- `README.md` where it describes default agent enrollment; and
- relevant examples and templates if searches find credential-path guidance.

