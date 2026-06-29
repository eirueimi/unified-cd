# unified-cd-runner Base Image 設計書

**Date:** 2026-06-29

## 概要

k8s-agent のデフォルト Pod イメージ（`podImage`）として使う、言語非依存の CI/CD 汎用ベースイメージを作成する。Debian 12 slim ベースで、kubectl・helm・git・curl 等の標準的な CI/CD ツールを同梱し、GHCR に公開する。

---

## イメージ仕様

**イメージ名:** `ghcr.io/eirueimi/unified-cd-runner`

**ベース:** `debian:12-slim`

### 含めるツール

| カテゴリ | ツール | 備考 |
|----------|--------|------|
| VCS | `git`, `openssh-client` | apt |
| HTTP | `curl`, `wget`, `ca-certificates` | apt |
| データ処理 | `jq` | apt |
| データ処理 | `yq` | GitHub Releases からバイナリ取得（`mikefarah/yq`）|
| ビルド | `make`, `unzip`, `tar`, `xz-utils` | apt |
| セキュリティ | `openssl`, `gnupg` | apt |
| Kubernetes | `kubectl` | apt（`pkgs.k8s.io`）最新 stable |

**含めないもの:** docker CLI、言語ランタイム（Go / Node.js / Python）

---

## 変更ファイル一覧

| ファイル | 変更内容 |
|---------|---------|
| `docker/runner.Dockerfile` | 新規作成 |
| `.github/workflows/release-docker.yml` | runner イメージのビルド・push ステップを追加 |
| `internal/k8sagent/config.go` | `PodImage` デフォルト値を変更 |
| `examples/config/k8s-agent.yaml` | `podImage` コメント・値を更新 |

---

## Dockerfile 設計

```dockerfile
FROM debian:12-slim

# apt ツール
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    openssh-client \
    curl \
    wget \
    ca-certificates \
    jq \
    make \
    unzip \
    tar \
    xz-utils \
    openssl \
    gnupg \
  && rm -rf /var/lib/apt/lists/*

# yq（YAML プロセッサ）
RUN curl -sSL https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 \
    -o /usr/local/bin/yq && chmod +x /usr/local/bin/yq

# kubectl
RUN curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.32/deb/Release.key \
    | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg && \
    echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.32/deb/ /' \
    > /etc/apt/sources.list.d/kubernetes.list && \
    apt-get update && apt-get install -y --no-install-recommends kubectl \
    && rm -rf /var/lib/apt/lists/*

CMD ["/bin/bash"]
```

---

## GitHub Actions ワークフロー追加

`.github/workflows/release-docker.yml` に以下のステップを追加：

```yaml
- name: Build and push runner
  uses: docker/build-push-action@v6
  with:
    context: .
    file: docker/runner.Dockerfile
    platforms: linux/amd64,linux/arm64
    push: true
    tags: |
      ghcr.io/eirueimi/unified-cd-runner:latest
      ghcr.io/eirueimi/unified-cd-runner:${{ github.ref_name }}
```

---

## k8s-agent デフォルト変更

**`internal/k8sagent/config.go`:**

```go
// 変更前
PodImage: "golang:1.24-alpine"

// 変更後
PodImage: "ghcr.io/eirueimi/unified-cd-runner:v0.0.3"
```

**`examples/config/k8s-agent.yaml`:**

```yaml
# 変更前
podImage: golang:1.24-alpine

# 変更後
podImage: ghcr.io/eirueimi/unified-cd-runner:v0.0.3
```

---

## 注意事項

- arm64 の `yq` バイナリは `yq_linux_arm64` を使う必要があるため、multi-platform build 時は `TARGETARCH` で分岐する
- kubectl バージョンは `v1.32` チャネルを使用（将来的にチャネル変更が必要になる場合は Dockerfile の APT リポジトリ URL を更新）
- デフォルト `podImage` は `v0.0.3` で固定。イメージを更新した際はコードのデフォルト値も合わせて更新する運用とする
