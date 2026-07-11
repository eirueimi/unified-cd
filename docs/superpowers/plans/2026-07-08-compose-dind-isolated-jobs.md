# Compose DinD for Isolated Jobs — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** docker-compose の host agent が、DinD サイドカー経由で隔離(非 native)ジョブを実行できるようにする。

**Architecture:** `docker` CLI は `DOCKER_HOST` を尊重するため Go コード変更は不要。両 compose に `docker:27-dind`(`privileged: true`)サイドカーを追加し、agent に `DOCKER_HOST=tcp://dind:2375` と docker CLI を与え、workspace を agent・dind 両方に同一パスの共有ボリュームでマウントして bind mount 整合を確保する。

**Tech Stack:** Docker Compose、docker:27-dind、alpine `docker-cli` パッケージ。

## Global Constraints

- dind は `docker:27-dind`、`privileged: true`、`DOCKER_TLS_CERTDIR: ""`(平文 TCP 2375)。
- 共有ボリューム名 `ucd-workspaces`、マウントポイントは agent・dind とも **`/ucd-workspaces`**(同一絶対パス必須)。agent の `UNIFIED_AGENT_WORKSPACE_DIR: /ucd-workspaces`。
- dind の `/var/lib/docker` はボリューム `dind-storage` で永続化。
- agent の追加 env: `DOCKER_HOST: tcp://dind:2375`、`UNIFIED_AGENT_WORKSPACE_DIR: /ucd-workspaces`。agent の `depends_on` に `dind: { condition: service_healthy }`。
- イメージにはクライアントのみ(`apk add --no-cache docker-cli`)。`dockerd`/`docker` デーモンパッケージは入れない。
- Go コードは変更しない。native ジョブの挙動は不変。
- 対象: ルート `docker-compose.yaml`(dev/air)と `deployments/docker/docker-compose.yaml`(prod)の両方。

---

### Task 1: 両 Dockerfile に docker CLI を追加

**Files:**
- Modify: `docker/dev.Dockerfile`
- Modify: `docker/agent.Dockerfile`

**Interfaces:**
- Produces: agent/controller イメージ内に `docker`(CLI)コマンドが存在する状態。Task 2/3 の compose がこれに依存。

- [ ] **Step 1: dev.Dockerfile に docker-cli 追加**

`docker/dev.Dockerfile` の `apk add` 行に `docker-cli` を追加。変更後の全文:

```dockerfile
FROM golang:1.26-alpine
RUN apk add --no-cache bash git ca-certificates docker-cli && \
    go install github.com/air-verse/air@latest
WORKDIR /app
```

- [ ] **Step 2: agent.Dockerfile に docker-cli 追加**

`docker/agent.Dockerfile` のランタイムステージの `apk add` 行に `docker-cli` を追加。変更後の該当箇所:

```dockerfile
# Stage 2: runtime with a shell (bash) for run: steps and git for uses: templates
FROM alpine:3.20
RUN apk add --no-cache bash coreutils git ca-certificates docker-cli
COPY --from=build /agent /usr/local/bin/agent
ENTRYPOINT ["agent"]
```

- [ ] **Step 3: 両イメージがビルドでき docker CLI が入ることを確認**

Run:
```bash
docker build -f docker/agent.Dockerfile -t ucd-agent-dind-check . && \
docker run --rm --entrypoint docker ucd-agent-dind-check --version
```
Expected: ビルド成功後、`Docker version 2x.x.x, build …` が表示される(daemon 不要、CLI のみ）。

Run:
```bash
docker build -f docker/dev.Dockerfile -t ucd-dev-dind-check . && \
docker run --rm ucd-dev-dind-check docker --version
```
Expected: 同様に `Docker version …` が表示される。

- [ ] **Step 4: Commit**

```bash
git add docker/dev.Dockerfile docker/agent.Dockerfile
git commit -m "build(docker): add docker-cli to dev and agent images for DinD isolated jobs"
```

---

### Task 2: ルート docker-compose.yaml に dind + agent 配線 + E2E 検証

**Files:**
- Modify: `docker-compose.yaml`(ルート)

**Interfaces:**
- Consumes: Task 1 の docker-cli 入りイメージ。
- Produces: 隔離ジョブが動く dev スタック。Task 3 が同じパターンを prod compose に適用。

- [ ] **Step 1: dind サービスを追加**

`docker-compose.yaml` の `services:` 配下(agent の直前が読みやすい)に追加:

```yaml
  # ---- Docker-in-Docker (container runtime for isolated jobs) ----
  # The host agent runs isolated (non-native) jobs by shelling out to `docker`,
  # which talks to this daemon via DOCKER_HOST. Job containers live inside dind,
  # not on the host. The ucd-workspaces volume is mounted at the SAME path here
  # and in the agent so `docker run -v <workspace>:...` (resolved by THIS daemon)
  # sees the files the agent wrote.
  dind:
    image: docker:27-dind
    privileged: true
    environment:
      DOCKER_TLS_CERTDIR: ""
    volumes:
      - ucd-workspaces:/ucd-workspaces
      - dind-storage:/var/lib/docker
    healthcheck:
      test: [ "CMD", "docker", "info" ]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 20s
    restart: unless-stopped
```

- [ ] **Step 2: agent サービスに DinD 配線を追加**

`docker-compose.yaml` の `agent:` サービスを、volumes に `ucd-workspaces`、environment に 2 変数、depends_on に dind を足した形へ変更。変更後の全文:

```yaml
  # ---- agent (Linux/Docker) ----
  agent:
    build:
      context: .
      dockerfile: docker/dev.Dockerfile
    volumes:
      - .:/app
      - gocache:/gocache
      - gomodcache:/gomodcache
      - ucd-workspaces:/ucd-workspaces
    command: air -c .air.agent.toml
    depends_on:
      controller:
        condition: service_healthy
      dind:
        condition: service_healthy
    environment:
      GOCACHE: /gocache
      GOMODCACHE: /gomodcache
      UNIFIED_SERVER: http://controller:8080
      UNIFIED_AGENT_TOKEN: ${UNIFIED_TOKEN:-dev-token-change-me}
      UNIFIED_AGENT_ID: docker-agent-1
      UNIFIED_AGENT_LABELS: kind:docker,pool:default
      DOCKER_HOST: tcp://dind:2375
      UNIFIED_AGENT_WORKSPACE_DIR: /ucd-workspaces
    restart: unless-stopped
```

- [ ] **Step 3: volumes ブロックに 2 ボリュームを追加**

`docker-compose.yaml` 末尾の `volumes:` ブロックを変更後の全文へ:

```yaml
volumes:
  pgdata:
  garagedata:
  gocache:
  gomodcache:
  ucd-workspaces:
  dind-storage:
```

- [ ] **Step 4: compose 構文の検証**

Run: `docker compose -f docker-compose.yaml config >/dev/null && echo OK`
Expected: `OK`(YAML/スキーマが妥当。エラー出力が無いこと)。

- [ ] **Step 5: スタック起動(ジョブ実行に必要なサービスのみ)**

Run:
```bash
docker compose up -d --build postgres garage controller dind agent
docker compose ps
```
Expected: `postgres`/`garage`/`controller`/`dind` が healthy、`agent` が running。初回は air が agent をビルドするため controller/agent の起動に時間がかかる(数十秒〜)。

必要なら agent がクレームを開始するまでログ確認:
Run: `docker compose logs --tail=20 agent`
Expected: 登録成功後に claim をポーリングしているログ(エラーループが無いこと)。

- [ ] **Step 6: 隔離ジョブの受け入れ E2E(hello.yaml)**

`examples/jobs/hello.yaml` は `native: true` を持たない=隔離ジョブで、`agentSelector: kind:docker` はこの agent にマッチする。CLI コンテナ経由で apply/trigger する(ホストに CLI が無くても controller 経由で実行できる):

Run:
```bash
export UNIFIED_SERVER=http://localhost:8080
export UNIFIED_TOKEN=dev-token-change-me
go run ./cmd/unified-cli apply -f examples/jobs/hello.yaml
go run ./cmd/unified-cli run trigger hello-docker
```
Expected: apply 成功、trigger で run が作成され、しばらくして `Succeeded`。

run の状態確認(run ID は trigger 出力に表示される):
Run: `go run ./cmd/unified-cli run show <run-id>` もしくは Web UI `http://localhost:8080/ui/`
Expected: ステータス `Succeeded`、2 ステップ(hello / check-env)がログ出力を持つ。`check-env` の `uname -a` 出力がジョブコンテナ(runner イメージ)内のものであること。

ジョブコンテナが dind 上に立ったことの確認:
Run: `docker compose exec dind docker ps -a`
Expected: 直近の run に対応する pause / job コンテナ(実行後は Exited)が dind の daemon 上に存在する。ホスト側 `docker ps -a` には現れない(隔離の確認)。

- [ ] **Step 7: workspace 保持の検証(隔離ジョブ・複数ステップ)**

`native` 無し・`agentSelector: kind:docker` の 2 ステップジョブで、step1 が書いたファイルを step2 が読めることを確認する。検証用ファイルを作成:

`/tmp/ucd-ws-check.yaml`(スクラッチに置く):
```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: ws-persist-check
spec:
  agentSelector:
    - kind:docker
  steps:
    - name: write
      run: echo "persisted-value-42" > marker.txt && cat marker.txt
    - name: read
      run: test "$(cat marker.txt)" = "persisted-value-42" && echo "WORKSPACE OK"
```

Run:
```bash
go run ./cmd/unified-cli apply -f /tmp/ucd-ws-check.yaml
go run ./cmd/unified-cli run trigger ws-persist-check
```
Expected: run が `Succeeded`。`read` ステップのログに `WORKSPACE OK`(共有ボリュームで bind mount が正しく解決され、ステップ間で workspace が保持されている証拠)。もし `read` が「No such file」で失敗する場合は共有ボリュームのパス不整合を疑う(agent と dind の `/ucd-workspaces` マウントと `UNIFIED_AGENT_WORKSPACE_DIR` を確認)。

- [ ] **Step 8: native ジョブの非回帰確認**

`examples/jobs/native-build.yaml`(`native: true`)がこの agent で従来どおり成功することを確認:

Run:
```bash
go run ./cmd/unified-cli apply -f examples/jobs/native-build.yaml
go run ./cmd/unified-cli run trigger <native-build-job-name>
```
(ジョブ名は `examples/jobs/native-build.yaml` の `metadata.name` を使用。)
Expected: `Succeeded`。native ジョブはホスト(agent コンテナ)上で直接実行され、dind を使わない(挙動不変)。

- [ ] **Step 9: Commit**

```bash
git add docker-compose.yaml
git commit -m "feat(compose): DinD sidecar so the dev agent runs isolated jobs"
```

---

### Task 3: deployments/docker/docker-compose.yaml に同じ配線を適用

**Files:**
- Modify: `deployments/docker/docker-compose.yaml`

**Interfaces:**
- Consumes: Task 1 の docker-cli 入り agent.Dockerfile / 公開イメージ。

- [ ] **Step 1: dind サービスを追加**

`deployments/docker/docker-compose.yaml` の `services:` 配下(agent の直前)に追加(ルートと同一定義):

```yaml
  # ---- Docker-in-Docker (container runtime for isolated jobs) ----
  # The agent runs isolated (non-native) jobs by shelling out to `docker`, which
  # talks to this daemon via DOCKER_HOST. Job containers live inside dind. The
  # ucd-workspaces volume is mounted at the SAME path here and in the agent so
  # bind mounts resolved by this daemon see the agent's workspace files.
  dind:
    image: docker:27-dind
    privileged: true
    environment:
      DOCKER_TLS_CERTDIR: ""
    volumes:
      - ucd-workspaces:/ucd-workspaces
      - dind-storage:/var/lib/docker
    healthcheck:
      test: [ "CMD", "docker", "info" ]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 20s
    restart: unless-stopped
```

- [ ] **Step 2: agent サービスに DinD 配線を追加**

`agent:` サービスを、volumes(新規)・environment 2 変数・depends_on に dind を足した形へ。変更後の全文:

```yaml
  # ---- Agent (Linux/Docker worker) ----
  agent:
    image: ghcr.io/eirueimi/unified-cd-agent:${UNIFIED_CD_VERSION:-latest}
    build:
      context: ../../
      dockerfile: docker/agent.Dockerfile
    depends_on:
      controller:
        condition: service_healthy
      dind:
        condition: service_healthy
    environment:
      UNIFIED_SERVER: http://controller:8080
      UNIFIED_AGENT_TOKEN: ${UNIFIED_TOKEN:-dev-token-change-me}
      UNIFIED_AGENT_ID: ${UNIFIED_AGENT_ID:-docker-agent-1}
      UNIFIED_AGENT_LABELS: ${UNIFIED_AGENT_LABELS:-kind:docker,pool:default}
      DOCKER_HOST: tcp://dind:2375
      UNIFIED_AGENT_WORKSPACE_DIR: /ucd-workspaces
    volumes:
      - ucd-workspaces:/ucd-workspaces
    restart: unless-stopped
```

- [ ] **Step 3: volumes ブロックに 2 ボリュームを追加**

末尾の `volumes:` ブロックを変更後の全文へ:

```yaml
volumes:
  pgdata:
  garagedata:
  ucd-workspaces:
  dind-storage:
```

- [ ] **Step 4: compose 構文の検証**

Run: `docker compose -f deployments/docker/docker-compose.yaml config >/dev/null && echo OK`
Expected: `OK`。

- [ ] **Step 5: agent イメージがビルドできることの確認**

Run: `docker compose -f deployments/docker/docker-compose.yaml build agent`
Expected: ビルド成功(Task 1 の docker-cli 入り agent.Dockerfile を使う)。

（フル E2E はルート compose(Task 2)で実施済み。prod compose は同一配線のため config + build 検証で足りると判断。）

- [ ] **Step 6: Commit**

```bash
git add deployments/docker/docker-compose.yaml
git commit -m "feat(compose): DinD sidecar in the production compose agent"
```

---

### Task 4: ドキュメント + spec ステータス

**Files:**
- Modify: `docs/agents.md`
- Modify: `docs/superpowers/specs/2026-07-08-compose-dind-isolated-jobs-design.md`

- [ ] **Step 1: docs/agents.md に追記**

`docs/agents.md` 内の native/isolated(claim pod)を説明しているセクションを開き(`grep -n "native" docs/agents.md` で位置特定)、docker-compose の host agent に関する最小の追記を、その説明に最も近い箇所へ挿入する。追記文面(既存のトーン・言語に合わせる。ファイルが英語ならこの内容を英語で):

> The bundled docker-compose stacks (repo-root `docker-compose.yaml` and `deployments/docker/docker-compose.yaml`) run a Docker-in-Docker (`dind`, `privileged: true`) sidecar so the host agent can execute isolated (non-native) jobs, not just `native: true` ones. The agent reaches the dind daemon via `DOCKER_HOST=tcp://dind:2375` and shares the `ucd-workspaces` volume with dind at the same path so job-container bind mounts resolve. The dind daemon listens on plain TCP 2375 on the compose network — this is for local/dev use; do not expose it beyond the compose network.

（英語ドキュメントなら上記英文をそのまま、日本語なら同義の日本語で。挿入位置は「native と isolated の違い」を述べている段落の直後が自然。）

- [ ] **Step 2: docs 追記の妥当性を目視確認**

Run: `grep -n -A2 "dind\|Docker-in-Docker\|DOCKER_HOST" docs/agents.md`
Expected: 追記した段落が該当セクション内に現れる。

- [ ] **Step 3: spec ステータスを実装済みへ**

`docs/superpowers/specs/2026-07-08-compose-dind-isolated-jobs-design.md` の `- ステータス: 設計レビュー中` を `- ステータス: 実装済み` に変更。

- [ ] **Step 4: Commit**

```bash
git add docs/agents.md docs/superpowers/specs/2026-07-08-compose-dind-isolated-jobs-design.md
git commit -m "docs(agents): document DinD sidecar for compose isolated jobs"
```
