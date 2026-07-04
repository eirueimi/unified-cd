# unified-cd 権限管理（RBAC + SSO）設計書

- 日付: 2026-07-04
- 対象: `unified-cd`（Go CI/CDツール）に認可(RBAC)を追加し、SSO(OIDC)を人間の主たる認証経路にする
- 想定規模: 小規模チーム（少数の信頼ユーザー、粗粒度ロール）

---

## 1. 目的とゴール

現状 unified-cd は**認証(AuthN)はあるが認可(AuthZ)が無い**。認証さえ通れば全ユーザーが全API（job/secret/run/token/agent 等）にフルアクセスできる。本設計は、

- **3ロールの粗粒度RBAC**（admin / developer / viewer）を導入し、リソース群×動詞で操作を制限する。
- ロールを **OIDCのグループ/ロールクレーム**（IdP由来）または **個人(email/sub)**、および **PAT単位**に紐づける。
- **SSO(OIDC)を人間の主たる認証経路**とし、**Dexを既定の単一ブローカ**として複数IdPを吸収できるようにする。ただしコントローラは OIDC 未設定でも起動可能（break-glass admin のみ）。
- unified-cd を **プロバイダ非依存の汎用OIDC RP** のまま維持する（特定IdPやDexにコード依存しない）。

### 成功基準

- 各principal（OIDCユーザー / PAT / セッション）に role が解決され、権限外の操作が 403 で拒否される。
- viewer は参照のみ、developer は job/run 系の変更まで、admin は secret・認証情報・トークン管理まで、が強制される。
- OIDCの `rolesClaim`（`groups` / `roles` / 名前空間付き等）を設定で切替え、Dex(各connector)・Entra・GitLab・Auth0 のいずれでも**コード変更なし**でロールを解決できる。
- 静的 `UNIFIED_TOKEN`(PAT) が常に admin として機能し、IdP障害時の break-glass になる。
- 既存の単一トークン運用が移行後も壊れない（既存PATは admin 付与）。

---

## 2. 非ゴール（スコープ外）

- **マルチテナンシー / 名前空間 / リソース所有者**（team/project/namespace 単位の分離）。全リソースは引き続きグローバル。
- **Casbin風の細粒度ポリシー**（resource×action×object のグロブ、per-job/per-app scoping）。将来 `roleMap` を `p,` ポリシーに拡張する余地は残すが本設計では扱わない。
- **外部認証プロキシ**（oauth2-proxy 等）への authN 委譲。unified-cd の既存OIDC RPを維持する。
- **agentトークンのロール化**。エージェント認証(`BearerAuth`)は現状のまま。
- **秘密値のAPI公開**。secretは今も名前のみ返す（値は返さない）。本設計もそれを変えない。

---

## 3. ロールと権限マトリクス

3ロールは厳密に階層的（admin ⊇ developer ⊇ viewer）。実装は「ロールランク（viewer=1, developer=2, admin=3）＋ルートごとの最小ロール要求」で足りる。

| リソース / 操作 | viewer | developer | admin |
|---|:--:|:--:|:--:|
| Jobs 参照（list/get/yaml） | ✅ | ✅ | ✅ |
| Jobs 作成/更新/削除（apply/delete） | ❌ | ✅ | ✅ |
| Runs 参照（list/get/logs/events/SSE） | ✅ | ✅ | ✅ |
| Runs 実行（trigger） | ❌ | ✅ | ✅ |
| Runs キャンセル | ❌ | ✅ | ✅ |
| Approvals 承認/却下 | ❌ | ✅ | ✅ |
| Schedules 参照 | ✅ | ✅ | ✅ |
| Schedules 作成/削除 | ❌ | ✅ | ✅ |
| Secrets 名前一覧 | ❌ | ✅ | ✅ |
| Secrets 設定/削除 | ❌ | ❌ | ✅ |
| AppSources 参照 | ✅ | ✅ | ✅ |
| AppSources 作成/削除 | ❌ | ❌ | ✅ |
| WebhookReceivers 参照 | ✅ | ✅ | ✅ |
| WebhookReceivers 作成/削除 | ❌ | ❌ | ✅ |
| GitCredentials 参照/作成/削除 | ❌ | ❌ | ✅ |
| PAT 発行（自ロール以下） | ❌ | ✅ | ✅ |
| PAT 一覧/失効（全ユーザー分） | ❌ | ❌ | ✅ |
| PAT 一覧/失効（自分が発行した分） | ❌ | ✅ | ✅ |
| Agents 情報 GET | ✅ | ✅ | ✅ |
| Agents 登録/heartbeat/claim/報告 | 静的agentトークン（人間ロール対象外） | | |

**確定した判断（曖昧セル）:**
- developer は **job の削除可**（小チームがパイプラインを所有）。
- developer は **approve/reject 可**（maintainerロールを設けないため）。
- developer は **secret の名前一覧のみ可**（job作成に必要）。値の設定/削除は admin。viewer は secret 不可。
- AppSource / WebhookReceiver / GitCredential の**変更は admin 限定**（認証情報や外部トリガを束ねる機微なため）。参照は viewer 以上（GitCredential は参照も admin）。
- developer は **自ロール以下の PAT を発行**でき、自分が発行した PAT を管理可。全ユーザー分の PAT 管理は admin。

---

## 4. アイデンティティとロール解決

### 4.1 Principal 拡張

`internal/controller/auth.go` の `Principal` に `Role` を追加する。

```go
type Principal struct {
    Name string // PAT名 or OIDC email(fallback sub)
    Kind string // "pat" | "oidc" | "session"
    Role string // "admin" | "developer" | "viewer"
}
```

### 4.2 ロールの決定元

| principal種別 | ロールの決め方 |
|---|---|
| OIDC id_token / session | `rolesClaim` のクレーム値を `roleMap` で解決。個人上書きは `userMap`（email/sub）。無ければ `defaultRole`。 |
| PAT | `pats.role` 列（新設）。発行時に発行者ロール以下で設定。 |
| bootstrap `UNIFIED_TOKEN` PAT | 常に **admin**（break-glass）。 |
| agentトークン(`BearerAuth`) | 対象外（人間ロールではない）。変更なし。 |

### 4.3 解決順（優先度・高→低）

1. **break-glass**: 静的 `UNIFIED_TOKEN` にマッチ → `admin`。
2. **userMap**: id_token の email（無ければ sub）が `userMap` に一致 → そのロール（個別の昇格/降格の上書き）。
3. **roleMap**: `rolesClaim` の値（配列可）が `roleMap` に一致 → 一致した中で**最も強いロール**（admin>developer>viewer）。
4. **defaultRole**: 上記いずれも無し → `defaultRole`。`deny`（または空）ならログイン拒否（403）。
5. PAT は 2〜4 を経ず、`pats.role` を直接使用。

### 4.4 OIDCスコープ／クレーム取り込み（実装項目）

- 認可リクエストに、`rolesClaim` に必要なスコープを含める（例 `groups` 用に `scope=... groups`）。現状 `openid/profile/email` のみの可能性が高いので拡張する。Entra の App Roles(`roles`) は追加スコープ不要。
- id_token 検証時（`verifyOIDCBearer` / callback）に、既存の `sub`/`email` に加えて **`rolesClaim` で指定されたクレームを抽出**する。クレーム値は string でも []string でも受ける。
- session に role を保持する（`sessions.role` 列）か、リフレッシュ毎に再解決する。本設計は **リフレッシュ時に再解決して session に保存**（グループ変更がリフレッシュで反映される。§10参照）。

---

## 5. 認可の実施（enforcement）

- ロールは階層的なので、**ルートごとに最小ロールを要求するミドルウェア** `requireMinRole(role)` を用意する（`Principal.Role` のランクを比較し、不足なら 403）。
- `internal/controller/server.go` のルート定義で、`ServerAuth` の内側に適用する:
  - **参照系**（GET list/get/logs/events/yaml, SSE）→ `requireMinRole("viewer")`
  - **変更系**（jobs/runs/schedules の POST/DELETE、runs cancel、approvals）→ `requireMinRole("developer")`
  - **admin系**（secrets set/delete、appsources/webhooks/gitcredentials の変更、全PAT管理）→ `requireMinRole("admin")`
- GET と POST/DELETE が同一パスに同居する場合は、chi のメソッド別ルーティング（`r.Get` / `r.Post`）でそれぞれに別の最小ロールを付ける。
- secret 名前一覧（developer可）と secret 設定/削除（admin）はメソッド/エンドポイントで分離する。
- **例外（参照でも admin）**: `GitCredentials` は参照(GET)も含め全メソッド `requireMinRole("admin")`（認証情報そのもののため、上の「参照系→viewer」一般則の例外）。`secrets` の GET は「名前一覧」なので developer。
- webhook ingress(`/webhook/{name}`)・health・OIDCフロー・`/ui/*` は従来どおり認可対象外（webhook は署名/トークン検証済み）。
- artifact ダウンロード(`AgentOrServerAuth`)は agent か viewer 以上に許可。

---

## 6. 設定スキーマ

`internal/config/controller.go` の `ControllerOIDCConfig` にロール解決フィールドを追加。YAML(`oidc:`)と `UNIFIED_OIDC_*` 環境変数の両対応（既存の解決優先順位を踏襲）。

```yaml
oidc:
  issuer:        https://...            # 既存
  clientId:      unified-cd             # 既存
  clientSecret:  ...                    # 既存
  deviceClientId: unified-cd-cli        # 既存

  # --- 追加: ロール解決 ---
  rolesClaim:    groups                 # ロール値を読むクレーム名（既定 "groups"）
  roleMap:                              # クレーム値 → ロール（グループ/ロール）
    unified-admins:  admin
    unified-devs:    developer
    unified-viewers: viewer
  userMap:                             # 個人(email or sub) → ロール（groups非対応IdP・個別上書き用）
    alice@example.com:      admin
    breakglass@example.com: admin
  defaultRole:   viewer                 # 未一致時のロール。"deny"/"" ならログイン拒否
```

- 単一値の環境変数化が難しい map は **設定ファイル側**を推奨。スカラー（`rolesClaim`/`defaultRole`）は `UNIFIED_OIDC_ROLES_CLAIM` / `UNIFIED_OIDC_DEFAULT_ROLE` でも設定可。
- `roleMap`/`userMap` は unified-cd 側に置く（Argo CD が `argocd-rbac-cm` に置くのと同じ思想。Dexはgroupsを流すだけ）。

---

## 7. SSOトポロジと各IdP統合

### 7.1 アーキテクチャ原則

- **unified-cd = 汎用OIDC RP（変更しない）。** issuer/clientID/JWKS/audience のみを扱い、プロバイダ固有処理を持たない。
- **issuer は常に1つ。** 複数IdPを統合したい場合は、その1つを **Dex（単一ブローカ）** にし、Dexに connector を複数足す。Dexが全ソースを1つの `groups` クレームに正規化する。→ unified-cd のコードをDexに結合させず、複数ソースの複雑さだけDexへ押し込める。
- **group→role マッピングは unified-cd 側（`roleMap`）。** Dex/上流はgroupsを流すだけ。

### 7.2 「groups」はOIDC標準ではない（重要）

`groups`/`roles` はOIDCコア仕様の標準クレームではなく、**IdP独自の拡張**。したがって「RFC準拠なら自動でgroupsが出る」ことはない。汎用OIDCで読めるのは「**IdPがクレームとして吐くよう設定した場合のみ**」。`rolesClaim` はその差異を吸収する。

### 7.3 IdPごとの「groupsを作る主体」と設定

| 構成 | groupsを作る主体 | rolesClaim | 左辺(roleMap)の値 | connector |
|---|---|---|---|---|
| **local Dex 静的** | （groups不可） | `email` | ―（`userMap`でemail→role） | 不要（staticPasswords） |
| **Dex→GitHub** | **Dexのgithub connector**（GitHub APIでorg/team取得し `org:team` を合成） | `groups` | `my-org:team-slug` | **`github` 専用が必須**（GitHubは非OIDC） |
| **Dex→GitLab** | 上流GitLab（OIDC）or Dexのgitlab connector | `groups` | group full-path | 汎用 `oidc` or `gitlab` |
| **Entra ID 直/Dex経由** | **Entra自身**（App Roles or groups claim） | `roles`（推奨）/ `groups` | App Role値(`admin`) / グループGUID | 直結 or 汎用 `oidc` |
| **Auth0 直/Dex経由** | Auth0（Post-Login Actionで名前空間付きクレーム） | `https://.../roles` | Auth0 Role名 | 直結 or 汎用 `oidc` |

**要点:**
- **GitHubだけは専用connector必須**（非OIDCで、team情報はREST API専用。Dexの `github` connectorが設定だけで使える既製品。コード実装は不要）。
- **Entra/GitLab/Auth0 等はIdP自身がクレームを吐く** → 汎用 `oidc` connector か直結で済み、専用connectorは不要。ただし各IdP側で「そのクレームを発行する設定」は必要。

### 7.4 ランタイム挙動（groupsはログイン時スナップショット）

- groups はリクエスト毎ではなく **ログイン時に、ユーザーが通った1つのconnector**が取得し、**id_tokenに焼き込まれる**。unified-cd は以後トークンから読むだけ（Dexへ毎回問い合わせない。unified-cdがDexを叩くのはJWKS取得のみ・キャッシュ）。
- 上流のグループ変更は **再ログイン/トークンリフレッシュ時**に反映（Argo CDと同挙動）。

### 7.5 Entra ID 具体手順（付録的メモ）

- **方式A（groups claim）**: Security グループ作成 → App registration → Token configuration → Add groups claim（既定はグループGUID）→ overage回避に「Groups assigned to the application」を利用 → GUIDを `roleMap` に記載（`rolesClaim: groups`）。⚠️ 約200グループ超で overage（トークンにgroups省略・Graph参照が必要）。
- **方式B（App Roles・推奨）**: App registration → App roles で `admin`/`developer`/`viewer`(Value) を定義 → Enterprise app の Users and groups で割当 → `roles` クレーム発行 → `rolesClaim: roles`、`roleMap: {admin: admin,...}`。GUID・overageなし。⚠️ **グループをApp Roleに割当てるにはEntra ID P1/P2**（個人割当は無償可）。

### 7.6 既定トポロジ（ドキュメントの推奨パス）

- **quickstart**: local Dex 静的ユーザー（外部依存ゼロ、`email`＋`userMap`）。
- **推奨本番**: Dex→GitLab connector（`groups`、CLI device flow維持、identityをGitLabに一元化）。
- **代替（文書化）**: Dex→GitHub connector / Entra(App Roles) / Auth0 / GitLab直結。

---

## 8. データモデル変更

- `pats` テーブルに `role text NOT NULL DEFAULT 'admin'` 列を追加（マイグレーション）。既定 `admin` は**後方互換のため**（§9）。
- `sessions` テーブルに `role text` 列を追加（ログイン/リフレッシュ時に解決値を保存）。
- 新規マイグレーション `NNN_add_role.up.sql` / `.down.sql`。
- API型: PAT作成リクエストに任意 `role`（省略時は発行者ロール、または発行者ロールを上限にクランプ）。

---

## 9. 後方互換と移行

- **既存PATは admin 付与**（マイグレーションの列デフォルト `admin`）。現行の「単一トークンで全アクセス」運用が壊れない。移行後、admin が **必要最小ロールのPATを再発行**することを推奨として文書化。
- **OIDC未設定でも起動可能**（現状維持）。その場合は静的 `UNIFIED_TOKEN`(admin) のみ。SSO必須化は「boot拒否」ではなく**運用ポリシー＋認可の常時ON**として実現。
- OIDC設定済みで `defaultRole: deny` の場合、`roleMap`/`userMap` 未整備だと既存OIDCユーザーがログインできなくなる → 移行時は一時的に `defaultRole: viewer` から始め、マッピング整備後に `deny` へ引き締める手順を文書化。

---

## 10. セキュリティ考慮

- **break-glass**: `UNIFIED_TOKEN`(admin) は緊急専用。日常使用禁止・厳重保管・定期ローテーションを文書化。
- **定数時間比較**: agentトークン/webhookトークンは既存どおり `hmac.Equal`/`ConstantTimeCompare`。
- **PATロール昇格防止**: PAT発行時、要求 role を発行者ロール以下にクランプ（developer が admin PAT を作れない）。
- **グループ変更の反映遅延**: 再ログイン/リフレッシュまで旧ロールが有効（§7.4）。降格を即時反映したい場合は session 失効(admin操作)を用意する（任意拡張）。
- **Entra overage**: groups claim 方式では大規模グループ所属で groups が省略され得る。App Roles 推奨、または「アプリ割当グループのみ」で回避。
- **`if:` 等の fail-open は無関係**（本設計は認可ミドルウェアで明示的にdeny）。

---

## 11. テスト方針

- **ロール解決の単体テスト**: rolesClaim(string/array)、roleMap一致（複数一致で最強ロール）、userMap上書き、defaultRole、deny。
- **PAT**: role列の読み出し、発行時クランプ、bootstrap=admin。
- **認可ミドルウェア**: viewerがPOST/DELETEで403、developerがsecrets set/deleteで403、adminは許可、viewerがtriggerで403、developerがapprove可、等の表駆動テスト。
- **後方互換**: 既存PAT（role未指定→admin）が全操作可能。
- **回帰**: 既存の認証（PAT/OIDC/session/agent）テストが緑。
- （E2E的手動）Dex静的でviewer/developer/adminを演じ分け、UI/CLIで拒否/許可を確認。

---

## 12. 設計判断の記録

| 判断 | 選択 | 理由 |
|---|---|---|
| 粒度 | 3ロール階層（admin/developer/viewer） | 小規模チームに過不足なし。Casbin細粒度は非ゴール（将来拡張路は残す） |
| ロール源 | OIDC `rolesClaim`+`roleMap`／個人`userMap`／PAT`role` | IdP非依存。Argo CD型（RP側にマッピング） |
| SSO必須 | 運用ポリシー＋認可常時ON（boot拒否にしない） | 静的break-glassを残しつつ人間はSSO。IdP障害時の締め出し回避 |
| ブローカ | Dexを既定の単一ブローカ、コードは汎用OIDC RPのまま | 複数ソースの複雑さをDexへ。直結IdPの選択肢も維持 |
| GitHub | 使う場合のみ Dex `github` connector | 非OIDCゆえ専用connector必須。他IdPは汎用で可 |
| Entra | App Roles(`roles`)推奨 | GUID/overage回避、`roleMap`左辺が綺麗 |
| 移行 | 既存PAT=admin | 現行運用を壊さない |
| enforcement | ロールランク＋ルート最小ロール | ロールが階層的なので最小実装で正しい |
