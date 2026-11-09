# Reverse Proxy, Client IPs and Request Limits

This page covers what the controller does when it sits behind an Ingress, a load balancer or any
other reverse proxy: where it takes the client address from, which limits it applies per client,
and which headers and body sizes the proxy has to pass through.

## Client address and trusted proxies

The controller uses the client address for the per-client rate limits (agent enrollment, token
refresh) and in the `remoteAddr` field of the access log. By default it is the **TCP peer
address** of the connection. The `X-Forwarded-For` and `X-Real-IP` headers are **ignored**,
because any client can send them: honouring them from an arbitrary peer lets a caller choose the
address it is rate limited under (and lets it exhaust the budget of a victim's address).

Behind a proxy the peer address is the proxy's, so every client looks the same and shares one
rate-limit bucket. Tell the controller which peers are your proxies with `trustedProxies`:

```yaml
# controller config file
trustedProxies:
  - 10.244.3.17        # one Ingress controller Pod (a bare IP means a single host)
  - 192.0.2.0/28       # a load balancer's address range
```

| Form | Value |
|---|---|
| Config file | `trustedProxies: [CIDR, ...]` |
| Environment | `UNIFIED_TRUSTED_PROXIES=10.244.3.17,192.0.2.0/28` (comma-separated) |
| Flag | `--trusted-proxies 10.244.3.17,192.0.2.0/28` |
| Helm | `controller.trustedProxies: ["10.244.3.17/32"]` |

When the TCP peer is inside a trusted prefix the controller reads `X-Forwarded-For` from the
**right**: it skips every hop that is itself a trusted proxy and takes the first address that is
not (anything further left was supplied by the client and is never used). If there is no usable
`X-Forwarded-For`, it falls back to `X-Real-IP`. A malformed hop stops the walk and the peer
address is kept. When the peer is **not** trusted, both headers are ignored.

Only list the addresses of the proxies themselves. Anything inside a listed prefix can choose the
client address it is attributed to, so listing a range that includes clients (for example
`0.0.0.0/0`) turns the spoofing protection back off.

**What the proxy must do.** The controller believes what a trusted peer sends, so the proxy has
to produce clean headers: it must **append** the peer address it saw to `X-Forwarded-For` (nginx:
`proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`; ingress-nginx does this by
default) and it must **overwrite or strip** `X-Real-IP` (nginx: `proxy_set_header X-Real-IP
$remote_addr;`). A proxy that forwards a client-supplied `X-Real-IP` or an unmodified
`X-Forwarded-For` untouched lets the client choose its address. Hops may be written as `ip`,
`ip:port` or `[v6]:port`.

### Kubernetes (Helm)

Set `controller.trustedProxies` to the addresses of the **Ingress controller Pods** (or of the
load balancer that reaches the controller), as narrowly as you can: a dedicated ingress node
pool's CIDR, a `/32` per Pod, or the LB's range. With `externalTrafficPolicy: Cluster` the
source address is rewritten by kube-proxy to a node address, so list the node range that does
that.

**Do not trust the cluster pod CIDR** (for example `10.244.0.0/16`): trusting the whole cluster pod CIDR trusts every Pod in it, including job Pods that run untrusted CI code. Any of them
can call the controller Service directly with a forged `X-Forwarded-For`, which brings back the
rate-limit bypass `trustedProxies` exists to close. If Ingress Pod IPs are not stable enough to
list, trust a narrow range and add a NetworkPolicy that allows only the Ingress controller's
namespace (and your agents) to reach the controller port. With the value empty the chart prints a
note after install when an Ingress is enabled.

## Rate limits

| Endpoint | Key | Limit |
|---|---|---|
| `POST /api/v1/agents/enroll`, `POST /api/v1/agents/token/refresh` | provider, client address, policy | burst 5, then one request per 6 seconds |
| `POST /api/v1/store-credentials` | client address | burst 200, then 20 requests per second (configurable) |

A rejected request gets `429 Too Many Requests`. The counters are per controller replica and are
kept in memory (4096 keys, least recently used evicted first).

`/api/v1/store-credentials` is unauthenticated (the projected ServiceAccount token in the body is
the credential and is checked with a Kubernetes TokenReview), so the limit protects the cluster API
from being hammered through it. A job's sidecar asks once per artifact or cache step.

**Shared client address.** Job sidecars that reach the controller through an Ingress, a load
balancer or SNAT all arrive from the same address (with `trustedProxies` unset, the proxy's) and
share one bucket. A large parallel job (for example a 64-way matrix with cache and artifact steps)
issues a few hundred requests in seconds, which is why the defaults are generous. Tune them with
`storeCredentialRateLimit` (`burst`, `perSecond`), `UNIFIED_STORE_CREDENTIAL_RATE_BURST` /
`UNIFIED_STORE_CREDENTIAL_RATE_PER_SECOND`, or `--store-credential-rate-burst` /
`--store-credential-rate-per-second`. The limit is per controller replica. The sidecar also
retries a `429` up to 6 times with a bounded wait that honours `Retry-After`, so a brief overrun
delays the step instead of failing it. Sidecars that use the in-cluster Service URL arrive from
their own Pod IPs and do not share a bucket.

## Request body limits

Every request body is capped, so an unauthenticated caller cannot make the controller buffer an
arbitrarily large JSON document. A body over its limit is rejected with `413 Request Entity Too
Large`.

| Routes | Limit | Setting |
|---|---|---|
| All API routes, including the unauthenticated `/api/v1/agents/enroll`, `/api/v1/agents/token/refresh` and `/api/v1/store-credentials` | 1 MiB | `maxRequestBodyBytes` / `UNIFIED_MAX_REQUEST_BODY_BYTES` / `--max-request-body-bytes` |
| Webhook ingress `POST /webhook/{name}` | 1 MiB | `UNIFIED_WEBHOOK_MAX_BODY_BYTES` / `--webhook-max-body-bytes` |
| Agent log routes (`/api/v1/agents/{id}/logs`, `.../logs/bulk`) | 16 MiB (or `maxRequestBodyBytes` if larger) | not configurable |
| Artifact upload `PUT /api/v1/runs/{id}/artifacts/{name}` | none (streamed to the object store) | none |

Step and `call:` outputs an agent reports travel as one JSON request, so their combined size is
bounded by `maxRequestBodyBytes` (1 MiB by default); an agent that exceeds it logs
`store step outputs failed` at WARN and the outputs are not recorded.

Applying a very large Job, Secret or Vars document needs `maxRequestBodyBytes` raised. A reverse
proxy in front of the controller has its own limit as well (nginx `client_max_body_size`, default
1 MiB); it must allow chunked request bodies and the artifact size you expect.
