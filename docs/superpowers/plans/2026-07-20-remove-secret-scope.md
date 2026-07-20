# Remove the Unused Secret Scope Feature — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Collapse secrets from `(name, scope, scope_ref)` to `name` alone, removing a feature that could be written but never read — the agent fetch path hardcoded `scope="global"`, so any non-global secret was unreachable by a job.

**Architecture:** Drop `scope`/`scope_ref` from the schema, the store layer, the API types, the controller handlers, and the `SecretBinding` AAD. Because the change to `SecretBinding`'s signature and to the store method signatures ripple to the same call sites, it is one cohesive commit rather than several — the build cannot be green with the change half-applied.

**Tech Stack:** Go, PostgreSQL (pgx), paired SQL migrations, testify, dockertest.

## Global Constraints

- Base branch `feat/remove-secret-scope`, worktree `C:/Users/arimax/unified-cd-project/unified-cd-noscope`. Never commit to `main`.
- **This is a breaking change for every existing secret.** `SecretBinding`'s canonical encoding is AES-GCM AAD; removing two fields changes the AAD, so *all* previously stored ciphertext (including `global` rows) fails authentication with `ErrBindingMismatch`. There are no production secrets to preserve, so migration `016` **deletes existing secret rows** rather than re-encrypting them — the same precedent migration `015` set for `sessions.refresh_token`.
- Migration house style (match `013`/`014`): filename `NNN_snake_case.{up,down}.sql`, both files; **no explicit `BEGIN`/`COMMIT`** (the runner wraps each file); `public.`-qualified DDL; idempotent guards; a leading `--` comment explaining why, naming the migration that previously owned the object.
- `SecretBinding` keeps its `kind:"secret"` tag, so it stays distinct from `SessionRefreshBinding`. Only the scope/scopeRef fields go.
- Tests use testify. Run `./scripts/prepare-shim-placeholders.sh` before any whole-tree build; add `-buildvcs=false` on `error obtaining VCS status`.
- Before pushing, run on Linux with `TZ=UTC`.

---

### Task 1: Remove scope everywhere

**Files:**
- Create: `internal/store/migrations/016_drop_secret_scope.up.sql`, `.down.sql`
- Modify: `internal/secrets/binding.go` (`SecretBinding` signature + doc)
- Modify: `internal/secrets/binding_test.go`, `internal/secrets/crypto_test.go`, `internal/secrets/vault/integration_test.go:114`
- Modify: `internal/store/store.go` (4 method signatures, `StoredSecret`, `SecretMeta`)
- Modify: `internal/store/postgres.go:1561-1615` (the 4 SQL methods)
- Modify: `internal/store/postgres_secrets_test.go`
- Modify: `internal/api/types.go` (`SetSecretRequest`, `SecretMeta`)
- Modify: `internal/controller/api_secrets.go` (write/list/delete handlers, agent fetch, 2 binding sites)
- Modify: `internal/controller/api_webhooks.go` (2 GetSecret + 2 binding sites), `internal/controller/scheduler.go` (1+1), `internal/controller/appsource_reconciler.go` (1+1)
- Modify: `internal/controller/api_secrets_test.go` (any scope assertions)

**Interfaces:**
- Produces:
  - `func SecretBinding(name string) Binding`
  - `UpsertSecret(ctx, name string, encryptedDEK, ciphertext []byte) (*StoredSecret, error)`
  - `GetSecret(ctx, name string) (*StoredSecret, error)`
  - `ListSecrets(ctx) ([]SecretMeta, error)`
  - `DeleteSecret(ctx, name string) error`
  - `StoredSecret` and `SecretMeta` without `Scope`/`ScopeRef`

- [ ] **Step 1: Write the migration**

`internal/store/migrations/016_drop_secret_scope.up.sql`:

```sql
-- Migration 001 owns the secrets table. The scope / scope_ref columns
-- supported per-scope secret values, but the agent fetch path always read
-- scope='global' (internal/controller/api_secrets.go), so a non-global secret
-- could be written and never read — a half-implemented feature. Collapse to a
-- name-only model.
--
-- SecretBinding's canonical encoding is AES-GCM additional authenticated data;
-- dropping scope/scope_ref from it changes the AAD, so every existing
-- ciphertext (global rows included) can no longer be authenticated. There are
-- no production secrets to preserve, so the rows are deleted and operators
-- re-set their secrets — the same handling migration 015 used for
-- sessions.refresh_token.
DELETE FROM public.secrets;

ALTER TABLE public.secrets DROP CONSTRAINT IF EXISTS secrets_name_scope_scope_ref_key;
ALTER TABLE public.secrets DROP COLUMN IF EXISTS scope;
ALTER TABLE public.secrets DROP COLUMN IF EXISTS scope_ref;
ALTER TABLE public.secrets ADD CONSTRAINT secrets_name_key UNIQUE (name);
```

`internal/store/migrations/016_drop_secret_scope.down.sql`:

```sql
-- Restore the migration-001 shape. Secrets are cleared in this direction too:
-- the name-only ciphertext cannot be re-bound to a (name, scope, scope_ref)
-- AAD without re-encryption, and leaving rows behind would violate the
-- restored NOT NULL columns.
DELETE FROM public.secrets;

ALTER TABLE public.secrets DROP CONSTRAINT IF EXISTS secrets_name_key;
ALTER TABLE public.secrets ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'global'::text;
ALTER TABLE public.secrets ADD COLUMN IF NOT EXISTS scope_ref text NOT NULL DEFAULT ''::text;
ALTER TABLE public.secrets ADD CONSTRAINT secrets_name_scope_scope_ref_key UNIQUE (name, scope, scope_ref);
```

- [ ] **Step 2: Update `SecretBinding` — write the failing test first**

Rewrite the scope-dependent tests in `internal/secrets/binding_test.go`. The collision test that relied on the name/scope boundary is replaced by one that proves two secret names still cannot collide (the length prefix guarantees it), and the empty-field test is dropped (there is only one field now):

```go
func TestBinding_SecretNamesDoNotCollide(t *testing.T) {
	// The length prefix guarantees two different names encode differently.
	assert.NotEqual(t, SecretBinding("ab").canonical(), SecretBinding("a").canonical())
	assert.NotEqual(t, SecretBinding("a").canonical(), SecretBinding("").canonical())
}

func TestBinding_CanonicalIsStable(t *testing.T) {
	assert.Equal(t, SecretBinding("NAME").canonical(), SecretBinding("NAME").canonical())
}

// A secret and a session-refresh binding with the same field must differ.
func TestBinding_KindsDoNotCollide(t *testing.T) {
	assert.NotEqual(t, SecretBinding("x").canonical(), SessionRefreshBinding("x").canonical())
}

func TestBinding_StringDescribesIdentity(t *testing.T) {
	b := SecretBinding("AWS_KEY")
	require.Contains(t, b.String(), "secret")
	assert.Contains(t, b.String(), "AWS_KEY")
}
```

Delete `TestBinding_CanonicalIsUnambiguous` and `TestBinding_EmptyFieldsAreDistinguished` (they tested multi-field boundaries that no longer exist). Run `go test ./internal/secrets/ -run TestBinding` and confirm it fails to compile (`too many arguments in call to SecretBinding`).

- [ ] **Step 3: Change `SecretBinding` in `binding.go`**

```go
// SecretBinding binds a ciphertext to a secret's name.
func SecretBinding(name string) Binding {
	return Binding{kind: "secret", fields: []string{name}}
}
```

Leave `canonical()`, `appendField`, `String()`, and `SessionRefreshBinding` unchanged — they already handle any field count.

- [ ] **Step 4: Update `crypto_test.go` and the vault integration test**

In `internal/secrets/crypto_test.go`, change every `SecretBinding(name, scope, scopeRef)` to `SecretBinding(name)`. In `internal/secrets/vault/integration_test.go:114`, the same.

Run `go test ./internal/secrets/... -run 'TestBinding|TestEnvelope' -count=1` — all pass.

- [ ] **Step 5: Collapse the store layer**

In `internal/store/store.go`: remove `Scope`/`ScopeRef` from `StoredSecret` and `SecretMeta`, and change the four interface methods to the signatures in the Interfaces block above.

In `internal/store/postgres.go`, rewrite the four methods:

```go
func (p *Postgres) UpsertSecret(ctx context.Context, name string, encryptedDEK, ciphertext []byte) (*StoredSecret, error) {
	const q = `INSERT INTO secrets(name, encrypted_dek, ciphertext)
		VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET encrypted_dek = EXCLUDED.encrypted_dek, ciphertext = EXCLUDED.ciphertext, updated_at = now()
		RETURNING id, name, encrypted_dek, ciphertext, created_at, updated_at`
	var s StoredSecret
	err := p.pool.QueryRow(ctx, q, name, encryptedDEK, ciphertext).
		Scan(&s.ID, &s.Name, &s.EncryptedDEK, &s.Ciphertext, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert secret: %w", err)
	}
	return &s, nil
}

func (p *Postgres) GetSecret(ctx context.Context, name string) (*StoredSecret, error) {
	const q = `SELECT id, name, encrypted_dek, ciphertext, created_at, updated_at
		FROM secrets WHERE name = $1`
	var s StoredSecret
	err := p.pool.QueryRow(ctx, q, name).
		Scan(&s.ID, &s.Name, &s.EncryptedDEK, &s.Ciphertext, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get secret %q: %w", name, err)
	}
	return &s, nil
}

func (p *Postgres) ListSecrets(ctx context.Context) ([]SecretMeta, error) {
	const q = `SELECT id, name, created_at FROM secrets ORDER BY name`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()
	var out []SecretMeta
	for rows.Next() {
		var m SecretMeta
		if err := rows.Scan(&m.ID, &m.Name, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteSecret(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM secrets WHERE name = $1`, name)
	return err
}
```

Read the current `UpsertSecret`/`GetSecret`/`ListSecrets` first — match their exact `StoredSecret`/`SecretMeta` field order and any existing error-wrapping style rather than the illustrative code above where they differ.

Update `internal/store/postgres_secrets_test.go`: drop the `"global", ""` positional args and the `Scope=="global"` assertion.

- [ ] **Step 6: Collapse the API types**

In `internal/api/types.go`, remove `Scope`/`ScopeRef` from `SetSecretRequest` and `SecretMeta`. Leave the unrelated `ScopeID`/`ScopeImage` (agent scope pods) alone.

- [ ] **Step 7: Update the controller handlers and every binding site**

`internal/controller/api_secrets.go`:
- Write handler: drop the `req.Scope == "" → "global"` default; `secrets.Encrypt(..., secrets.SecretBinding(req.Name))`; `UpsertSecret(ctx, req.Name, encDEK, ct)`.
- List handler: drop the `?scope=`/`?scopeRef=` reads; `ListSecrets(ctx)`; the `api.SecretMeta` copy no longer sets scope.
- Delete handler: drop the query reads; `DeleteSecret(ctx, name)`.
- Agent fetch (`:137`): `GetSecret(ctx, name)`; binding `secrets.SecretBinding(stored.Name)`.

The other three decrypt sites — `api_webhooks.go` (2), `scheduler.go` (1), `appsource_reconciler.go` (1) — each call `GetSecret(ctx, ref, "global", "")` then `SecretBinding(stored.Name, stored.Scope, stored.ScopeRef)`. Change each to `GetSecret(ctx, ref)` and `SecretBinding(stored.Name)`.

Update any assertion in `internal/controller/api_secrets_test.go` that referenced scope (most tests already build `SetSecretRequest{Name, Value}` and survive).

- [ ] **Step 8: Build and test**

Run:
```
./scripts/prepare-shim-placeholders.sh
go build ./... && go test ./internal/secrets/... ./internal/store/ ./internal/config/ -count=1
go test ./internal/controller/ -count=1
```
Expected: all pass. Grep to confirm nothing was missed:
```
grep -rn "ScopeRef\|scope_ref\|\.Scope\b" internal/store internal/api internal/controller internal/secrets --include=*.go | grep -iv "scopeid\|scopeimage\|scope pod\|oidc\|gittemplate"
```
Expected: no hits outside the agent-scope-pod and OIDC uses.

- [ ] **Step 9: Commit**

```bash
git add internal/ && git commit -m "feat(secrets): remove the unused scope dimension; secrets are name-only"
```

---

### Task 2: Documentation and migration notes

**Files:**
- Modify: `docs/secrets.md`, `docs/cli.md` if either describes a scope
- Modify: `docs/migration-2026-07-security-hardening.md` or a new migration note, recording that secrets are cleared by migration 016 and must be re-set

- [ ] **Step 1: Check for operator-facing scope references**

```
grep -rn -i "scope" docs/*.md | grep -iv "scopeid\|scope pod\|oidc\|under-scoped\|agent scope\|workload"
```
Update any that describe a *secret* scope. The historical design docs under `docs/superpowers/` describe the old `SecretBinding` signature but are point-in-time records — leave them.

- [ ] **Step 2: Add a migration note**

State plainly that migration 016 drops the scope columns and, because the AAD changes, clears existing secrets — operators re-set them with `unified-cli secret set`. Mirror the tone of the existing security-hardening migration guide.

- [ ] **Step 3: Commit**

```bash
git add docs/ && git commit -m "docs: note that migration 016 clears secrets and removes scope"
```

---

## Final verification

- [ ] Full suite on Linux, `TZ=UTC`:
```
wsl.exe -e bash -lc 'cd /mnt/c/Users/arimax/unified-cd-project/unified-cd-noscope && ./scripts/prepare-shim-placeholders.sh && TZ=UTC go test ./internal/... -count=1'
```
- [ ] Push and open a PR stating the breaking change: migration 016 removes `scope`/`scope_ref` and clears existing secrets (the AAD change makes old ciphertext unreadable); operators re-set their secrets.
