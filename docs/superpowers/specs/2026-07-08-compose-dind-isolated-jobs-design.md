# docker-compose の host agent で隔離(非 native)ジョブを実行可能にする設計

- 日付: 2026-07-08
- ステータス: 設計レビュー中

## 背景と動機

host agent は job-level isolation により、`native: true` のジョブはホスト上で直接実行し、それ以外(隔離ジョブ)は**コンテナランタイム(docker/podman/nerdctl)を要求**する(`internal/agent/agent.go:281`: "isolated job requires a container runtime …")。隔離ジョブの実体は `internal/runtime` が `docker` CLI をシェルアウトして claim pod(pause + runner/job コンテナ)を作り、ワークスペースを bind mount する仕組み。

現状の docker-compose の agent サービス(ルート `docker-compose.yaml` の dev/air 構成、および `deployments/docker/docker-compose.yaml` の prod 構成)には **docker CLI もコンテナランタイムへの到達手段も無い**ため、隔離ジョブは全て失敗し、`native: true` のジョブしか動かせない。

**ゴール**: compose の agent が隔離ジョブも実行できるようにする。`privileged: true` を使ってよい。

## 方針: DinD サイドカー(コード変更なし)

`docker` CLI は `DOCKER_HOST` 環境変数を尊重するため、**Go コードの変更は不要**。以下の compose とイメージの変更だけで実現する。

### 1. dind サイドカーサービス(両 compose に追加)

```yaml
dind:
  image: docker:27-dind
  privileged: true
  environment:
    DOCKER_TLS_CERTDIR: ""            # 平文 TCP 2375(ローカル dev 用途)
  volumes:
    - ucd-workspaces:/ucd-workspaces  # agent と同一パスで共有(bind mount 整合の要)
    - dind-storage:/var/lib/docker    # イメージ/レイヤキャッシュを再起動間で保持
  healthcheck:
    test: ["CMD", "docker", "info"]
    interval: 5s
    timeout: 5s
    retries: 10
    start_period: 20s
  restart: unless-stopped
```

- `DOCKER_TLS_CERTDIR: ""` で TLS を無効化し平文 2375 で待受(compose の内部ネットワーク限定なのでローカルでは許容。本番向けの注意は「スコープ外」参照)。
- `dind-storage` ボリュームで `/var/lib/docker` を永続化し、pause/runner/ジョブイメージの pull キャッシュをコンテナ再起動間で保持(コールドスタート緩和)。

### 2. agent サービスの変更(両 compose)

- 環境変数を追加:
  - `DOCKER_HOST: tcp://dind:2375`
  - `UNIFIED_AGENT_WORKSPACE_DIR: /ucd-workspaces`(既定 `~/workspace` を固定の絶対パスに)
- ボリュームに `ucd-workspaces:/ucd-workspaces` を追加(dind と**同一マウントポイント**)。
- `depends_on` に `dind: { condition: service_healthy }` を追加。

### 3. ワークスペース整合性(この設計の肝)

隔離ジョブでは agent が `docker run -v /ucd-workspaces/working<slot>/<job>:/workspace …` を発行するが、この `-v` の**ホスト側パスは docker デーモン(=dind)のファイルシステムで解決される**。したがって agent が書き込む workspace ディレクトリが、dind コンテナ内でも**同じ絶対パス**に存在しなければジョブコンテナから中身が見えない。

名前付きボリューム `ucd-workspaces` を agent・dind の両方で `/ucd-workspaces` にマウントし、agent の workspace ベースを `UNIFIED_AGENT_WORKSPACE_DIR=/ucd-workspaces` に固定することでこれを満たす。

デーモンが解決する bind mount は **workspace の1種類のみ**であることをコードで確認済み(`internal/agent/claim_pod.go:183` の `{HostPath: m.workDir, ContainerPath: m.mountPath}`、および `internal/agent/workspace.go:88` の runner 実行時マウント。いずれも workspace ベース配下)。スコープ用の一時ディレクトリ(`internal/agent/backend_host.go` の `os.MkdirTemp` 群、`internal/agent/scope.go`)は `docker cp`(CLI クライアント側でファイルを読む)経由なので共有ボリューム不要。

### 4. イメージへの docker CLI 追加

- `docker/dev.Dockerfile`(dev/air。ルート compose の agent・controller が共用): `apk add --no-cache docker-cli` を追加。
- `docker/agent.Dockerfile`(prod。deployments compose の agent): 同様に `docker-cli` を追加。
- **クライアントのみ**を入れる(`docker-cli` パッケージ。デーモン `docker` パッケージや `dockerd` は入れない — デーモンは dind が担う)。

### 5. 変更対象ファイル

- `docker-compose.yaml`(ルート、dev)— dind サービス + agent 変更 + volumes 追記
- `deployments/docker/docker-compose.yaml`(prod)— 同上
- `docker/dev.Dockerfile` — docker-cli
- `docker/agent.Dockerfile` — docker-cli
- ドキュメント: `docs/agents.md`(host agent と native/isolated の扱いを記載しているドキュメント)に、compose の agent が dind サイドカー経由で隔離ジョブを実行できるようになった旨と、`privileged: true`/平文 TCP 2375 の注意を追記。実装時に該当セクションを確認し、最も自然な箇所へ最小追記する。

## スコープ外(記録)

- 本番運用向けの TLS 付き dind(`DOCKER_TLS_CERTDIR` を設定し 2376 + 証明書共有)。ローカル dev は平文 2375 で足りるため、注意書きのみ残す。
- DooD(ホスト socket マウント)方式は不採用(ホスト docker を汚す・隔離が弱い・bind mount パス整合が困難)。
- k8s-agent(既にクラスタ内で隔離実行可能)。
- Go コードの変更(本設計では発生しない)。
- native ジョブの挙動(不変)。
- runner/pause イメージのバージョン変更。

## テスト / 受け入れ基準

コード非変更のため Go ユニットテストは無改修で緑のはず(回帰確認のみ)。動作確認は手動 E2E:

1. **非回帰**: `native: true` のジョブが従来どおり compose の agent で成功する。
2. **隔離ジョブ成功**: `native` 未指定(=隔離)または `runsIn.image` を使うジョブを apply → agent が `DOCKER_HOST=tcp://dind:2375` 経由で claim pod を作成し、ステップが成功する。`docker -H tcp://dind:2375 ps`(または dind コンテナ内 `docker ps`)で pause/job コンテナが dind 上に立つことを確認。
3. **workspace 保持**: 隔離ジョブ内の複数ステップでファイルを書いて後続ステップから読めること(bind mount 整合の確認)。
4. **両 compose**: ルート `docker-compose.yaml` と `deployments/docker/docker-compose.yaml` の両方で 1〜3 が成立。

受け入れ確認用の最小ジョブ YAML(隔離 + 複数ステップの workspace 保持)は実装フェーズのプランで具体化する。
