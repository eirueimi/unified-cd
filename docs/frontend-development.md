# Frontend Development Guide

The unified-cd UI is built with **Svelte + Vite**.

## Directory Layout

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
    │   └── utils.js         # statusBadge, fmtTime, etc.
    ├── components/
    │   └── AuthSetup.svelte # Bearer token / OIDC SSO input
    └── routes/
        ├── JobList.svelte
        ├── JobDetail.svelte
        ├── JobRun.svelte
        ├── RunDetail.svelte
        └── AgentMonitor.svelte

internal/controller/web/     # Vite build output (go:embed target)
├── index.html
└── assets/
```

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

When development is done, build the UI for embedding into the Go binary:

```bash
make ui-build
# or
cd web && npm run build
```

HTML, CSS, and JS are output to `internal/controller/web/`. They are embedded into the binary at `go build` time.

`make build` calls `ui-build` internally, so normally `make build` alone is sufficient.

## How It Works

```
Browser → :5173/ui/*  → Vite dev server
                /api/*  → proxy → :8080 (Go)

Production:
Browser → :8080/ui/*  → Go (serves index.html + assets via go:embed)
          :8080/api/* → Go API
```

Vite's `base: '/ui/'` setting makes built asset URLs `/ui/assets/...`, matching the Go server's `/ui` prefix.

## Routing

Hash routing via `svelte-spa-router` (`#/` format).

| URL | Component |
|-----|-----------|
| `#/` | JobList |
| `#/jobs/:name` | JobDetail |
| `#/jobs/:name/run` | JobRun |
| `#/runs/:id` | RunDetail |
| `#/agents` | AgentMonitor |

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
