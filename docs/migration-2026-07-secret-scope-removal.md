# Migration: secrets are name-only (`scope`/`scopeRef` removed)

This release removes the `scope`/`scopeRef` dimension from secrets. Secrets
are now addressed by **name alone**, everywhere: storage, the REST API, and
`SecretBinding`'s AAD encoding. **This is a breaking change: upgrading
deletes every existing secret row** — see below before you upgrade a fleet
with secrets already registered.

## Why

The `scope`/`scopeRef` columns supported per-scope secret values (e.g. one
`DATABASE_URL` per environment), but the agent fetch path always looked up
`scope='global'` — a non-global secret could be written and was never
readable by any job. It was a half-implemented feature with no working
consumer, so it's removed rather than finished.

## Breaking change: migration 016 clears the `secrets` table

`SecretBinding`'s canonical encoding feeds directly into the AES-GCM
additional authenticated data (AAD) used to decrypt each secret. Dropping
`scope`/`scopeRef` from that encoding changes the AAD for every row,
including ones that only ever used the default `scope: global` — so no
existing ciphertext can be authenticated after upgrade. There is no
in-place re-encryption path, so migration `016_drop_secret_scope` deletes
every row in `public.secrets` before dropping the columns. This is the same
handling migration `015` used for `sessions.refresh_token` when its AAD
binding changed (that migration clears `sessions` and users simply log in
again).

**What you need to do.** After upgrading, `unified-cli secret list` returns
empty — this is expected, not a bug. Re-set every secret your jobs
reference, the same way you originally created them:

```bash
unified-cli secret set DATABASE_URL "postgres://user:pass@host/db"
unified-cli secret set DEPLOY_KEY -f ~/.ssh/id_rsa
echo -n "mysecretpassword" | unified-cli secret set DB_PASSWORD
```

Do this **before** the next scheduled or webhook-triggered run that
references `{{ secrets.NAME }}` — a run referencing an unregistered secret
name now fails outright rather than expanding to an empty value (see
[Secrets Management Guide:
Troubleshooting](secrets.md#troubleshooting)).

If you were relying on the old per-scope model to store more than one value
under the same secret name (e.g. a `DATABASE_URL` scoped differently per
environment), give each value its own distinct name instead — there is no
replacement for the scope dimension itself.

## API surface changes

- `POST /api/v1/secrets/` no longer accepts `scope`/`scopeRef` in the
  request body — only `name` and `value`.
- `GET /api/v1/secrets/` no longer accepts `?scope=`/`?scopeRef=` query
  filters; it always lists every secret.
- `DELETE /api/v1/secrets/{name}` no longer accepts `?scope=`/`?scopeRef=`
  query parameters.
- Secret metadata (`SecretMeta`) no longer includes `scope`/`scopeRef`
  fields in list responses.

The `unified-cli secret set/list/delete` commands were already name-only at
the CLI layer, so no CLI flag or usage change is needed — only fleets
calling the REST API directly with `scope`/`scopeRef` need to drop those
fields.

## Reference

- [Secrets Management Guide](secrets.md) — current (name-only) data model,
  CLI/API usage, and troubleshooting.
- `internal/store/migrations/016_drop_secret_scope.up.sql` — the migration
  itself, including the same reasoning in a code comment.
