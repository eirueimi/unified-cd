# Human-facing Artifact Access Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a human list and download a run's artifacts through the controller HTTP API and the CLI, using normal server credentials, without weakening the agent-only upload path.

**Architecture:** Add a combined auth middleware (agent token OR human PAT/OIDC/session), restructure the artifacts route group to per-route auth, add a list handler backed by `objStore.List`, extract the tar+zstd extractor into a shared `internal/artifact` package used by both the agent and a new CLI `artifact` command, and cover the wire round-trip with an e2e test.

**Tech Stack:** Go, chi router, cobra CLI, klauspost/compress (zstd), archive/tar, httptest.

## Global Constraints

- Upload (`PUT /{name}`) MUST stay agent-only (`BearerAuth(s.cfg.AgentToken)`). Only download + list gain combined auth.
- Object store key layout is authoritative and fixed: `artifacts/{runID}/{name}.tar.gz`. Do NOT add a DB table for artifacts.
- List response is `[]api.ArtifactInfo` (JSON array of `{"name": "..."}`); empty ⇒ `[]`, never `null`.
- The path-traversal guard in the extractor MUST be preserved verbatim when moved.
- Agent token comparison MUST be constant-time (`crypto/subtle.ConstantTimeCompare`), matching `BearerAuth`.
- `go build ./...` and `go test ./...` must pass after every task.
- Module path is `github.com/eirueimi/unified-cd`.

---

### Task 1: Shared tar+zstd extractor package

Move the agent's unexported `extractTarZstd` into a new shared package so the CLI can reuse the exact same (security-sensitive) extractor. This task is first because it is a pure refactor with no behavior change and both later consumers depend on it.

**Files:**
- Create: `internal/artifact/targz.go`
- Create: `internal/artifact/targz_test.go`
- Modify: `internal/agent/client.go` (replace local `extractTarZstd` with a call to `artifact.ExtractTarZstd`; remove the moved function and now-unused imports)

**Interfaces:**
- Produces: `func ExtractTarZstd(r io.Reader, dest string) error` in package `artifact` (import path `github.com/eirueimi/unified-cd/internal/artifact`). Extracts a tar+zstd stream into `dest`; rejects any entry whose cleaned path escapes `dest`.

- [ ] **Step 1: Write the failing test**

Create `internal/artifact/targz_test.go`:

```go
package artifact

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// makeTarZstd builds a tar+zstd stream from name->content entries.
func makeTarZstd(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarZstd_RoundTrip(t *testing.T) {
	dest := t.TempDir()
	data := makeTarZstd(t, map[string]string{"a.txt": "hello", "sub/b.txt": "world"})
	if err := ExtractTarZstd(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("a.txt = %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dest, "sub", "b.txt"))
	if err != nil || string(got) != "world" {
		t.Fatalf("sub/b.txt = %q, %v", got, err)
	}
}

func TestExtractTarZstd_RejectsTraversal(t *testing.T) {
	dest := t.TempDir()
	data := makeTarZstd(t, map[string]string{"../escape.txt": "evil"})
	if err := ExtractTarZstd(bytes.NewReader(data), dest); err == nil {
		t.Fatal("expected path-traversal rejection, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/artifact/`
Expected: FAIL — package/function does not exist (`undefined: ExtractTarZstd` / no Go files).

- [ ] **Step 3: Write the implementation**

Create `internal/artifact/targz.go` by moving the body from `internal/agent/client.go` (lines ~302-346) verbatim, renamed and exported:

```go
// Package artifact provides the shared tar+zstd artifact wire-format helpers
// used by both the agent and the CLI.
package artifact

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ExtractTarZstd extracts a tar+zstd stream into dest.
// Includes path checks to prevent path-traversal attacks.
func ExtractTarZstd(r io.Reader, dest string) error {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()

	cleanDest := filepath.Clean(dest) + string(filepath.Separator)
	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		target := filepath.Join(dest, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target+string(filepath.Separator), cleanDest) {
			return fmt.Errorf("invalid path %q in artifact archive", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}
```

- [ ] **Step 4: Update the agent client to use it**

In `internal/agent/client.go`:
1. Delete the local `extractTarZstd` function (the whole `func extractTarZstd(...) { ... }` block, ~lines 302-346).
2. Change the call site in `DownloadArtifact` from `return extractTarZstd(resp.Body, destDir)` to `return artifact.ExtractTarZstd(resp.Body, destDir)`.
3. Add the import `"github.com/eirueimi/unified-cd/internal/artifact"`.
4. Remove imports that are now unused in `client.go` ONLY IF no longer referenced anywhere else in the file (`archive/tar`, `errors`, `os`, `path/filepath`, `strings`, `github.com/klauspost/compress/zstd`). Verify each with a search before removing — several are likely still used by the upload path. Let `goimports`/the compiler guide you; do NOT remove an import still in use.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/artifact/ ./internal/agent/`
Expected: PASS. Then `go build ./...` — expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/artifact/ internal/agent/client.go
git commit -m "refactor: extract ExtractTarZstd into shared internal/artifact package"
```

---

### Task 2: Combined auth middleware + list/download API

Add the `AgentOrServerAuth` middleware, restructure the artifacts route group to per-route auth, and add the list handler + `api.ArtifactInfo` type.

**Files:**
- Modify: `internal/controller/auth.go` (add `AgentOrServerAuth`)
- Modify: `internal/controller/server.go:310-314` (restructure the artifacts route group)
- Modify: `internal/controller/api_artifacts.go` (add `handleArtifactList`)
- Create/Modify: `internal/api/artifact.go` (add `ArtifactInfo`)
- Create: `internal/controller/api_artifacts_test.go`

**Interfaces:**
- Consumes: `ServerAuth(st store.Store, srv *Server)`, `BearerAuth(expected string)`, `bearerPrefix`, `s.objStore` (`objectstore.ObjectStore` with `List(ctx, prefix) ([]string, error)` and `Put`), `s.store`, `chi.URLParam`.
- Produces:
  - `func AgentOrServerAuth(agentToken string, st store.Store, srv *Server) func(http.Handler) http.Handler`
  - `func (s *Server) handleArtifactList(w http.ResponseWriter, r *http.Request)` serving `GET /api/v1/runs/{runID}/artifacts`
  - `type ArtifactInfo struct { Name string `json:"name"` }` in package `api`

- [ ] **Step 1: Write the failing test**

Create `internal/controller/api_artifacts_test.go`. Use the package's existing test helpers for building a `Server` with an in-memory/local object store — inspect a neighboring `*_test.go` (e.g. how other controller tests construct `Server` + `objStore`) and reuse that harness. The test must assert:

```go
package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
)

// newArtifactTestServer returns a Server whose router is wired and whose objStore
// is a usable in-memory/local store, plus the agent token. Reuse the existing
// controller test harness for constructing Server; only the object store + agent
// token matter here. (Adapt to the real harness signature found in the package.)
func newArtifactTestServer(t *testing.T) (*Server, string)

func TestArtifactList_ReturnsNames(t *testing.T) {
	s, agentToken := newArtifactTestServer(t)
	// upload two artifacts via the agent PUT path
	for _, name := range []string{"build", "logs"} {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/runs/run1/artifacts/"+name, strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+agentToken)
		rr := httptest.NewRecorder()
		s.r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("put %s: %d", name, rr.Code)
		}
	}
	// list with the agent token (combined auth accepts it)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run1/artifacts", nil)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d (%s)", rr.Code, rr.Body.String())
	}
	var got []api.ArtifactInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := map[string]bool{}
	for _, a := range got {
		names[a.Name] = true
	}
	if !names["build"] || !names["logs"] {
		t.Fatalf("missing names: %v", got)
	}
}

func TestArtifactList_EmptyIsArrayNotNull(t *testing.T) {
	s, agentToken := newArtifactTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/empty/artifacts", nil)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	if strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("empty list body = %q, want []", rr.Body.String())
	}
}

func TestArtifactDownload_RejectsNoAuth(t *testing.T) {
	s, _ := newArtifactTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run1/artifacts/build", nil)
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth download = %d, want 401", rr.Code)
	}
}

func TestArtifactUpload_RejectsNonAgentToken(t *testing.T) {
	s, _ := newArtifactTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/runs/run1/artifacts/build", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer not-the-agent-token")
	rr := httptest.NewRecorder()
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad-token upload = %d, want 401", rr.Code)
	}
}
```

If the existing harness cannot construct a `Server` with a working object store without a DB, wire `newArtifactTestServer` to set `s.objStore` to a `LocalObjectStore` (see `internal/objectstore`) pointed at `t.TempDir()`, and set `s.cfg.AgentToken` to a known value, and call whatever the package uses to register routes (`s.routes()` / the constructor). Keep the DB nil — these tests only exercise object-store + agent-token paths.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestArtifact`
Expected: FAIL — `handleArtifactList`, `ArtifactInfo`, and the list route do not exist.

- [ ] **Step 3: Add the `ArtifactInfo` type**

Create `internal/api/artifact.go` (or add to an existing api file if artifacts already have one — search first):

```go
package api

// ArtifactInfo describes one artifact produced by a run.
type ArtifactInfo struct {
	Name string `json:"name"`
}
```

- [ ] **Step 4: Add the combined auth middleware**

In `internal/controller/auth.go` add (imports `crypto/subtle`, `strings`, `net/http`, `github.com/eirueimi/unified-cd/internal/store` are already present or add as needed):

```go
// AgentOrServerAuth allows the agent static token (constant-time compare) OR,
// failing that, any identity ServerAuth accepts (PAT / OIDC / session). Used for
// artifact download + list, which both agents and humans need.
func AgentOrServerAuth(agentToken string, st store.Store, srv *Server) func(http.Handler) http.Handler {
	server := ServerAuth(st, srv)
	expected := []byte(agentToken)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if strings.HasPrefix(h, bearerPrefix) {
				got := []byte(strings.TrimPrefix(h, bearerPrefix))
				if len(expected) != 0 && subtle.ConstantTimeCompare(got, expected) == 1 {
					next.ServeHTTP(w, r)
					return
				}
			}
			server(next).ServeHTTP(w, r)
		})
	}
}
```

- [ ] **Step 5: Add the list handler**

In `internal/controller/api_artifacts.go` add (ensure imports `encoding/json`, `strings`, and `github.com/eirueimi/unified-cd/internal/api` are present):

```go
// handleArtifactList handles GET /api/v1/runs/{runID}/artifacts.
// Lists artifact names for the run from the object store.
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
		if name == "" || name == k {
			continue
		}
		out = append(out, api.ArtifactInfo{Name: name})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
```

- [ ] **Step 6: Restructure the route group**

In `internal/controller/server.go`, replace the artifacts route block (currently lines ~310-314):

```go
	s.r.Route("/api/v1/runs/{runID}/artifacts", func(r chi.Router) {
		r.Use(BearerAuth(s.cfg.AgentToken))
		r.Put("/{name}", s.handleArtifactUpload)
		r.Get("/{name}", s.handleArtifactDownload)
	})
```

with:

```go
	s.r.Route("/api/v1/runs/{runID}/artifacts", func(r chi.Router) {
		r.With(BearerAuth(s.cfg.AgentToken)).Put("/{name}", s.handleArtifactUpload)
		r.With(AgentOrServerAuth(s.cfg.AgentToken, s.store, s)).Get("/{name}", s.handleArtifactDownload)
		r.With(AgentOrServerAuth(s.cfg.AgentToken, s.store, s)).Get("/", s.handleArtifactList)
	})
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run TestArtifact`
Expected: PASS (all four). Then `go build ./...` — clean.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/auth.go internal/controller/server.go internal/controller/api_artifacts.go internal/controller/api_artifacts_test.go internal/api/
git commit -m "feat(controller): human-facing artifact list + combined-auth download"
```

---

### Task 3: CLI `artifact list` / `artifact download`

Add a `unified-cd artifact` command group with `list` and `download` subcommands.

**Files:**
- Create: `internal/cli/artifact.go`
- Create: `internal/cli/artifact_test.go`
- Modify: `internal/cli/root.go` (register `newArtifactCmd(resolve)`)

**Interfaces:**
- Consumes: `Config{Server, Token}`, `resolve func() (Config, error)`, `api.ArtifactInfo`, `artifact.ExtractTarZstd`.
- Produces: `func newArtifactCmd(resolve func() (Config, error)) *cobra.Command` and an internal `newArtifactCmdWithClient(resolve, *http.Client)` for tests (mirroring `approvals.go`).

- [ ] **Step 1: Write the failing test**

Create `internal/cli/artifact_test.go`:

```go
package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/klauspost/compress/zstd"
)

func tarZstd(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func TestArtifactList_PrintsNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/run1/artifacts" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]api.ArtifactInfo{{Name: "build"}, {Name: "logs"}})
	}))
	defer srv.Close()

	resolve := func() (Config, error) { return Config{Server: srv.URL, Token: "t"}, nil }
	cmd := newArtifactCmdWithClient(resolve, srv.Client())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list", "run1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "build") || !strings.Contains(out.String(), "logs") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestArtifactDownload_ExtractsToDest(t *testing.T) {
	payload := tarZstd(t, "hello.txt", "hi")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runs/run1/artifacts/build" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dest := t.TempDir()
	resolve := func() (Config, error) { return Config{Server: srv.URL, Token: "t"}, nil }
	cmd := newArtifactCmdWithClient(resolve, srv.Client())
	cmd.SetArgs([]string{"download", "run1", "build", "--dest", dest})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("hello.txt = %q, %v", got, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestArtifact`
Expected: FAIL — `newArtifactCmdWithClient` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/artifact.go`:

```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/spf13/cobra"
)

func newArtifactCmd(resolve func() (Config, error)) *cobra.Command {
	return newArtifactCmdWithClient(resolve, http.DefaultClient)
}

func newArtifactCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "artifact",
		Short: "List and download run artifacts",
	}
	cmd.AddCommand(newArtifactListCmd(resolve, httpClient))
	cmd.AddCommand(newArtifactDownloadCmd(resolve, httpClient))
	return cmd
}

func newArtifactListCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list <run-id>",
		Short: "List artifacts for a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			url := cfg.Server + "/api/v1/runs/" + args[0] + "/artifacts"
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %s (%d)", string(b), resp.StatusCode)
			}
			var list []api.ArtifactInfo
			if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
				return err
			}
			for _, a := range list {
				fmt.Fprintln(cmd.OutOrStdout(), a.Name)
			}
			return nil
		},
	}
}

func newArtifactDownloadCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var dest string
	cmd := &cobra.Command{
		Use:   "download <run-id> <name>",
		Short: "Download and extract a run artifact",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			url := cfg.Server + "/api/v1/runs/" + args[0] + "/artifacts/" + args[1]
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %s (%d)", string(b), resp.StatusCode)
			}
			if dest == "" {
				dest = "."
			}
			if err := artifact.ExtractTarZstd(resp.Body, dest); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "extracted %s of run %s to %s\n", args[1], args[0], dest)
			return nil
		},
	}
	cmd.Flags().StringVar(&dest, "dest", ".", "destination directory")
	return cmd
}
```

- [ ] **Step 4: Register in root**

In `internal/cli/root.go`, after the other `root.AddCommand(...)` lines (e.g. after `newRejectCmd`), add:

```go
	root.AddCommand(newArtifactCmd(resolve))
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestArtifact`
Expected: PASS (both). Then `go build ./...` — clean.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/artifact.go internal/cli/artifact_test.go internal/cli/root.go
git commit -m "feat(cli): add 'artifact list' and 'artifact download' commands"
```

---

### Task 4: End-to-end round-trip test

Verify the full wire round-trip (upload → list → download → extract) over HTTP against the real controller router with a local object store, no DB.

**Files:**
- Create: `test/e2e/artifact_roundtrip_test.go`

**Interfaces:**
- Consumes: the controller `Server` constructor + object-store wiring (reuse whatever `test/e2e` or `internal/controller` tests already use to build a `Server` with a `LocalObjectStore`), `api.ArtifactInfo`, `artifact.ExtractTarZstd`, the agent's `Client` OR direct `http` calls with the agent token.

- [ ] **Step 1: Inspect existing e2e harness**

Look at existing files under `test/e2e/` (and how `internal/controller` tests build a `Server`) to find the canonical way to construct a controller with a working `LocalObjectStore` and a known agent token, and to obtain an `httptest.Server` (or equivalent base URL). Reuse it — do NOT invent a new bootstrap if one exists.

- [ ] **Step 2: Write the test**

Create `test/e2e/artifact_roundtrip_test.go`. Adapt the `Server`/objStore construction to the harness found in Step 1; the shape is:

```go
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	// + controller + objectstore + agent client imports per the harness
)

func TestArtifactRoundTrip(t *testing.T) {
	baseURL, agentToken := startArtifactController(t) // built from the harness in Step 1

	// 1. Upload a tar+zstd artifact via the agent PUT path.
	payload := makeArtifactTarZstd(t, "out.txt", "round-trip-ok") // local tar+zstd helper
	putReq, _ := http.NewRequest(http.MethodPut, baseURL+"/api/v1/runs/r1/artifacts/build", bytes.NewReader(payload))
	putReq.Header.Set("Authorization", "Bearer "+agentToken)
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil || putResp.StatusCode != http.StatusNoContent {
		t.Fatalf("upload: %v code=%d", err, code(putResp))
	}

	// 2. List and assert the name appears.
	listReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/runs/r1/artifacts", nil)
	listReq.Header.Set("Authorization", "Bearer "+agentToken)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil || listResp.StatusCode != http.StatusOK {
		t.Fatalf("list: %v code=%d", err, code(listResp))
	}
	var list []api.ArtifactInfo
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	if len(list) != 1 || list[0].Name != "build" {
		t.Fatalf("list = %v", list)
	}

	// 3. Download and extract; assert content round-trips.
	getReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/runs/r1/artifacts/build", nil)
	getReq.Header.Set("Authorization", "Bearer "+agentToken)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("download: %v code=%d", err, code(getResp))
	}
	dest := t.TempDir()
	if err := artifact.ExtractTarZstd(getResp.Body, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "out.txt"))
	if err != nil || string(got) != "round-trip-ok" {
		t.Fatalf("out.txt = %q, %v", got, err)
	}
}
```

Provide the local helpers in the same file: `makeArtifactTarZstd(t, name, content) []byte` (same construction as the Task 1 test helper, using `archive/tar` + `klauspost/compress/zstd`), `code(resp)` (returns `resp.StatusCode` or `0` when nil), and `startArtifactController(t)` (builds the `Server` + `LocalObjectStore` + agent token per Step 1, returns `httptest.Server` base URL via `t.Cleanup(srv.Close)` and the token). If the `test/e2e` package has no such bootstrap and building one requires a DB, fall back to constructing the controller `Server` directly with `s.objStore = objectstore.NewLocalObjectStore(t.TempDir())` (or the real constructor name) and `s.cfg.AgentToken = "e2e-token"`, register routes, and wrap in `httptest.NewServer(s.r)` — DB stays nil (only object-store + agent-token paths are exercised).

- [ ] **Step 3: Run the test**

Run: `go test ./test/e2e/ -run TestArtifactRoundTrip -v`
Expected: PASS.

- [ ] **Step 4: Full build + test**

Run: `go build ./... && go test ./...`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/artifact_roundtrip_test.go
git commit -m "test(e2e): artifact upload/list/download/extract round-trip"
```

---

### Task 5: Document the human artifact API + CLI

**Files:**
- Modify: `docs/jobs.md` (Artifacts section)

- [ ] **Step 1: Update the docs**

In `docs/jobs.md`, in the Artifacts section, document the human-facing surface:
- `GET /api/v1/runs/{runID}/artifacts` — list artifact names (JSON `[{"name":"..."}]`), accepts an agent token or a human identity (PAT/OIDC/session).
- `GET /api/v1/runs/{runID}/artifacts/{name}` — download the tar+zstd stream (same combined auth).
- `PUT` remains agent-only.
- CLI: `unified-cd artifact list <run-id>` and `unified-cd artifact download <run-id> <name> [--dest .]`.

Match the surrounding doc style. Add a short example of each CLI command.

- [ ] **Step 2: Commit**

```bash
git add docs/jobs.md
git commit -m "docs: document human-facing artifact list/download API and CLI"
```

---

## Self-Review

**Spec coverage:** B (combined-auth download + list handler + `ArtifactInfo`) → Task 2; C (CLI + shared extractor) → Tasks 1 & 3; E (e2e round-trip) → Task 4; docs → Task 5. All spec sections covered.

**Placeholder scan:** No TBD/TODO; every code step contains full code. The two test-harness steps (Task 2 Step 1, Task 4 Steps 1-2) intentionally instruct the implementer to reuse the existing package harness and give an explicit fallback (LocalObjectStore + nil DB) — this is a discovery instruction with a concrete default, not a placeholder.

**Type consistency:** `api.ArtifactInfo{Name string}` defined in Task 2, consumed identically in Tasks 3 & 4. `artifact.ExtractTarZstd(io.Reader, string) error` defined in Task 1, consumed in Tasks 3 & 4 and by the agent client. `AgentOrServerAuth(string, store.Store, *Server)` defined and used in Task 2. Route paths (`/api/v1/runs/{runID}/artifacts` and `/{name}`) consistent across tasks.
