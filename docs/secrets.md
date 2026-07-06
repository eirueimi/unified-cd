# Secrets Management Guide

This document covers how to create, use, and manage secrets in unified-cd.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Creating and Managing Secrets (CLI)](#creating-and-managing-secrets-cli)
- [Referencing Secrets in Job YAML](#referencing-secrets-in-job-yaml)
- [Automatic Log Masking](#automatic-log-masking)
- [Security Model](#security-model)
- [Troubleshooting](#troubleshooting)

---

## Overview

Secrets are a key-value store saved in the controller's PostgreSQL database **encrypted with AES-256-GCM**.

- Reference them in job `env:` or `run:` fields using the `{{ secrets.NAME }}` syntax.
- Values are sent to the agent in plaintext at runtime, but **log output is automatically masked**.
- There is no API endpoint to retrieve values — only names and metadata are returned.

---

## Prerequisites

### Setting `UNIFIED_CONTROLLER_KEY` (required)

This is the master encryption key for secrets. **If unset, the controller generates a key once and persists it in the database (`controller_settings`), so secrets survive restarts; set the variable explicitly in production so the key can be backed up.**

```bash
# Generate once and store
openssl rand -hex 32
# => e.g. a1b2c3d4...  (64 hex characters)
```

Set it in your `.env` file or as an environment variable:

```bash
UNIFIED_CONTROLLER_KEY=a1b2c3d4...
```

In HA setups, use the same value across all replicas (see [HA Guide](high-availability.md)).

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
  │   └─ encryption key = UNIFIED_CONTROLLER_KEY (KEK: Key Encryption Key)
  └─ Ciphertext (body encrypted with the DEK)
```

- As long as `UNIFIED_CONTROLLER_KEY` is not leaked, a DB leak does not expose plaintext.
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

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| `key manager not configured` | Controller started without `UNIFIED_CONTROLLER_KEY`. Set it and restart. |
| `{{ secrets.NAME }}` appears unexpanded | The secret name doesn't match a registered secret, or contains a character other than alphanumerics/underscores/hyphens. Check the exact name with `unified-cli secret list`. |
| Secret is referenced with `{{ secrets.NAME }}` but value is empty | Secret is not registered, or the name casing does not match. Check with `unified-cli secret list`. |
| Info log: `controllerKey not set — generated a new key and persisted it to the database` | `UNIFIED_CONTROLLER_KEY` is not set. A key was auto-generated and stored in the `controller_settings` table (reused on subsequent restarts). To manage it explicitly, retrieve the value from the DB and set it as `UNIFIED_CONTROLLER_KEY`. |
| `decrypt` errors in HA setup | Replicas are pointing to different DBs (`controller_settings`), or `UNIFIED_CONTROLLER_KEY` differs between replicas. Use the same DB and the same key value on all replicas. |
| `secret set` from CI returns `unauthorized` | The token in use does not have admin privileges (agent tokens cannot manage secrets). |
