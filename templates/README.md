# ジョブテンプレート集

`unified-cd` の再利用可能な `Job` テンプレート集です。`unified-cd apply templates/<file>.yaml` で登録して
`call:` から呼び出すか、`uses:` で `git://` URI 経由でインラインするかのどちらかで使用します。

```yaml
# 登録済みジョブとして呼び出す場合
steps:
  - name: notify
    call:
      job: slack-notify
      with:
        status: success
        job_name: my-job
        run_id: "{{ .RunID }}"

# git テンプレートとしてインラインする場合
steps:
  - name: notify
    uses:
      job: git://github.com/your-org/ci-templates/slack-notify.yaml@v1
      with:
        status: success
        job_name: my-job
        run_id: "{{ .RunID }}"
```

## テンプレート一覧

| テンプレート | 用途 | 必要ツール | 推奨エージェントラベル | 使用シークレット |
|---|---|---|---|---|
| `git-checkout.yaml` | Git リポジトリのクローン/チェックアウト（HTTPS/SSH、LFS、sparse checkout、submodule 対応） | git, (git-lfs) | `git:true` | `token_secret` で指定（例: github-token）／`ssh_key_secret` で指定 |
| `slack-notify.yaml` | Slack Incoming Webhook への通知 | curl | - | `slack-webhook-url` |
| `github-commit-status.yaml` | GitHub コミットステータス更新 | curl | - | `github-token` |
| `notify-webhook.yaml` | 汎用 Webhook への JSON POST 通知 | curl | - | `url_secret` で指定（省略時は平文 `url`） |
| `notify-email.yaml` | SMTP 経由のメール通知 | curl（SMTP(S) 対応ビルド） | - | `smtp_url_secret`, `username_secret`, `password_secret` で指定 |
| `github-pr-comment.yaml` | GitHub PR/Issue へのコメント投稿 | curl | - | `token_secret`（デフォルト: `github-token`） |
| `gitlab-commit-status.yaml` | GitLab コミットステータス更新 | curl | - | `token_secret`（デフォルト: `gitlab-token`） |
| `docker-build-push.yaml` | Docker イメージのビルド & プッシュ（buildx マルチプラットフォーム対応） | docker, (docker buildx) | `docker:true` | `username_secret` / `password_secret` で指定（省略可） |
| `setup-go.yaml` | Go モジュール/ビルドキャッシュのセットアップ | go | `go:true` | なし |
| `setup-node.yaml` | Node.js 依存関係キャッシュのセットアップ（npm ci） | node, npm | `node:true` | なし |
| `github-release.yaml` | GitHub リリース作成 & アセットアップロード（curl のみ、gh 不要） | curl | - | `token_secret`（デフォルト: `github-token`） |
| `semver-bump.yaml` | Conventional Commits に基づく次バージョン算出 | git | `git:true` | なし |
| `k8s-deploy.yaml` | Kubernetes マニフェスト適用 & ロールアウト待機 | kubectl | `kubectl:true` | `kubeconfig_secret` で指定 |
| `helm-upgrade.yaml` | Helm upgrade --install | helm, kubectl | `helm:true`, `kubectl:true` | `kubeconfig_secret` で指定 |
| `rsync-deploy.yaml` | rsync 経由のリモートデプロイ | rsync, ssh | `rsync:true` | `ssh_key_secret` で指定 |
| `s3-sync.yaml` | S3 互換オブジェクトストレージへの同期（AWS/MinIO/Garage） | aws (AWS CLI v2) | `aws:true` | `access_key_secret` / `secret_key_secret` で指定 |
| `smoke-check.yaml` | デプロイ後の URL ポーリングによるスモークテスト | curl | - | なし |
| `unity-build.yaml` | Unity バッチモードビルド（Android/iOS/WebGL 等） | Unity Editor | `unity:true` | `license_*_secret` で指定（省略可） |
| `fastlane-upload.yaml` | fastlane レーン実行（App Store Connect API キー対応） | fastlane, bundler, Xcode | `macos:true`, `fastlane:true` | `asc_*_secret` で指定（省略可） |
| `google-play-upload.yaml` | Google Play への AAB アップロード（fastlane supply） | fastlane | `fastlane:true` | `service_account_json_secret` で指定 |

## 規約

各テンプレートは以下の house style に従います（`git-checkout.yaml` / `slack-notify.yaml` が原型）:

- `apiVersion: unified-cd/v1`, `kind: Job`。`spec.params.inputs` で `name` / `type` / `required` / `default` / `description`
  を宣言する。`description` は日本語で記述する。
- ファイル先頭のヘッダーコメントに、テンプレートの目的・必要なシークレット（`unified-cd secret set ...` の実行例）・
  `git://` テンプレートとして参照する場合の使用例を書く。
- ツールの前提条件（docker, kubectl, helm, aws CLI, fastlane, Unity Editor 等）はヘッダーコメントに明記する。
  実行に必要なエージェントラベル（例: `docker:true`, `kubectl:true`, `unity:true`）は本 README の表に記載する
  **命名規約であり、`agentSelector` として強制されるものではない**（各利用者がジョブ側で `agentSelector` を設定する）。
- パラメータやシークレットは `env:` にマッピングしてから POSIX `sh`（`set -eu`、bashism 禁止）のスクリプトで使う。
  シェルコードに直接文字列展開しない。
- 任意（optional）シークレットの間接参照パターン:
  `"{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}"`
- 秘密鍵やトークン等の機微情報をファイルに書き出す場合は `mktemp` で一時ファイルを作り `chmod 600` し、
  `trap ... EXIT` でクリーンアップする。
- `cache:` ステップの `key` / `restoreKeys` はテンプレート式を展開できる（`{{ hashFile "path/glob" }}` を使う。
  docs 上の `checksum` という関数名は存在しないので注意）が、`path` は展開されない**固定文字列**である
  （`internal/agent/agent.go` の `executeCacheStep` 参照）。可変のキャッシュ対象パスが必要な場合は
  `setup-go.yaml` のようにキャッシュ対象ごとにステップを分けるか、`setup-node.yaml` のようにシンボリックリンクで
  固定パスへ寄せる。
- `type: array` の入力パラメータは、YAML 配列として渡された値がジョブ実行時に改行区切りの文字列として
  環境変数に入る（`git-checkout.yaml` の `sparse_paths` 参照）。デフォルト値は空配列ではなく空文字列
  `default: ""` で宣言する（配列型でも文字列のデフォルトを使うのが既存の慣習）。
