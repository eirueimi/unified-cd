# Web ハードニング(Secure Cookie・Origin 検証・セキュリティヘッダ)設計

- 日付: 2026-07-07
- ステータス: 設計レビュー中

## 背景と動機

CSRF/セッション保護の監査で3つのギャップを確認した。

1. セッション Cookie(`ucd_session`)に `Secure` フラグが無く、平文 HTTP でも送信される(`internal/controller/auth_oidc.go` の SetCookie 2箇所)。
2. CSRF 防御が `SameSite=Lax` の単層。モダンブラウザでは十分だが、同一サイト扱いのサブドメインからの攻撃や SameSite 非対応クライアントに対する多層防御が無い。
3. セキュリティヘッダ(`X-Frame-Options` 等)が皆無で、クリックジャッキング等への defense-in-depth が無い。

前提知識: Chrome/Firefox/Edge は `http://localhost` を trustworthy origin として扱うため、`Secure` を付けても localhost HTTP での開発は影響を受けない(Safari は例外)。

## 変更内容

### 1. セッション Cookie の Secure 化 — `internal/controller/auth_oidc.go` + 設定

- ログイン時の SetCookie(`handleOIDCCallback`)とログアウト時の削除 SetCookie(`handleLogout`)の両方に `Secure: true` を付与する。
- オプトアウト: `controller.Config` に `InsecureCookies bool` を追加(デフォルト false = Secure 付与)。true のときのみ `Secure` を外す。用途は Safari での localhost 開発や LAN 内平文 HTTP 運用。
- 設定の配線は既存慣行に従う: YAML config フィールド + 環境変数 `UNIFIED_INSECURE_COOKIES` + フラグ `--insecure-cookies`(`cmd/controller/main.go` の eff → flag パターン)。
- `__Host-` プレフィックスは今回見送り(オプトアウト時に Cookie 名が変わる複雑さを避ける。将来検討として記録)。

### 2. Origin 検証ミドルウェア — `internal/controller/` 新規ファイル

- `/api/v1` 配下の unsafe メソッド(POST / PUT / PATCH / DELETE)に適用する CSRF 多層防御。
- 判定ロジック(順に評価):
  1. `Origin` ヘッダが存在する場合: その host(port 含む)を許可 host 集合と比較。不一致 → `403 Forbidden`(本文 `cross-origin request rejected`)。
  2. `Origin` が無く `Referer` が存在する場合: Referer の host で同じ判定。
  3. 両方無い場合: 許可(CLI・agent・curl は送らない。ブラウザは unsafe メソッドで必ず Origin を送るため、ブラウザ経由の攻撃はここに落ちない)。
- 許可 host 集合: リクエストの `r.Host` +(設定されていれば)`OIDCConfig.ExternalURL` の host。scheme は比較しない(TLS 終端プロキシで歪むため)。
- `Origin: null`(サンドボックス iframe 等)は不一致として 403。
- 適用位置: **ルータ全体**(`s.r.Use`)。auth 系 POST(`/api/v1/auth/logout` 等)が `/api/v1` グループ外に直接登録されているため、全体適用でカバーする。agent・CLI・webhook 類は Origin を送らないため素通り、`/dex` プロキシへのブラウザ POST は同一ホストなので通る。GET/HEAD/OPTIONS は対象外。

### 3. セキュリティヘッダ・ミドルウェア — `internal/controller/` 新規ファイル(2 と同居可)

全レスポンス(`s.r.Use`)に以下を付与:

- `X-Frame-Options: DENY`
- `X-Content-Type-Options: nosniff`
- `Referrer-Policy: same-origin`

## スコープ外(記録)

- CSP(Vite dev の HMR・インライン style との整合を精査してから別件で)
- HSTS(TLS 終端プロキシの責務)
- `__Host-` Cookie プレフィックス
- CSRF トークン(synchronizer/double-submit)— Lax + Origin 検証で十分と判断

## テスト

- Cookie(`internal/controller/`): ログイン callback 相当の SetCookie で `Secure` が付くこと、`InsecureCookies: true` で外れること、logout 側も同様。
- Origin ミドルウェア: (1) Origin host 一致 → pass、(2) Origin host 不一致 → 403、(3) Origin 無し・Referer 無し → pass、(4) Origin 無し・Referer 不一致 → 403、(5) GET は Origin 不一致でも pass、(6) `ExternalURL` の host に一致する Origin → pass、(7) `Origin: null` → 403。
- ヘッダ: 任意のエンドポイントのレスポンスに 3 ヘッダが付くこと。
