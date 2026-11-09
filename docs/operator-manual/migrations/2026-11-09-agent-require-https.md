# Migrating: host agents refuse a plain-http controller URL

The host (VM) agent used to accept any `--server` URL, including `http://`, and sent its one-time
enrollment token and its refresh/access credentials to it in cleartext. The Kubernetes agent has
required `https://` (unless `allowInsecureHTTP`) for some time; the host agent now does the same.

| `--server` / `UNIFIED_SERVER` | Before | Now |
|---|---|---|
| `https://...` | Accepted. | Accepted. |
| `http://localhost...`, `http://127.0.0.1...`, `http://[::1]...` | Accepted. | Accepted. |
| `http://<any other host>` | Accepted. | The agent exits with `invalid --server ... uses plain http` unless the opt-in below is set. |
| Anything that is not an absolute `http(s)` URL | Failed later at connect time. | Exits at startup with `server must be an absolute URL` or `server must use https://`. |

## What you must do

Preferably put TLS in front of the controller and switch to its `https://` URL. If the controller
is only reachable over plain http on a network you trust (a compose network, a VM-local bridge),
opt in explicitly:

- Flag: `--allow-insecure-http`
- Environment: `UNIFIED_ALLOW_INSECURE_HTTP=true`
- Config file: `allowInsecureHTTP: true`

The shipped `docker-compose.yaml` files and test rigs set the opt-in for their agents already.
