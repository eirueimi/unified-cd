# OIDC Claim Handling for Direct IdP Setups (TODO 49) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close TODO 49 (OIDC claim and scope handling for direct IdP setups) with three changes:
- email matching respects `email_verified`;
- Entra ID groups overage is detected and reported;
- `rolesClaim` can name a nested claim.

**Architecture:** All three live in the controller's claim decoding (`internal/controller/auth_oidc.go` `oidcIdentity`, `internal/controller/rbac.go` `extractRoleValues`). Every login and bearer path already goes through those two functions, so no other code needs to change.

**Tech Stack:** Go.

**Spec:** Agreed in conversation on 2026-11-20 (small change, no separate spec). The user's decisions:
1. **`email_verified`:**
   - When the ID token carries `email_verified` and it is `false` (a JSON boolean false, or the string `"false"`), the email is not used for matching. It is unusable for `userMap` and for `permissions.*.users`.
   - When the claim is absent, behaviour is unchanged.
2. **Entra ID groups overage:**
   - Detection: the token has no `rolesClaim` value and carries the overage marker, either `_claim_names` containing the key `groups`, or `hasgroups: true`.
   - Then the controller logs a warning (once per login) and lets the login continue with no group values.
   - The docs explain the Entra fix: emit only "Groups assigned to the application" in the token.
   - There is no Microsoft Graph call.
3. **Nested `rolesClaim`:**
   - Look up `rolesClaim` as an exact top-level key first, so namespaced claims such as `https://example.com/roles` keep working.
   - Only if no such key exists, treat it as a `.`-separated path into nested objects, for example Keycloak's `realm_access.roles`.

## Global Constraints

- **Repo rules.** English only, no PII (AGENTS.md). Docs must stay consistent with the code. Commit with `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit`. Every message ends with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV
  ```
- **No config key added or removed. No change for IdPs that send neither `email_verified: false` nor an overage marker, and whose `rolesClaim` is a top-level key.**
- **Exact log string:** `oidc: the identity provider reported a groups overage; group-based roles and permissions are not applied for this login (see identity-providers.md "Entra ID")`, with the attributes `sub` and `rolesClaim`. Never log the email or the token.
- **Fallback when the email is unusable.** `oidcIdentity` already falls back to `sub` when `email` is empty. Keep that: an unverified email is treated as empty, so matching falls back to `sub`.

## Review Focus

1. **`email_verified` as a string** (some IdPs send `"true"`/`"false"`). Both forms must work. Any other type (number, object) counts as absent, which means today's behaviour.
2. **A namespaced claim with dots that also happens to resolve as a path.** The exact key wins.
3. **A nested path whose leaf is a string, an array or an object.** A string or an array of strings works, as at the top level. An object or a missing segment yields nil.
4. **The overage marker alongside a non-empty groups claim.** No warning is logged, and the groups that are present are used.
5. **Refreshed sessions and bearer id_tokens** go through the same functions. Check `refreshedClaims` and `verifyBearerClaims` callers.

---

### Task 1: email_verified, groups overage, nested rolesClaim

**Files:**
- Modify:
  - `internal/controller/rbac.go:91-117` (`extractRoleValues`)
  - `internal/controller/auth_oidc.go:519` (`oidcIdentity`)
  - the overage warning, placed where the login, refresh and bearer paths all pass (`oidcIdentity` is the natural place)
- Test: `internal/controller/rbac_test.go` and the existing `oidcIdentity` tests (or a new `auth_oidc_claims_test.go`)
- Docs:
  - `docs/operator-manual/identity-providers.md`:
    - the Keycloak section: `rolesClaim: realm_access.roles` works directly, and the flat-mapper workaround becomes optional;
    - the Entra section: the overage behaviour and the fix, with the exact log line;
    - an `email_verified` note.
  - `docs/operator-manual/authorization.md`: `userMap` and `permissions.*.users` email matching requires a verified email when the IdP reports it.
  - `docs/reference/configuration.md`: the `rolesClaim` description covers the exact-key-then-path lookup.
  - `TODO.md`: remove item 49.

- [ ] **Step 1: Write failing tests.**
  - **`extractRoleValues`:**
    - `{"realm_access":{"roles":["admin"]}}` with `realm_access.roles` gives `["admin"]`.
    - `{"https://example.com/roles":["dev"]}` with `https://example.com/roles` gives `["dev"]` (exact key).
    - With both an exact key `a.b` and nested `{"a":{"b":...}}` present, the exact key wins.
    - A missing segment gives nil.
    - A leaf that is an object gives nil.
    - A leaf string gives a single value.
    - The default `groups` is unchanged.
  - **`oidcIdentity`:**
    - `email_verified: false` (bool) gives email unusable, falling back to sub.
    - `"false"` (string) gives the same.
    - `true`, `"true"` or absent: the email is used.
    - `email_verified: 1` (number) is treated as absent, so the email is used.
  - **Admission:** a `userMap` entry for the email does not admit a token with `email_verified: false`. The same entry keyed by sub still admits.
  - **Overage:**
    - Claims `{"_claim_names":{"groups":"src1"},"_claim_sources":{...}}` with `rolesClaim: groups` log the exact warning once, give no group values, and do not fail the login.
    - `{"hasgroups":true}` behaves the same.
    - The marker plus a non-empty `groups` claim gives no warning.
    - Capture the log and assert that neither the email nor any token appears.
- [ ] **Step 2: Run the tests and confirm they fail.**

  ```bash
  go test ./internal/controller/ -run 'ExtractRole|OIDCIdentity|Overage|EmailVerified|Admission'
  ```

  Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** Expected: PASS.

  ```bash
  go test ./internal/controller/
  go build ./... && go vet ./...
  mkdocs build --strict   # venv per docs/requirements.txt; delete .venv-docs and site/ after
  ```
- [ ] **Step 5: Commit.**

  ```text
  feat(oidc): honour email_verified, report Entra groups overage, nested rolesClaim
  ```
