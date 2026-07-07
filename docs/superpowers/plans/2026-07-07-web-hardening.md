# Web Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** セッション Cookie の Secure 化(オプトアウト付き)、Origin 検証ミドルウェア(CSRF 多層防御)、セキュリティヘッダ 3 点を controller に追加する。

**Architecture:** 新規ファイル `internal/controller/hardening.go` に 2 つのミドルウェア(セキュリティヘッダ / Origin 検証)を置き、`routes()` で全体に適用。Cookie は `sessionCookie` ヘルパーに集約して `Secure: !cfg.InsecureCookies` を一元化し、設定は既存の YAML → env → flag の3層(`internal/config/controller.go` + `cmd/controller/main.go`)で配線する。

**Tech Stack:** Go 標準ライブラリ + chi(既存)。テストは testify/assert + httptest(PG 不要のユニット。ただし controller パッケージの既存スイートは PG を使うため、ゲート実行時は Docker 起動が必要)。

## Global Constraints

- ヘッダは正確に: `X-Frame-Options: DENY` / `X-Content-Type-Options: nosniff` / `Referrer-Policy: same-origin`。
- Origin 検証: unsafe メソッド = POST/PUT/PATCH/DELETE のみ対象(GET/HEAD/OPTIONS は素通り)。`Origin` → 無ければ `Referer` → 両方無ければ許可。host(port 含む)比較のみで scheme は比較しない。不一致は `403` + 本文 `cross-origin request rejected`。`Origin: null` は 403。許可 host = `r.Host` + `OIDCConfig.ExternalURL` の host(設定時)。
- 適用位置: securityHeaders / originCheck とも `s.r.Use`(ルータ全体)。
- Cookie: `Secure: !s.cfg.InsecureCookies`。`InsecureCookies` のデフォルトは false(= Secure 付与)。既存属性(HttpOnly / SameSite=Lax / Path=/)は不変。ログイン・ログアウト両方の SetCookie が対象。
- 設定名: YAML `insecureCookies` / env `UNIFIED_INSECURE_COOKIES` / flag `--insecure-cookies`。
- テストゲート: `go build ./... && go test -count=1 ./internal/controller/`(Docker 稼働が前提 — 既存 PG テストが同居)。

---

### Task 1: ミドルウェア 2 点(セキュリティヘッダ + Origin 検証)

**Files:**
- Create: `internal/controller/hardening.go`
- Create: `internal/controller/hardening_test.go`
- Modify: `internal/controller/server.go`(`routes()` 冒頭の `s.r.Use` 群、204-207 行付近)

**Interfaces:**
- Consumes: `Server.cfg` / `Server.oidcCfg`(既存フィールド)。
- Produces: `func securityHeadersMiddleware(next http.Handler) http.Handler`、`func (s *Server) originCheckMiddleware(next http.Handler) http.Handler`(後続タスク依存なし)。

- [ ] **Step 1: 失敗するテストを書く** — `internal/controller/hardening_test.go` を新規作成:

```go
package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func hardeningOKHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestSecurityHeadersMiddleware_SetsAllThree(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://ucd.local/api/v1/jobs", nil)
	securityHeadersMiddleware(hardeningOKHandler()).ServeHTTP(rec, req)
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "same-origin", rec.Header().Get("Referrer-Policy"))
}

func originCheckStatus(t *testing.T, s *Server, method, origin, referer string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "http://ucd.local/api/v1/runs", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	s.originCheckMiddleware(hardeningOKHandler()).ServeHTTP(rec, req)
	return rec.Code
}

func TestOriginCheck_SameHostOriginPasses(t *testing.T) {
	assert.Equal(t, http.StatusOK, originCheckStatus(t, &Server{}, http.MethodPost, "http://ucd.local", ""))
}

func TestOriginCheck_SchemeIsNotCompared(t *testing.T) {
	// TLS-terminating proxies make the scheme unreliable; host match is enough.
	assert.Equal(t, http.StatusOK, originCheckStatus(t, &Server{}, http.MethodPost, "https://ucd.local", ""))
}

func TestOriginCheck_CrossOriginRejected(t *testing.T) {
	assert.Equal(t, http.StatusForbidden, originCheckStatus(t, &Server{}, http.MethodPost, "https://evil.example", ""))
}

func TestOriginCheck_PortMismatchRejected(t *testing.T) {
	assert.Equal(t, http.StatusForbidden, originCheckStatus(t, &Server{}, http.MethodPost, "http://ucd.local:8080", ""))
}

func TestOriginCheck_NullOriginRejected(t *testing.T) {
	assert.Equal(t, http.StatusForbidden, originCheckStatus(t, &Server{}, http.MethodPost, "null", ""))
}

func TestOriginCheck_NoHeadersPass(t *testing.T) {
	// CLI / agents / webhooks send neither Origin nor Referer.
	assert.Equal(t, http.StatusOK, originCheckStatus(t, &Server{}, http.MethodPost, "", ""))
}

func TestOriginCheck_RefererFallbackMatchPasses(t *testing.T) {
	assert.Equal(t, http.StatusOK, originCheckStatus(t, &Server{}, http.MethodPost, "", "http://ucd.local/ui/"))
}

func TestOriginCheck_RefererFallbackMismatchRejected(t *testing.T) {
	assert.Equal(t, http.StatusForbidden, originCheckStatus(t, &Server{}, http.MethodPost, "", "https://evil.example/page"))
}

func TestOriginCheck_GetPassesRegardless(t *testing.T) {
	assert.Equal(t, http.StatusOK, originCheckStatus(t, &Server{}, http.MethodGet, "https://evil.example", ""))
}

func TestOriginCheck_ExternalURLHostAllowed(t *testing.T) {
	s := &Server{oidcCfg: &OIDCConfig{ExternalURL: "https://ci.example.com"}}
	assert.Equal(t, http.StatusOK, originCheckStatus(t, s, http.MethodPost, "https://ci.example.com", ""))
}
```

- [ ] **Step 2: RED 確認**

Run: `go test -count=1 ./internal/controller/ -run "TestSecurityHeaders|TestOriginCheck" -v`
Expected: コンパイルエラー(`undefined: securityHeadersMiddleware` / `originCheckMiddleware`)。

- [ ] **Step 3: 実装** — `internal/controller/hardening.go` を新規作成:

```go
package controller

import (
	"net/http"
	"net/url"
	"strings"
)

// securityHeadersMiddleware adds defense-in-depth headers to every response.
// CSP and HSTS are deliberately absent: CSP needs a pass over the Vite dev
// setup (HMR, inline styles) first, and HSTS belongs to the TLS-terminating
// proxy.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// originCheckMiddleware rejects cross-origin state-changing requests — CSRF
// defense-in-depth on top of the session cookie's SameSite=Lax. Browsers
// always attach an Origin header to non-GET requests, so a request carrying
// neither Origin nor Referer is a non-browser client (CLI, agent, webhook)
// and passes through. The scheme is deliberately not compared: a
// TLS-terminating proxy makes it unreliable; the host (including port) is.
func (s *Server) originCheckMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		ref := r.Header.Get("Origin")
		if ref == "" {
			ref = r.Header.Get("Referer")
		}
		if ref == "" {
			next.ServeHTTP(w, r)
			return
		}
		if u, err := url.Parse(ref); err == nil && u.Host != "" && s.allowedBrowserHost(r, u.Host) {
			next.ServeHTTP(w, r)
			return
		}
		// Covers mismatches and non-URL values like "null" (sandboxed iframes).
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
	})
}

// allowedBrowserHost reports whether host (host[:port]) matches the request's
// own Host or the configured OIDC ExternalURL's host.
func (s *Server) allowedBrowserHost(r *http.Request, host string) bool {
	if strings.EqualFold(host, r.Host) {
		return true
	}
	if s.oidcCfg != nil && s.oidcCfg.ExternalURL != "" {
		if u, err := url.Parse(s.oidcCfg.ExternalURL); err == nil && u.Host != "" && strings.EqualFold(host, u.Host) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: GREEN 確認**

Run: `go test -count=1 ./internal/controller/ -run "TestSecurityHeaders|TestOriginCheck" -v`
Expected: 12/12 PASS。

- [ ] **Step 5: 配線** — `internal/controller/server.go` の `routes()` 冒頭(現在 `s.r.Use(middleware.Recoverer)` 〜 `s.r.Use(s.metricsMiddleware)` の並び)に追記:

```go
	s.r.Use(middleware.Recoverer)
	s.r.Use(middleware.RealIP)
	s.r.Use(accessLogMiddleware)
	s.r.Use(s.metricsMiddleware)
	s.r.Use(securityHeadersMiddleware)
	// Router-wide (not just /api/v1): the auth POST routes (e.g.
	// /api/v1/auth/logout) are registered directly on s.r outside the
	// /api/v1 group, and non-browser clients pass through anyway.
	s.r.Use(s.originCheckMiddleware)
```

- [ ] **Step 6: フルゲート**

Run: `go build ./... && go test -count=1 ./internal/controller/`
Expected: build クリーン、`ok`(既存の PG テスト含む — Docker 稼働が前提)。

- [ ] **Step 7: Commit**

```bash
git add internal/controller/hardening.go internal/controller/hardening_test.go internal/controller/server.go
git commit -m "feat(controller): security headers and cross-origin request rejection"
```

---

### Task 2: セッション Cookie の Secure 化 + InsecureCookies 設定

**Files:**
- Modify: `internal/controller/auth_oidc.go`(SetCookie 2箇所をヘルパーへ集約: handleOIDCCallback 内 248 行付近と handleLogout 内 267 行付近)
- Modify: `internal/controller/server.go`(`Config` 構造体、23-36 行付近)
- Modify: `internal/config/controller.go`(YAML フィールド + env + マージ)
- Modify: `cmd/controller/main.go`(flag + `controller.Config` リテラル、90 / 197 行付近)
- Test: `internal/controller/hardening_test.go`(Task 1 のファイルに追記)

**Interfaces:**
- Consumes: なし(Task 1 とはファイル共有のみ)。
- Produces: `func (s *Server) sessionCookie(value string, expires time.Time, maxAge int) *http.Cookie`、`Config.InsecureCookies bool`、`config.ControllerConfig.InsecureCookies bool`。

- [ ] **Step 1: 失敗するテストを書く** — `internal/controller/hardening_test.go` の末尾に追記:

```go
func TestSessionCookie_SecureByDefault(t *testing.T) {
	s := &Server{cfg: Config{}}
	c := s.sessionCookie("tok", time.Now().Add(time.Hour), 0)
	assert.True(t, c.Secure)
	assert.True(t, c.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
	assert.Equal(t, "/", c.Path)
	assert.Equal(t, "ucd_session", c.Name)
	assert.Equal(t, "tok", c.Value)
}

func TestSessionCookie_InsecureCookiesOptOut(t *testing.T) {
	s := &Server{cfg: Config{InsecureCookies: true}}
	assert.False(t, s.sessionCookie("tok", time.Now().Add(time.Hour), 0).Secure)
}

func TestSessionCookie_LogoutDeletionShape(t *testing.T) {
	s := &Server{cfg: Config{}}
	c := s.sessionCookie("", time.Time{}, -1)
	assert.Equal(t, "", c.Value)
	assert.Equal(t, -1, c.MaxAge)
	assert.True(t, c.Secure)
}
```

(`time` のインポートをテストファイルに追加。)

- [ ] **Step 2: RED 確認**

Run: `go test -count=1 ./internal/controller/ -run TestSessionCookie -v`
Expected: コンパイルエラー(`s.sessionCookie undefined` / `Config` に `InsecureCookies` が無い)。

- [ ] **Step 3: 実装**

`internal/controller/server.go` の `Config` に追加(`StderrPlain` の下):

```go
	// InsecureCookies disables the Secure attribute on session cookies.
	// Default (false) sets Secure — Chrome/Firefox treat http://localhost as
	// trustworthy so local dev keeps working; opt out only for plain-HTTP
	// deployments (LAN access, Safari-based local dev).
	InsecureCookies bool
```

`internal/controller/auth_oidc.go` にヘルパーを追加し、2箇所を置き換え:

```go
// sessionCookie builds the ucd_session cookie with the shared attributes.
// maxAge < 0 deletes the cookie (logout); maxAge == 0 omits Max-Age (login,
// which uses Expires instead).
func (s *Server) sessionCookie(value string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   !s.cfg.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}
```

handleOIDCCallback の SetCookie を:

```go
	http.SetCookie(w, s.sessionCookie(sessionToken, expiresAt, 0))
```

handleLogout の SetCookie を:

```go
	http.SetCookie(w, s.sessionCookie("", time.Time{}, -1))
```

`internal/config/controller.go`:
- 構造体(`StderrPlain` の下)に `InsecureCookies bool \`yaml:"insecureCookies"\`` を追加(コメント: `// InsecureCookies disables the Secure attribute on session cookies (env: UNIFIED_INSECURE_COOKIES).`)。
- env 組み立て(`StderrPlain: envBool(...)` の下)に `InsecureCookies: envBool("UNIFIED_INSECURE_COOKIES"),` を追加。
- ファイルマージ(`if file.StderrPlain { ... }` の下)に:

```go
	if file.InsecureCookies {
		eff.InsecureCookies = true
	}
```

`cmd/controller/main.go`:
- flag 定義(`stderrPlain` の下)に:

```go
	insecureCookies := flag.Bool("insecure-cookies", eff.InsecureCookies, "do not set the Secure attribute on session cookies (env: UNIFIED_INSECURE_COOKIES)")
```

- `controller.Config{...}` リテラル(197 行付近)に `InsecureCookies: *insecureCookies,` を追加。

- [ ] **Step 4: GREEN + 生 Cookie リテラルが残っていないことの確認**

Run: `go test -count=1 ./internal/controller/ -run TestSessionCookie -v`
Expected: 3/3 PASS。

Run: `grep -n "http.Cookie{" internal/controller/auth_oidc.go`
Expected: `sessionCookie` ヘルパー内の 1 箇所のみ。

- [ ] **Step 5: フルゲート**

Run: `go build ./... && go test -count=1 ./internal/controller/`
Expected: build クリーン、`ok`。

- [ ] **Step 6: Commit**

```bash
git add internal/controller/server.go internal/controller/auth_oidc.go internal/controller/hardening_test.go internal/config/controller.go cmd/controller/main.go
git commit -m "feat(controller): Secure session cookies by default with --insecure-cookies opt-out"
```

---

### Task 3: spec ステータス更新

**Files:**
- Modify: `docs/superpowers/specs/2026-07-07-web-hardening-design.md`(ステータス行のみ)

- [ ] **Step 1: ステータスを「実装済み」へ** — `- ステータス: 設計レビュー中` を `- ステータス: 実装済み` に変更。

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/specs/2026-07-07-web-hardening-design.md
git commit -m "docs(spec): web hardening implemented"
```
