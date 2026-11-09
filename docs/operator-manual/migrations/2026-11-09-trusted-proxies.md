# Migrating: forwarded client addresses are ignored unless the proxy is trusted

The controller used to rewrite the client address from `X-Forwarded-For` / `X-Real-IP` on every
request, whoever sent it. A client could therefore put any address in those headers to escape the
per-client rate limits on agent enrollment and token refresh, or to fill a victim's address bucket
and lock that client out. The controller now only honours those headers when the **TCP peer** is
listed in the new `trustedProxies` setting, which is empty by default.

| Deployment | Before | Now |
|---|---|---|
| Controller reachable directly (no proxy) | Client address could be forged with a header. | Peer address is used. Nothing to do. |
| Controller behind an Ingress / load balancer, `trustedProxies` not set | Per-client rate limits and the access log used the forwarded address. | Every request is attributed to the proxy's address, so all clients share one rate-limit bucket and the access log shows the proxy. Set `trustedProxies`. |
| Controller behind a proxy, `trustedProxies` set to the proxy's range | n/a | Forwarded address is used for requests that arrive from that range. |

## What you must do

If a reverse proxy, Ingress controller or load balancer is in front of the controller, add its
address range:

- Config file: `trustedProxies: ["10.244.0.0/16"]`
- Environment: `UNIFIED_TRUSTED_PROXIES=10.244.0.0/16`
- Flag: `--trusted-proxies 10.244.0.0/16`
- Helm: `--set 'controller.trustedProxies[0]=10.244.0.0/16'`

Use the range the proxy connects **from** (the Ingress controller pod CIDR, or the node range when
traffic is source-NATed). An invalid entry stops the controller at startup with
`invalid --trusted-proxies`. See [Reverse Proxy, Client IPs and Request Limits](../reverse-proxy.md).

The audit log does not record client addresses, so no audit data changes.
