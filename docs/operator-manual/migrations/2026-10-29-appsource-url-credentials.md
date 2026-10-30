# Migrating: no credentials in AppSource repoURL

An AppSource `spec.repoURL` can no longer carry credentials. Embedded userinfo
leaked to anyone who could list AppSources, to the Web UI and to exported
manifests, so it is now refused on write, stripped on read, and scrubbed from
existing rows. Authenticate private repositories with a
[GitCredential](../../user-guide/resources/git-credential.md) instead.

## What changed

| Before | After |
|---|---|
| `repoURL: https://user:pass@github.com/org/repo` or `https://TOKEN@github.com/org/repo` was accepted, stored, returned by `GET /api/v1/appsources`, shown in the Web UI and written by `unified-cli export`. | Apply, create and update are refused with `400`: `repo URL must not embed credentials (userinfo is only allowed as a bare user name on ssh:// and scp-like URLs); store the token or key in a GitCredential for the host instead`. The message never repeats the URL. |
| `ssh://git:pass@host/org/repo` was accepted. | Refused with the same message. `ssh://git@host/org/repo` and `git@host:org/repo` (user name only, no password) are unchanged. |
| Stored rows kept the credential in `spec`. | On controller startup every stored `repoURL` is rewritten without userinfo (all of it for `http(s)`, the password only for `ssh://`), the row's `last_commit` is reset so it re-syncs, and `last_error` is scrubbed. The controller logs one warning listing the affected AppSource names (never the secrets): `removed credentials embedded in AppSource repoURL; create a GitCredential for each repository host so these AppSources can authenticate again`. |
| `repoURL`, `lastError` and error messages in API/CLI/UI output could contain the credential. | Every response redacts them even if a row still holds a credential: `repoURL` is returned without userinfo and URL userinfo in `lastError` becomes `***`. |

## What you must do

1. Before upgrading, find AppSources with embedded credentials:
   `unified-cli appsource list` (any `repoURL` that shows credentials on an
   older server) or search your manifests for `://` followed by `@` before the
   first `/`.
2. Store the token as a secret (`unified-cli secret set ...`) and create a
   `GitCredential` whose `spec.host` is the repository host (for example
   `github.com`). It applies to every AppSource using that host.
3. Change each manifest to the plain URL (`https://github.com/org/repo`).
4. **Rotate the old credential.** It was stored in plaintext and may have been
   visible to other users, the UI and exported files.

An AppSource that relied on embedded `http(s)` credentials and has no matching
GitCredential fails to sync after the upgrade (the rewritten URL is
unauthenticated) until step 2 is done. Nothing else about it changes.
