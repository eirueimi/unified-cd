# GitOps強化: AppSource管理リソースの書き込み保護 + `unified-cli export` 設計

- 日付: 2026-07-06
- ステータス: 設計承認済み

## 背景と動機

GitOps観点のレビューで挙がった2つのギャップを埋める。

1. **Gitを迂回できる**: AppSource が同期した Job/Schedule/WebhookReceiver/GitCredential/AppSource と、`unified-cli apply` / REST API で直接書き込んだリソースが同じテーブルに無差別に混在する。AppSource 管理下のリソースを手動で上書き・削除でき、Git と DB が黙って乖離する。
2. **DB の状態を Git に書き戻せない**: 手動 apply したリソースを Git 管理へ移行する手段（エクスポート）がなく、災害復旧も pg_dump 頼み。

本設計は (1) AppSource 管理下リソースへの直接書き込みを **409 Conflict で拒否**（デフォルト有効）、(2) 全リソースを AppSource がそのまま読めるディレクトリツリーとして書き出す **`unified-cli export`** の2機能を追加する。

## 機能1: AppSource 管理リソースの書き込み保護

### 方式（承認済みの決定）

- **書き込み拒否**方式（selfHeal ではない）。API レベルで 409 Conflict を返す。
- **デフォルト有効**。AppSource 単位で `syncPolicy.allowManualOverride: true` により緩和できる。
- 管理判定は `app_sources.managed_resources`（jsonb）を書き込み時に検索する（非正規化カラムやメモリキャッシュは採らない: 前者は二重管理、後者は HA でリーダー外レプリカが更新を受けられず誤判定するため）。

### store 層

新規メソッド（`store.Store` interface + Postgres 実装 + テスト用 fake）:

```go
// FindManagingAppSource returns the AppSource whose managed_resources contains
// {kind,name}, or nil when the resource is unmanaged.
FindManagingAppSource(ctx context.Context, kind, name string) (*AppSource, error)
```

Postgres 実装: `SELECT ... FROM app_sources WHERE managed_resources @> $1::jsonb LIMIT 1`（`$1` = `[{"kind":"Job","name":"team-a/build"}]`）。完全一致のみ（managed_resources は bug #25 対応後、修飾名に正規化済み）。スキーマ変更・マイグレーションなし。

### dsl 層

[`AppSyncPolicy`](../../internal/dsl/appsource_types.go) に `AllowManualOverride bool \`yaml:"allowManualOverride,omitempty"\`` を追加。JSON Schema（`schemas/unified-cd.schema.json`）を再生成。

### controller 層

共通ガード関数を1つ追加し、**全 kind の apply/delete ハンドラの先頭**（parse 後、store 書き込み前）に挟む:

| エンドポイント | ハンドラ | ガード対象 kind/name |
|---|---|---|
| `POST /api/v1/jobs` | `handleApplyJob` | Job / `QualifiedName()` |
| `DELETE /api/v1/jobs/*` | `handleDeleteJob` | Job / 修飾名 |
| `POST /api/v1/schedules` | `handleApplySchedule` | Schedule / metadata.name |
| `DELETE /api/v1/schedules/{name}` | `handleDeleteSchedule` | 同上 |
| `POST /api/v1/webhooks` | `handleApplyWebhook` | WebhookReceiver / metadata.name |
| `DELETE /api/v1/webhooks/{name}` | `handleDeleteWebhook` | 同上 |
| `POST /api/v1/gitcredentials` | `handleUpsertGitCredential` | GitCredential / name |
| `DELETE /api/v1/gitcredentials/{name}` | `handleDeleteGitCredential` | 同上 |
| `POST /api/v1/appsources` | `handleApplyAppSource` | AppSource / metadata.name |
| `DELETE /api/v1/appsources/{name}` | `handleDeleteAppSource` | 同上 |

ガードの判定順:

1. `FindManagingAppSource(kind, name)` → 未管理なら許可。
2. **自己管理の例外**: kind=AppSource かつ 管理元 AppSource 名 == 対象名（app-of-apps のルートが自分自身を管理）なら許可。これを塞ぐと Git 側が壊れた際に修復不能になる。
3. 管理元 spec の `syncPolicy.allowManualOverride: true` なら許可。
4. それ以外は 409 Conflict:
   `resource Job "team-a/build" is managed by AppSource "my-pipelines"; update it in Git (<repoURL>), or set syncPolicy.allowManualOverride: true on the AppSource`

補足:

- リコンサイラの書き込み経路（`applyResource` → store 直接）はガードを通らない（影響なし）。`POST /appsources/{name}/sync` も無関係。
- `FindManagingAppSource` の DB エラーは **fail-close**（500 で書き込み拒否）。保護機能が DB 障害時に素通しにならないようにする。
- 管理元 spec の JSON parse に失敗した場合も fail-close（拒否）。

### CLI

変更なし（HTTP エラー本文は既存のエラーパスで表示される）。必要ならメッセージ整形のみ。

### テスト（TDD）

- store: `FindManagingAppSource` 一致 / 不一致 / 修飾名 / 複数 AppSource。
- controller（kind ごと）: 管理下 apply → 409、管理下 delete → 409、`allowManualOverride: true` → 成功、未管理 → 成功、自己管理 AppSource の apply → 成功。
- 既存の AppSource 同期 e2e が引き続き通ること（リコンサイラ経路が阻害されないことの回帰確認）。

### ドキュメント

`docs/resources.md` の AppSource 節に保護動作・`allowManualOverride`・409 の意味を追記。

## 機能2: `unified-cli export`

### コマンド（承認済みの決定）

```
unified-cli export -o <dir> [--unmanaged-only] [--force]
```

- デフォルトは全リソース（Job / Schedule / WebhookReceiver / GitCredential / AppSource）。
- `--unmanaged-only`: どの AppSource の `managedResources` にも含まれないリソースのみ（GitOps 移行用）。
- `--force`: 出力先が空でないディレクトリでも上書き。未指定で非空ならエラー停止。
- Secret は値を API から取得できないため対象外。終了サマリで明示する（例: `exported 12 resources (3 skipped as managed); secrets are not exported`）。

### 出力レイアウト

エクスポート先をそのまま AppSource の `path` に指定して**同一の修飾名で往復**できる配置:

```
<out>/
  team-a/build.yaml        # Job: 修飾パスのディレクトリに配置、metadata.name は leaf のみ
  hello.yaml               # ルート直下の Job（未修飾）
  schedules/nightly.yaml   # 非 Job kind は専用ディレクトリ（ディレクトリは Job 名にしか影響しない）
  webhookreceivers/gh.yaml
  gitcredentials/github.yaml
  appsources/my-pipelines.yaml
```

- Job の `path` アノテーションは出力しない（ディレクトリ配置が修飾を与える）。
- Job の修飾パス先頭セグメントが予約ディレクトリ名（`schedules` / `webhookreceivers` / `gitcredentials` / `appsources`）と衝突する場合はエラーで通知（黙って壊れた往復をさせない）。
- YAML は DB に保存された spec JSON からの再構成であり、元ファイルのコメント・キー順は保持されない（DB が保持していないため原理的に不可能）。ドキュメントに明記。

### サーバー側の追補（additive、後方互換）

エクスポートに必要な情報が現行のリスト API に不足しているため、以下を `omitempty` で追加する:

| 型 | 追加フィールド | 理由 |
|---|---|---|
| `api.ScheduleMeta` | `Params map[string]string` | 現状 cron/jobName のみで params が落ちる |
| `api.WebhookReceiverMeta` | `Spec json.RawMessage` | 現状 name/updatedAt のみで spec が取れない |
| `api.AppSourceMeta` | `SyncPolicy`（interval/prune/allowManualOverride）, `ManagedResources []ResourceRef` | AppSource YAML 再構成と `--unmanaged-only` 判定に必要 |

`api.Job`（spec 込み）と `api.GitCredentialMeta`（host/credType/secretRef）は現状で足りる。

### CLI 実装

1. 各リスト API を呼び、kind ごとに `apiVersion: unified-cd/v1` / `kind` / `metadata.name` / `spec` の完全な YAML ドキュメントへ再構成（spec JSON → `yaml.v3` で変換）。
2. `--unmanaged-only` 時は `/appsources` の `managedResources` を集合化し `{kind,name}` 完全一致でスキップ。
3. ファイル書き出し（レイアウトは上記）。リスト API エラー・書き込みエラーは即時中断し、部分出力を成功扱いにしない。

### テスト（TDD）

- CLI テスト（既存のモックサーバーパターン）: 全 kind の出力レイアウト、`--unmanaged-only` のスキップ、非空ディレクトリ拒否 / `--force`、予約ディレクトリ名衝突エラー。
- 往復検証: エクスポート結果の各ファイルを `probeKind` / 各 `dsl.Parse*` に通し、修飾名含め元のリソース名と一致することを検証（リコンサイラの命名ロジックとの整合）。

### ドキュメント

`docs/cli.md` にコマンド追加。`docs/resources.md` の AppSource 節に「手動リソースの Git 移行手順」を追記: export → Git にコミット → AppSource 登録。初回同期で同名リソースがそのまま upsert され `managed_resources` に載るため、旧手動リソースの削除作業は不要で、以後は自動的に保護対象になる。

## スコープ外（YAGNI / 将来）

- selfHeal（DB 直接変更の自動巻き戻し）。API 迂回の書き込みは想定リスクとして許容。
- `SealedSecret` kind（公開鍵暗号化シークレットの Git 管理）。別設計とする。
- export の kind フィルタ（`--kinds job,...`）、単一ファイル出力モード。
- Web UI での保護状態表示（managed バッジ等）。API が 409 を返すため UI は既存エラー表示で足りる。

## 実装順序の提案

機能1（store → dsl → controller ガード → docs）→ 機能2（API 追補 → CLI export → docs）。機能2の `--unmanaged-only` は機能1と独立だが、`AppSourceMeta.ManagedResources` の追加は両機能で共有しない（機能1はサーバー内で store を直接引く）ため、順序依存はない。
