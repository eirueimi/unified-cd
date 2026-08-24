# Webhooks

## Webhook returns 401

**Symptom** — one of these `signature verification failed: …` messages:

```
signature verification failed: secret "<name>" not found — create it with ...
signature verification failed: secret "<name>" is empty — set a non-empty value ...
signature verification failed: missing X-Hub-Signature-256 header — GitHub sends it only when ...
signature verification failed: X-Hub-Signature-256 does not match — the "<name>" secret differs ...
```

**Cause**

The receiver's `spec.auth.type` is `hmac-sha256` or `github`, and signature
verification failed. The message names the specific reason:

- **`secret "<name>" not found`** — no secret with that `secretRef` exists.
- **`secret "<name>" is empty`** — the secret exists but its value is empty.
  This commonly happens when the value was piped in without one (e.g.
  `echo | unified-cli secret set <name>`), or set with an empty string.
- **`missing … header`** — no signature header arrived. For GitHub, this means
  the webhook has **no Secret configured** (GitHub only sends
  `X-Hub-Signature-256` when a Secret is set).
- **`… does not match`** — a signature arrived but the HMAC differs: the stored
  secret differs from the sender's, or the raw body was altered in transit.

**Fix**

- Set the secret with the **two-argument form**, which does not add a trailing
  newline, and use the *exact same value* on the sender:
  ```bash
  unified-cli secret set <name> '<value>'
  ```
  Avoid `echo "<value>" | unified-cli secret set <name>` — `echo` appends a
  `\n`, so the stored secret won't match the sender's. Use `echo -n` if you must
  pipe.
- For GitHub receivers, set the webhook **Secret** field to that same value and
  set **Content type** to `application/json` (a form-encoded body is signed the
  same on both sides but changes the bytes the receiver hashes).
- `hmac-sha256` receivers accept either `X-Signature: sha256=<hex>` or the
  GitHub-compatible `X-Hub-Signature-256: sha256=<hex>`; `github` receivers only
  check `X-Hub-Signature-256`. Confirm the sender uses the expected header.
- The signature must be computed over the **exact raw request body bytes** —
  re-encoding the JSON (key order, whitespace) before signing produces a
  different HMAC and this same error.
- To isolate whether the stored secret is the problem, sign a test body with the
  value you *think* is stored and POST it directly; if that succeeds, the
  mismatch is on the sender's side:
  ```bash
  SECRET='<value-you-think-is-stored>'
  BODY='{"ref":"refs/heads/main"}'
  SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | sed 's/^.* //')"
  curl -i -X POST http://<controller>/webhook/<name> \
    -H 'Content-Type: application/json' -H "X-Hub-Signature-256: $SIG" -d "$BODY"
  ```
- See [Resource Reference: WebhookReceiver](../user-guide/resources/webhook-receiver.md#webhookreceiver) for
  the full auth field table and delivery response codes.

## Webhook returns 400 `invalid JSON payload`

**Symptom**

```
invalid JSON payload
```

A GitHub delivery fails with `400` even though the `Secret` is correct (the
signature check passed).

**Cause**

The receiver parses the raw request body as JSON. GitHub only sends raw JSON
when the webhook's **Content type** is `application/json`. With the other option,
`application/x-www-form-urlencoded`, GitHub wraps the payload as
`payload=<url-encoded JSON>` — the signature still verifies (it is computed over
the raw body on both sides), but that body is not valid JSON, so parsing fails.

**Fix**

- On GitHub, open **Settings → Webhooks →** *(your hook)* and set **Content
  type** to `application/json`, then **Redeliver** from Recent Deliveries.
- For non-GitHub senders, POST the JSON body directly (do not form-encode it)
  with `Content-Type: application/json`.
- See the [Getting Started webhook walkthrough](../getting-started/quickstart.md#configuring-the-webhook-on-github).

## Webhook returns 400 `missing required param`

**Symptom**

```
missing required param: image
```

**Cause**

The target job declares a `required: true` input (e.g. `image`), and the
receiver's `spec.paramsMapping` either omits that key entirely or maps it from
a payload field that isn't present in the delivered body — either way, no
value resolves for a required input.

**Fix**

- Add (or correct) a `paramsMapping` entry for every required input:
  ```yaml
  spec:
    paramsMapping:
      image: "{{ .Payload.repository.name }}"
  ```
- If a required input can reasonably default, give it a `default` in the job
  instead of requiring every caller to supply it.
- Test the mapping by POSTing a representative payload to the receiver and
  confirming the response is `200` with a run, not `400`.
- See [Resource Reference: WebhookReceiver](../user-guide/resources/webhook-receiver.md#webhookreceiver) for
  the full delivery response table (`200` / `204` / `401` / `400`).

