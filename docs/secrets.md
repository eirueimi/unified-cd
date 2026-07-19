# Secrets Management Guide

This document covers how to create, use, and manage secrets in unified-cd.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Creating and Managing Secrets (CLI)](#creating-and-managing-secrets-cli)
- [Referencing Secrets in Job YAML](#referencing-secrets-in-job-yaml)
- [Automatic Log Masking](#automatic-log-masking)
- [Security Model](#security-model)
- [Using Vault or OpenBao (Transit)](#using-vault-or-openbao-transit)
- [Troubleshooting](#troubleshooting)

---

## Overview

Secrets are a key-value store saved in the controller's PostgreSQL database **encrypted with AES-256-GCM**.

- Reference them in job `env:` or `run:` fields using the `{{ secrets.NAME }}` syntax.
- Values are sent to the agent in plaintext at runtime, but **log output is automatically masked**.
- There is no API endpoint to retrieve values — only names and metadata are returned.

---

## Prerequisites

### Setting `UNIFIED_CONTROLLER_KEY_FILE` (required)

This is the master encryption key for secrets. The controller refuses to start unless it is
given a key: a file (`UNIFIED_CONTROLLER_KEY_FILE`), an external KMS (`UNIFIED_KMS_URI` — see
[Using Vault or OpenBao (Transit)](#using-vault-or-openbao-transit) below), or an explicit
throwaway key for local development (`UNIFIED_DEV_MODE=1`, unreadable after a restart).

```bash
# Generate once and store
unified-cli keygen --out /etc/unified-cd/kek
```

Point the controller at the file:

```bash
UNIFIED_CONTROLLER_KEY_FILE=/etc/unified-cd/kek
```

In HA setups, every replica must be given the same key file (or the same `UNIFIED_KMS_URI`) —
see [HA Guide](high-availability.md).

---

## Creating and Managing Secrets (CLI)

### Create / Update

Three ways to supply the value. The operation is **idempotent** (overwrites if the name already exists).

```bash
# 1) Value as a direct argument
unified-cli secret set DATABASE_URL "postgres://user:pass@host/db"

# 2) From a file (good for SSH keys, certificates, or values with newlines)
unified-cli secret set DEPLOY_KEY -f ~/.ssh/id_rsa

# 3) From stdin (avoids leaving the value in shell history)
echo -n "mysecretpassword" | unified-cli secret set DB_PASSWORD
# Or interactively
read -s SECRET && echo -n "$SECRET" | unified-cli secret set DB_PASSWORD
```

> **Naming rules**
>
> Secret names must contain only alphanumerics, underscores, and hyphens and must start with a
> letter or `_` (regex `^[A-Za-z_][A-Za-z0-9_-]*$`). This is enforced at creation — an invalid
> name is rejected with HTTP 400 — so a stored name is always resolvable from a template.
> Both `{{ secrets.NAME }}` and `{{ .Secrets.NAME }}` work with hyphenated names —
> the template engine automatically rewrites hyphenated references to an index lookup
> internally, since Go template dot-notation cannot address a map key containing a hyphen
> directly.
>
> ✓ Valid: `DATABASE_URL`, `deploy_key`, `API_KEY_PROD`, `slack-webhook-url`, `gitlab-token`

### List

Values are never shown — only names and creation dates.

```bash
unified-cli secret list
# => DATABASE_URL   (2026-06-01)
#    DEPLOY_KEY     (2026-06-01)
#    API_KEY_PROD   (2026-06-10)
```

### Delete

```bash
unified-cli secret delete DATABASE_URL
# => secret "DATABASE_URL" deleted
```

### REST API

You can also operate directly via the API instead of the CLI.

```bash
# Create / update
curl -X POST http://localhost:8080/api/v1/secrets/ \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"DATABASE_URL","value":"postgres://..."}'

# List
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/secrets/

# Delete
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/secrets/DATABASE_URL
```

---

## Referencing Secrets in Job YAML

### Basic syntax

Write `{{ secrets.SECRET_NAME }}` inside `env:` values or `run:` strings.

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: deploy
spec:
  steps:
    - name: deploy
      env:
        DATABASE_URL: "{{ secrets.DATABASE_URL }}"
        API_KEY: "{{ secrets.API_KEY_PROD }}"
      run: |
        ./deploy.sh
```

### Using env: (recommended)

Passing secrets as environment variables is the safest approach. Putting secrets in
command arguments can expose them in process listings.

```yaml
steps:
  - name: docker-login
    env:
      REGISTRY_USER: "{{ secrets.REGISTRY_USER }}"
      REGISTRY_PASS: "{{ secrets.REGISTRY_PASS }}"
    run: |
      echo "$REGISTRY_PASS" | docker login registry.example.com \
        -u "$REGISTRY_USER" --password-stdin
```

### Using run: directly

You can also expand secrets directly in the `run:` string (use with care).

```yaml
steps:
  - name: configure
    run: |
      git config --global url."https://{{ secrets.GIT_TOKEN }}@github.com/".insteadOf "https://github.com/"
```

### How it works

Before dispatching a job to an agent, the controller scans all `env:` and `run:` strings
for `secrets.NAME` patterns, collects only the secrets needed, and fetches them.
You don't need to declare them separately — just write `{{ secrets.X }}` and they are fetched automatically.

```
Write {{ secrets.X }} in env/run of the Job YAML
         │
         ▼ (controller scans at dispatch time)
SecretsNeeded: ["X"] included in the claim response
         │
         ▼ (agent fetches)
POST /api/v1/agents/{id}/secrets/fetch → {X: "plaintext value"}
         │
         ▼
Template expansion + log masker setup → step execution
```

---

## Automatic Log Masking

If a secret value appears in log output, the agent automatically replaces it with `***`.
All three of the following forms are masked:

| Form | Example |
|------|---------|
| Plaintext | `mysecretpassword` → `***` |
| Base64 encoded | `bXlzZWNyZXRwYXNzd29yZA==` → `***` |
| URL encoded | `mysecret%40password` → `***` |

> Masking is best-effort. Encoding forms not covered (e.g. `echo $SECRET | base32`) may not be masked.
> Do not write commands that intentionally print secret values.

---

## Security Model

### Encryption structure

Each secret is protected by **two layers of encryption**:

```
Plaintext
  │ encrypted with AES-256-GCM
  ▼
Ciphertext (stored in DB)
  │
  ├─ DEK (Data Encryption Key): randomly generated, encrypted with AES-GCM
  │   └─ encryption key = the KEK (Key Encryption Key) from UNIFIED_CONTROLLER_KEY_FILE / UNIFIED_KMS_URI
  └─ Ciphertext (body encrypted with the DEK)
```

- As long as the KEK is not leaked, a DB leak does not expose plaintext. The KEK itself is never
  stored in the database.
- Each secret has its own DEK, so compromise of one DEK has limited blast radius.

### Access control

| Actor | Access |
|-------|--------|
| Admin (static token / PAT / OIDC) | Create, delete ✓ (**cannot retrieve values**) |
| Developer or Admin | List names ✓ (values never shown) |
| Agent (agent token) | Fetch only the secrets needed for a run ✓ |
| External API / browser | Retrieve values ✗ (no endpoint exists) |

Secret **values cannot be retrieved via the API** by design. If you lose a value, re-register it.

---

## Using Vault or OpenBao (Transit)

The controller wraps each data-encryption key with Transit, so the key-encryption
key never leaves the KMS.

> The commands below use the `vault` CLI. OpenBao's CLI is `bao`, with
> identical subcommands — swap only the binary name (e.g. `bao secrets enable
> transit`).

Enable the engine and create the key:

```sh
vault secrets enable transit
vault write -f transit/keys/unified-cd-kek
```

The controller needs two capabilities, plus two optional ones:

```hcl
path "transit/encrypt/unified-cd-kek" {
  capabilities = ["update"]
}

path "transit/decrypt/unified-cd-kek" {
  capabilities = ["update"]
}

# Needed to keep a token alive by renewal rather than only by re-login (see
# "Tokens need renewing, not rotating" below) — keymanager.go calls this via
# RenewSelfWithContext for every auth method, not just token auth. Vault's
# built-in `default` policy already grants this, so it only needs to be
# listed explicitly for a token minted with `-no-default-policy`, which is
# exactly what granting a token *only* the capabilities in this block (as the
# Kubernetes section below recommends) amounts to.
path "auth/token/renew-self" {
  capabilities = ["update"]
}

# Optional (token auth only): lets the renewal loop learn the token's real
# TTL and renewability instead of the zero values a bare token carries. See
# "Tokens need renewing, not rotating" below for what is lost without it.
path "auth/token/lookup-self" {
  capabilities = ["read"]
}
```

Then configure the controller:

```sh
UNIFIED_KMS_URI=hashivault://unified-cd-kek
UNIFIED_VAULT_ADDR=https://vault.example.com:8200
UNIFIED_VAULT_TOKEN_FILE=/run/secrets/vault-token
```

### Tokens need renewing, not rotating

A **periodic** token's TTL resets to its configured period on every renewal, so
the controller keeps it alive indefinitely on its own and it never needs to be
replaced. A token that is not periodic dies at its max TTL (32 days by default)
however often it is renewed, and must genuinely be replaced — put it in a file
(`UNIFIED_VAULT_TOKEN_FILE`) and the controller picks up the replacement on its
next login without a restart, because the file is read fresh every time.

This is also why `read` on `auth/token/lookup-self` is worth granting even
though it is optional: without it the controller cannot see the token's actual
TTL or whether Vault considers it renewable, so it logs a warning at startup
and falls back to re-authenticating on its normal cadence instead of renewing
proactively. The controller still works — it just leans on re-login instead of
renewal, and an operator loses early visibility into an unexpectedly short-lived
token.

### Kubernetes

On Kubernetes, use the Kubernetes auth method instead: the pod's ServiceAccount
token is already projected by the kubelet, so there is no credential to
distribute or rotate. The Kubernetes login response carries the token's TTL
directly, so this path does not need — and does not perform — the
`auth/token/lookup-self` call that token auth optionally makes; grant only the
two Transit capabilities above.

```sh
UNIFIED_VAULT_AUTH=kubernetes
UNIFIED_VAULT_AUTH_PARAM=role=unified-cd
```

See also [High Availability Guide: Vault / OpenBao](high-availability.md#vault--openbao-when-unified_kms_uri-is-used)
for what changes when Vault runs in HA.

---

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| Controller exits at startup naming `unified-cli keygen --out` | No key source is configured. Set `UNIFIED_CONTROLLER_KEY_FILE` (or `UNIFIED_KMS_URI`, or `UNIFIED_DEV_MODE=1` for local development) and restart. |
| `{{ secrets.NAME }}` appears unexpanded, or the run fails with a "secret not found" / decrypt error | The secret name doesn't match a registered secret (or the name casing doesn't match), or contains a character other than alphanumerics/underscores/hyphens. An unregistered or unresolvable secret now fails the run rather than expanding to an empty value. Check the exact name with `unified-cli secret list`. |
| `decrypt` errors in HA setup | Replicas were given different key files, or different `UNIFIED_KMS_URI` values. Give every replica the identical key file (or the same KMS URI). |
| `secret set` from CI returns `unauthorized` | The token in use does not have admin privileges (agent tokens cannot manage secrets). |
