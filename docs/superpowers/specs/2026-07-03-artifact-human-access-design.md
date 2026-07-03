# Design: Human-facing artifact access (list/download API + CLI + e2e)

**Date:** 2026-07-03
**Status:** Approved (pending implementation plan)

## Problem

Artifacts can be produced and consumed by *jobs* (the agent uploads/downloads
via `PUT`/`GET /api/v1/runs/{runID}/artifacts/{name}` under `BearerAuth(AgentToken)`),
but a **human** operator has no way to see what a run produced or fetch it:

- There is no **list** endpoint — you cannot discover artifact names for a run.
- The **download** endpoint is gated on the agent token only, so a human with a
  PAT / session / OIDC identity is rejected.
- There is no **CLI** surface (`unified-cd artifact …`).
- No **end-to-end** test exercises the full upload → list → download → extract
  round-trip over HTTP (the agent has unit coverage of its own client, but the
  wire round-trip through the controller is unverified).

This closes items B (human API), C (CLI), and E (e2e) from the artifact gap
analysis. (Item A — k8s-agent artifact support — shipped separately via the
workspace sidecar.)

## Goal

Let a human list and download a run's artifacts, through the controller HTTP
API and the CLI, using their normal server credentials — without weakening the
agent-only upload path or the agent's existing download path.

## Scope decisions

| Question | Decision |
|---|---|
| Upload auth | **Unchanged** — `PUT /{name}` stays `BearerAuth(AgentToken)` (agents only). |
| Download auth | **Combined** — `GET /{name}` accepts the agent token OR a human identity (PAT/session/OIDC), so the agent download path AND humans both work. |
| List | New `GET /api/v1/runs/{runID}/artifacts` (combined auth), returns the artifact names for the run as JSON. |
| List source of truth | `objStore.List("artifacts/{runID}/")` — derive names by stripping the `artifacts/{runID}/` prefix and the `.tar.gz` suffix. No DB table (object store is authoritative). |
| List response shape | `[]api.ArtifactInfo{{Name string}}` — a struct (not bare `[]string`) so size/mtime can be added later without breaking the wire. Empty ⇒ `[]`, never `null`. |
| CLI | `unified-cd artifact list <run-id>` and `unified-cd artifact download <run-id> <name> [--dest .]`, authenticating with the configured server token. |
| Shared extractor | Move the agent's unexported `extractTarZstd` (with its path-traversal guard) into a new `internal/artifact` package as exported `ExtractTarZstd`; the agent client and the CLI both call it. One implementation of the security-sensitive extractor. |
| e2e auth | The round-trip test uses the **agent token** (which combined auth accepts) so it needs **no database** and runs on Windows. ServerAuth (human) acceptance is covered by the controller handler unit test. |

## Non-goals (YAGNI / separate)

- UI (item D) — not in scope.
- Artifact deletion / retention / TTL — separate concern.
- Size/mtime in the list response — the struct leaves room; not populated now.
- A DB index of artifacts — the object store `List` is authoritative and simple.
- Per-artifact ACLs beyond "authenticated to this server" — out of scope.

## Design

### 1. Combined auth middleware — `internal/controller/auth.go`

Add a middleware that accepts the agent static token OR any `ServerAuth`
identity, so a single route can serve both agents and humans:

```go
// AgentOrServerAuth allows the agent static token (constant-time compare) OR,
// failing that, any identity ServerAuth accepts (PAT / OIDC / session).
func AgentOrServerAuth(agentToken string, st store.Store, srv *Server) func(http.Handler) http.Handler {
    server := ServerAuth(st, srv)
    expected := []byte(agentToken)
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            h := r.Header.Get("Authorization")
            if strings.HasPrefix(h, bearerPrefix) {
                got := []byte(strings.TrimPrefix(h, bearerPrefix))
                if len(expected) != 0 && subtle.ConstantTimeCompare(got, expected) == 1 {
                    next.ServeHTTP(w, r) // agent token — no principal
                    return
                }
            }
            server(next).ServeHTTP(w, r) // fall back to human auth
        })
    }
}
```

### 2. Restructure artifact routes — `internal/controller/server.go`

Mirror the existing per-route-auth pattern already used by `/api/v1/agents`:

```go
s.r.Route("/api/v1/runs/{runID}/artifacts", func(r chi.Router) {
    r.With(BearerAuth(s.cfg.AgentToken)).Put("/{name}", s.handleArtifactUpload)
    r.With(AgentOrServerAuth(s.cfg.AgentToken, s.store, s)).Get("/{name}", s.handleArtifactDownload)
    r.With(AgentOrServerAuth(s.cfg.AgentToken, s.store, s)).Get("/", s.handleArtifactList)
})
```

### 3. List handler — `internal/controller/api_artifacts.go`

```go
// handleArtifactList handles GET /api/v1/runs/{runID}/artifacts.
func (s *Server) handleArtifactList(w http.ResponseWriter, r *http.Request) {
    if s.objStore == nil {
        http.Error(w, "object store not configured", http.StatusServiceUnavailable)
        return
    }
    runID := chi.URLParam(r, "runID")
    prefix := fmt.Sprintf("artifacts/%s/", runID)
    keys, err := s.objStore.List(r.Context(), prefix)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    out := make([]api.ArtifactInfo, 0, len(keys))
    for _, k := range keys {
        name := strings.TrimSuffix(strings.TrimPrefix(k, prefix), ".tar.gz")
        if name == "" || name == k { // skip keys that don't match the expected shape
            continue
        }
        out = append(out, api.ArtifactInfo{Name: name})
    }
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(out)
}
```

New type in `internal/api`:

```go
// ArtifactInfo describes one artifact of a run.
type ArtifactInfo struct {
    Name string `json:"name"`
}
```

### 4. Shared extractor — `internal/artifact/targz.go`

Move `extractTarZstd` verbatim (including the path-traversal guard) from
`internal/agent/client.go` into a new package, exported:

```go
package artifact

// ExtractTarZstd extracts a tar+zstd stream into dest.
// Includes path checks to prevent path-traversal attacks.
func ExtractTarZstd(r io.Reader, dest string) error { /* moved body */ }
```

`internal/agent/client.go` `DownloadArtifact` calls `artifact.ExtractTarZstd`.
No import cycle: `internal/artifact` imports only stdlib + `klauspost/compress/zstd`.

### 5. CLI — `internal/cli/artifact.go`

`newArtifactCmd(resolve)` with two subcommands, registered in `root.go`:

```go
// unified-cd artifact list <run-id>
//   GET {server}/api/v1/runs/{run-id}/artifacts  (Bearer cfg.Token)
//   → decode []api.ArtifactInfo, print one name per line.
//
// unified-cd artifact download <run-id> <name> [--dest .]
//   GET {server}/api/v1/runs/{run-id}/artifacts/{name}  (Bearer cfg.Token)
//   → artifact.ExtractTarZstd(resp.Body, dest)
```

Follow the existing `approvals.go` command shape (`newXCmdWithClient(resolve,
httpClient)` for testability with `httptest`). Non-2xx ⇒ error with body+status.

### 6. Tests

- **Controller unit** (`api_artifacts_test.go`): list returns the run's names as
  JSON; empty run ⇒ `[]`; `PUT` still rejects a non-agent token; `GET /{name}`
  and `GET /` accept the agent token; `GET` with no/invalid auth ⇒ 401. Where a
  human (ServerAuth) principal is needed, use the package's existing store test
  harness / a seeded PAT.
- **artifact package unit** (`targz_test.go`): a tar+zstd stream extracts to the
  expected files; a `../escape` entry is rejected (guard). (Move any existing
  agent extractor test here.)
- **CLI unit** (`artifact_test.go`): `list` prints names from a stubbed server;
  `download` writes the extracted file into `--dest`; non-2xx ⇒ error. Use
  `httptest`.
- **e2e** (`test/e2e/artifact_roundtrip_test.go`): stand up the controller
  router with a `LocalObjectStore` behind `httptest.Server`; using the agent
  token, `PUT` an artifact, `GET /` and assert the name appears, `GET /{name}`
  and `ExtractTarZstd` and assert the file content round-trips. No DB; runs on
  all platforms.

## Touch points

| Path | Change |
|---|---|
| `internal/controller/auth.go` | `AgentOrServerAuth` middleware |
| `internal/controller/server.go` | per-route auth for the artifacts route group + list route |
| `internal/controller/api_artifacts.go` | `handleArtifactList` |
| `internal/api/*.go` | `ArtifactInfo` type |
| `internal/artifact/targz.go` (new) | exported `ExtractTarZstd` (moved) |
| `internal/agent/client.go` | call `artifact.ExtractTarZstd`; drop local copy |
| `internal/cli/artifact.go` (new), `internal/cli/root.go` | `artifact list`/`download` |
| `internal/controller/api_artifacts_test.go`, `internal/artifact/targz_test.go`, `internal/cli/artifact_test.go`, `test/e2e/artifact_roundtrip_test.go` | tests |
| `docs/jobs.md` (Artifacts section) | document the list/download API + CLI |

## Acceptance criteria

- `GET /api/v1/runs/{runID}/artifacts` returns the run's artifact names as JSON
  (`[]` when none); `GET /{name}` and the list accept either the agent token or a
  human identity; `PUT /{name}` remains agent-only.
- `unified-cd artifact list <run-id>` and `download <run-id> <name> [--dest]`
  work against a running controller.
- `extractTarZstd` lives once in `internal/artifact.ExtractTarZstd`, used by both
  the agent and the CLI; the path-traversal guard is preserved and tested.
- An e2e test round-trips an artifact upload → list → download → extract over
  HTTP with no database, passing on the local platform.
- `go build ./...` and the full `go test ./...` pass.
