# Frontend Development Guide

The unified-cd UI is built with **Svelte + Vite**.

## Directory Layout

Illustrative — see `web/src` for the current, complete set of files.

```
web/                         # Svelte + Vite project
├── index.html               # Vite entry point
├── package.json
├── vite.config.js
└── src/
    ├── main.js              # mount
    ├── App.svelte           # nav + router
    ├── app.css              # global styles
    ├── lib/
    │   ├── api.js           # Svelte stores, apiFetch
    │   ├── theme.js         # theme (light/dark) store + toggle
    │   └── utils.js         # statusBadge, fmtTime, etc.
    ├── components/
    │   └── AuthSetup.svelte # Bearer token / OIDC SSO input
    └── routes/
        ├── JobList.svelte
        ├── JobDetail.svelte
        ├── ...              # see the Routing table below for the full list
        └── AgentMonitor.svelte
```

`web/dist/` (Vite's build output directory) is **not committed** — it is
built fresh for each deployment mode; see Production Build below.

## Dev Setup

```bash
cd web
npm install
```

## Development (two options)

### Option A: Local dev (recommended)

Open **2 terminals** and run one command in each.

**Terminal 1 — Go backend (hot-reload with air)**

```bash
# Install air first
go install github.com/air-verse/air@latest

make dev-go   # starts on :8080
```

**Terminal 2 — Svelte dev server (Vite HMR)**

```bash
make dev-ui   # starts on :5173
```

Open `http://localhost:5173/ui/` in your browser. The page updates instantly whenever you save a `.svelte` file.

Requests to `/api` and `/webhook` are automatically proxied to `:8080` (see `vite.config.js`).

### Option B: Docker Compose

```bash
# First time only
docker compose build

# Start
docker compose up -d
docker compose logs -f  # watch logs

# Then open
http://localhost:5173/ui/
```

Vite auto-reloads on `.svelte` saves (HMR). Air auto-rebuilds and restarts on Go code changes.

**Stop**
```bash
docker compose down
```

## Production Build

There is **no `go:embed`** — the UI is not compiled into the controller
binary. The controller serves the built UI at **runtime** from a directory on
disk, and the Go binaries are built independently of the frontend:

```bash
make build
# builds web/dist (Vite) and the Go binaries (cmd/controller, cmd/unified-cd-agent, ...)
# as separate, independent artifacts — neither embeds the other
```

Run the controller pointing `--web-dir` (or `UNIFIED_WEB_DIR`) at the built
`web/dist` directory (see `internal/controller/server.go`,
`cmd/controller/main.go`):

```bash
cd web && npm run build     # produces web/dist
./controller --web-dir ./web/dist ...
```

The Docker image (`docker/controller.Dockerfile`) builds `web/dist` in a
Node stage and copies it into the runtime image, setting
`UNIFIED_WEB_DIR=/ui` so the controller serves it without any extra flags.

## How It Works

unified-cd's controller can serve the UI three ways, selected by
`--web-dir` / `UNIFIED_WEB_DIR` and `--ui-proxy-target` / `UNIFIED_UI_PROXY_TARGET`
(`internal/controller/server.go`):

```
1. Local dev (Option A):
   Browser → :5173/ui/*  → Vite dev server (HMR)
                  /api/*  → proxy → :8080 (Go)

2. Production / Docker image:
   Browser → :8080/ui/*  → Go http.FileServer over --web-dir (built web/dist)
             :8080/api/* → Go API

3. Docker Compose dev stack:
   Browser → :8080/ui/*  → Go reverse-proxies to UI_PROXY_TARGET (a live Vite dev server)
             :8080/api/* → Go API
```

Mode 3 is used when `--web-dir`/`UNIFIED_WEB_DIR` is **empty** but
`--ui-proxy-target`/`UNIFIED_UI_PROXY_TARGET` is set: the Go server reverse-proxies
every `/ui/*` request to that target instead of serving files itself, so
`docker compose up` can point the controller at the Vite dev server container
and still get HMR through the controller's own port. If neither is set,
`/ui/*` returns `404`.

Vite's `base: '/ui/'` setting makes built asset URLs `/ui/assets/...`, matching the Go server's `/ui` prefix.

## Routing

Hash routing via `svelte-spa-router` (`#/` format).

| URL | Component |
|-----|-----------|
| `#/` | JobList |
| `#/jobs/:name` | JobDetail |
| `#/jobs/:name/run` | JobRun |
| `#/jobs/:name/yaml` | JobYaml |
| `#/runs/:id` | RunDetail |
| `#/runs/:id/yaml` | RunYaml |
| `#/agents` | AgentMonitor |
| `#/agents/:id` | AgentDetail |
| `#/tokens` | TokenList |
| `#/resources/schedules` | ScheduleList |
| `#/resources/webhooks` | WebhookList |
| `#/resources/gitcredentials` | GitCredentialList |
| `#/resources/appsources` | AppSourceList |
| `#/resources/secrets` | SecretList |

(See `web/src/App.svelte` for the authoritative route table.)

## Adding a New Page

1. Create `web/src/routes/MyPage.svelte`
2. Add the path to the `routes` object in `web/src/App.svelte`
3. If a nav link is needed, add `<a href="#/my-page">` to `<nav>`

## API Calls

Use `apiFetch` from `web/src/lib/api.js`. It handles auth header injection, 401→SSO redirect, and error handling.

```js
import { apiFetch } from '../lib/api.js';

const jobs = await apiFetch('/api/v1/jobs');
await apiFetch('/api/v1/runs', { method: 'POST', body: JSON.stringify({ jobName, params }) });
```

## Permissions in the UI

The server authorizes every request. The UI only decides what to **show**, from
the caller's grants, and a UI control is **never the only enforcement**: a
hidden button that is called anyway (devtools, a stale page, a crafted request)
gets the same `403`/`404` the CLI would, and the UI renders it as an ordinary
error without retrying.

### Where the grants come from

`GET /api/v1/auth/me` returns the caller's effective grants under
`permissions` (`{global: [...], folders: {"team-a": [...]}}`; folder keys
appear only where a grant matched the caller). `web/src/lib/api.js` loads it
into the `currentUser` store and exposes the derived `grants` store, and it
re-reads `/auth/me` once after any `403` so a page left open across a config
change converges without a reload.

### `can()` and `allows()`

`web/src/lib/authz.js` mirrors the Go rule (`internal/authz`
`Grants.Can`): permission `P` is held on job `J` if it is in `global` or in the
grants of any proper ancestor folder of `J` (`team-a` covers `team-a/x` and
`team-a/sub/x`, not `team-ab/x`, and not a root job named `team-a`).

- `can(grants, permission, jobName)` is the exact check.
- `canAnywhere(grants, permission)` asks whether the permission is held
  globally or on any folder (use it for navigation and global pages).
- `allows(grants, permission, jobName?)` is what components call. While the
  grants are not yet known (`null`) it is permissive so the page renders as it
  used to and the server decides; with a job name it calls `can`, without one
  `canAnywhere`.
- `requiresText(permission, jobName?)` builds the tooltip, for example
  `requires run.approve on team-a/prod`.

### The shared fixture

The JS rule and the Go rule are tested against one file,
`web/src/lib/authz-cases.json`, a list of `(grants, permission, job) -> want`
cases. `internal/authz/grants_test.go` (`TestCanFixture`) and
`web/src/lib/authz.test.js` both read it, so the file is the specification of
the prefix and union rule. Change the rule in both languages and add a case.

### Hide versus disable

- A control for an action the caller could never take anywhere is **hidden**:
  navigation to Secrets, GitCredentials, AppSources, Webhooks and Schedules, the
  Agents and Tokens links (`allows($grants, 'secret.list')`, ...).
- A per-job control the caller lacks on *this* job is **disabled** with a
  tooltip naming the permission, so a user can tell why the button is greyed
  out: the Run link on a job, Cancel, Rerun and Approve/Reject on a run. The
  token page offers only ceilings the caller holds.
- Lists (jobs, active runs, agent run history, schedules) come from filtered
  server responses, so unreadable jobs never reach the browser; a direct link to
  an unreadable job or run is a `404` and renders the ordinary not-found view.

### The UI action inventory

`web/src/lib/ui-actions.json` lists every state-changing call the UI makes:
file, action, method, route and permission. `web/src/lib/ui-actions.test.js`
scans the source for `method:` assignments and fails when one is missing from
the inventory, and `TestUIActionInventoryIsEnforced`
(`internal/controller/ui_actions_test.go`) proves each listed route is
classified and refuses a caller without its permission. **When you add a UI
action, add its row** to the inventory, gate the control with `allows()`, and
make sure the route is in `routeTable`.
