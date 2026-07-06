# TODO — 実機検証で見つかった不具合・修正すべき点

2026-07-03 実施の実機検証(docker compose スタック + CLI/HTTP/Webhook 経由の実ジョブ実行)で発見。
検証環境: `docker compose up`(controller + agent + PostgreSQL + Garage)、`unified-cli` はホストでビルド。

---

## 致命的(Critical)

### 1. ステップの失敗/スキップでランが永久にハングし、エージェントが停止する

- **症状:** ステップが1つでも失敗すると(または `if:` が false でスキップされると)、後続ステップの `Skipped` 報告が DB の CHECK 制約違反で HTTP 500 になり、エージェントが無限リトライ。ランは永遠に `Running` のまま、**エージェントは再起動するまで他のランを一切処理しない**。`run cancel` でも復旧しない。
- **原因:** `internal/store/migrations/001_init.up.sql:26` の `step_reports` の CHECK 制約が `('Running','Succeeded','Failed','Cancelled')` のみで、エージェントが送る `Skipped`(`internal/agent/agent.go` の if:-false / 失敗後自動スキップ経路)と `WaitingApproval`(`internal/agent/approval.go:44`)を許可していない。後続マイグレーション(〜016)にも追加なし。
- **再現:** 2ステップのジョブで1ステップ目を `exit 1` にするだけ。`if:` 不使用でも発生。
- **修正案:**
  - 制約に `Skipped` / `WaitingApproval` を追加する新規マイグレーション
  - agent の `retryUntilSuccess` を 4xx/制約系エラーでは打ち切るようにし、不正ステータスでエージェントが二度と詰まらないようにする
  - 失敗を含むマルチステップジョブの e2e テスト追加(ラン最終状態 Failed、後続ステップ Skipped を検証)

### 2. `required` パラメータが強制されず、`default` も適用されない

- **症状:** `required: true` の入力パラメータ未指定でもトリガーが成功し、`{{ .Params.x }}` は空文字に展開。`default: staging` も一切注入されず、CEL の `if:` 条件は `no such key` エラーになる。
- **原因:** `Required`/`Default` は型定義とシリアライズにしか登場せず、`internal/controller/api_runs.go` の `handleTriggerRun` は `req.Params` を無検証で `store.CreateRun` に渡している。
- **docs との矛盾:** `docs/jobs.md` は「required 未指定ならランは即失敗」「default は省略時に適用」と明記。
- **補足(Playwright での UI 検証で確認):** Web UI の Run タブは default をフォームにプレフィルし、required は `*` 表示+未入力時は Run ボタンを disabled にしており**フロントエンドは正しく実装済み**。欠けているのはコントローラ API 側のみ(CLI / API 直叩き / webhook / schedule 経路で発症)。
- **修正案:** trigger 経路(API / webhook / schedule すべて)で、required 欠落は 400 で拒否、省略パラメータに default を充填、型検証も検討。

### 3. `if:` の文書化された構文が実装と不一致 + fail-open で危険

- **症状:** README(〜189行)と `docs/jobs.md`(182–187行)は Go テンプレート構文(`if: '{{ eq .Params.env "production" }}'`)を記載しているが、実装は CEL(`internal/dsl/condition.go`、正しくは `params.env == "production"`)。docs 通りに書くと CEL コンパイルエラー → **fail-open でステップが必ず実行される**(警告は agent ログのみ)。production 限定のはずの deploy が staging トリガーで実行されるのを実機確認。
- **修正案:**
  - README / docs/jobs.md の `if:` 例をすべて CEL に書き換え(webhook の `filters:` は本当に Go テンプレートなのでそのまま)
  - `if:` のコンパイルエラーをランに表面化する(ステップ警告 or ランイベント)、少なくとも fail-open 挙動を目立つ形で文書化

---

## 重要(High)

### 4. `uploadArtifact` が相対パスを解決できない(標準エージェント)

- **症状:** docs の書式通り `path: out.txt`(相対パス)を指定すると `tar walk "out.txt": lstat out.txt: no such file or directory` で失敗。シェルステップの cwd(`/root/workspace/workingN`)にファイルは存在し、絶対パス指定なら成功。
- **原因:** `internal/agent/agent.go:637` 付近の `executeUploadArtifact` が `ua.Path` をワークスペースと結合せず、エージェントプロセス自身の cwd 基準で解決している。`executeDownloadArtifact` の `destDir` も同様の疑い。
- **修正案:** ExecStep と同じワークスペースディレクトリ基準で相対パスを解決。相対パスアップロードのテスト追加。
- **注意:** アップロード失敗 → 後続ステップ自動スキップ → 上記 1. のハングに連鎖する。

### 5. 承認待ちステップが `run show` に表示されない

- **症状:** approval ステップで待機中、`run show` にステップが1件も出ず、どのステップが承認待ちか見えない。承認自体(`approve <run> <step>`)は機能しラン完走。
- **原因:** `WaitingApproval` の ReportStep がベストエフォート送信(`_ =`)で、上記 1. の制約違反により黙って捨てられている。1. の制約修正で解消するはず(要確認)。

---

## 中(Medium)

### 6. 同梱サンプル `examples/jobs/parallel-steps.yaml` が apply で拒否される

- 先頭ステップが `- name:`(空)のため `spec.steps[0]: name is required` で失敗。名前を付ければ並列実行自体は正常。

### 7. サンプル/docs の CLI フラグ表記が誤り

- 各サンプルのヘッダコメントが `--params image=myapp,tag=v1.0.0` 形式だが、実 CLI は繰り返し指定の `--param k=v`。`examples/jobs/*.yaml` のコメントと該当 docs を修正。

### 8. アーティファクトステップの表示名が `step[1]` になる

- `executeUploadArtifact` / `executeDownloadArtifact` 内の ReportStep に `StepName`(と `StageIndex`)が未設定のため、`run show` でステップ名でなく `step[1]` と表示される。

### 9. ワークスペースがラン間で掃除されず再利用される

- 別ジョブ・別ランでも `working0` が使い回され、前のランのファイルが残留するのを確認。`--clean-workspace` フラグ(オプトイン)は存在する。デフォルト挙動としてよいかの判断と、ジョブ間の情報漏えいリスク(シークレットを書いたファイル等)の文書化が必要。

### 9b. エージェントのワークスペース位置が `~/workspace` にハードコード

- `internal/agent/agent.go:99-101` — `Agent.WorkspaceDir` フィールドは存在するが、`cmd/agent/main.go` からも設定ファイルからも一切セットされない(テストのみ使用)。フラグ `--help` にも `docs/configuration.md` にも設定手段がない。
- Windows では `C:\Users\<user>\workspace` に直接作られる(実機確認)。ユーザーの既存ディレクトリと衝突し得る。`-workspace-dir` フラグ / 設定ファイルキーとして公開すべき。

### 9c. Windows: ラン中キャンセルで子プロセスが孤児化し、ステップが Running 表示のまま残る

- **症状(実機確認):** `sleep 120` 中のランをキャンセル → ランは `Cancelled` になり step の bash.exe は終了するが、**子プロセス `sleep.exe` は生き残る**(30秒以上生存を確認、手動 kill が必要)。また該当ステップの表示が永久に `Running` のまま。
- **原因(推定):** Windows でプロセスツリー kill(Job Object)を使っていない。ステップの最終ステータス報告もキャンセル経路で行われていない。
- **修正案:** Windows では Job Object(または `taskkill /T`)でツリーごと終了。キャンセル時にステップを `Cancelled` として報告。
- docs/jobs.md の「両エージェントは実行中キャンセルを検知し、実行中ステップを中断する」という記述と部分的に矛盾。

### 10. Cancelled ランのログが Web UI に表示されない

- **症状:** Run Detail ページで Cancelled のラン(例: 途中で失敗→キャンセルしたラン)のログ欄が「Waiting for logs…」のまま。同じランを CLI の `logs` で見るとログ(`about to fail`)は取得できる。Succeeded ランのログは UI でも正常表示(SSE)。
- **切り分け:** データは存在しサーバ API からは取れるので、UI(またはログ SSE エンドポイント)の終了済み/キャンセル済みラン向けバックフィル経路の問題。
- 検証: Playwright(スクリーンショット `20-failed-run-detail.png`)。

### 11. [IMPLEMENTED] CLI に `run cancel` がない

- **状態: 実装済み**(`run cancel` は commit `ce8309e` で追加済み。さらに branch `feature/cli-gaps` で CLI の API カバレッジを拡充: `run list-active` / `run outputs` / `run show-yaml` / `run approvals`、`jobs get` / `jobs show-yaml`、`webhook list` / `webhook delete`、`agent get` / `agent runs`。docs/cli.md にも反映)。
- 原記載: API には `POST /api/v1/runs/{id}/cancel` が存在する(204 を返すのを確認)が、CLI の `run` サブコマンドは delete/list/show/trigger のみ。ハングしたランの復旧手段として特に重要(現状 curl 直叩きが必要)。

### 12. エージェント登録が起動時のみで、コントローラ DB 消失後に inventory が不整合になる

- **症状(実機確認):** コントローラの DB が初期化された後(検証中に Docker Desktop の VM リセットで発生)、稼働中のエージェントは claim を継続しジョブも正常実行されるが、`agent list` / UI の Agents ページから消えたまま戻らない。
- **含意:** ①登録は起動時の1回のみで再登録しない。②コントローラは**未登録エージェントからの claim を受理する**(ラベルは claim クエリ由来)。運用上は「見えないエージェントがジョブを実行する」状態になり、監視・監査に穴。
- **修正案:** claim 時に登録レコードを upsert する(または未登録 claim を拒否して再登録を促す)。

---

## 可用性 / フェイルオーバー(設計要)

### A. [IMPLEMENTED] 実行中エージェント死亡時の in-flight run が永久に `Running` で放置される(orphaned-run reaper)

- **状態: 実装済み**(plan: `docs/superpowers/plans/2026-07-04-orphaned-run-reaper.md`)。3コンポーネントで解消: ①エージェント heartbeat(`POST /api/v1/agents/{id}/heartbeat` 15秒毎、claim ポーリングと独立、busy agent の誤 stale-delete も同時に解消)②stuck-run reaper(`internal/controller/stuckrun_reaper.go`、leader 選出、30秒毎、`last_seen_at` が staleAfter=90秒超過 or agent 行消失で `Running` run を `MarkRunFinished(Failed)`、grace=60秒)③k8s orphan-pod GC(`internal/k8sagent/podgc.go`、約1分毎に終端/消滅 run の `ucd-run-*` Pod を削除)。詳細: `docs/high-availability.md` の「Orphaned-Run Recovery」節。**再投入(→Pending)は意図的に不採用**(副作用の二重実行リスクのため Fail のみ)。
- 以下は設計検討時点の分析(参考として残置):

- **分類:** 可用性ギャップ。標準エージェント・k8s-agent 共通(= いわゆる「k8s-agent の HA 化」の本丸)。設計から要検討(brainstorm → spec → plan)。
- **現状の整理(コード確認済み):**
  - **水平スケール自体は既に成立** — 実行は `ClaimNextRun` = `FOR UPDATE SKIP LOCKED`(`internal/store/postgres.go:486`)で二重 claim なし。k8s-agent は `UNIFIED_K8S_AGENT_ID`(pod 名)で各レプリカが一意 ID を持てる。→ Deployment replicas>1 で片方が死んでも残りが claim 継続。
  - **ギャップ:** run は claim 時に `claimed_by` / `claimed_at` / status=`Running` を持ち(`postgres.go:500`)、agents には `last_seen_at` もあるが、**`Running` のまま取り残された run を失敗/再投入する reaper が存在しない**。既存 reaper は `RunScheduler`(Pending→queue)と `RunApprovalReaper`(承認 timeout)のみ。
  - **結果:** エージェントが実行中に死ぬと run は永久に `Running`。k8s では加えて **Pod がリーク**(agent の deferred `DeletePod` が走らない)。TODO #12(DB 消失で inventory ドリフト)と地続き。
- **修正案(設計方針):**
  - **stuck-run reaper**(leader 選出、`RunApprovalReaper` と同型): `claimed_by` エージェントの `last_seen_at` が stale な `Running` run を検出し **Failed 化**(理由「agent lost」)。再投入(→Pending)は副作用の二重実行リスクがあるため既定は Fail が安全側(GitHub Actions 等も runner 喪失時は run を失敗させる)。
  - **k8s orphan-pod GC**: run が終端/消滅した `ucd-run-*` Pod を掃除。
  - **設計論点:** エージェントが実行中もハートビート(`last_seen_at` 更新)するか。claim ポーリングと別立てにするかで stale 判定の信頼性が変わる。#1(不正ステータスで agent が自分で無限リトライ)とは別の詰まり方であり、両方揃って初めて「run が詰まらない」が完成する。

---

## k8s-agent(docker-desktop クラスタで実機検証、2026-07-04)

### 13. デフォルトの sidecarImage が pull できず、全 k8s ジョブが ImagePullBackOff で止まる

- **症状(実機確認):** デフォルト設定 `ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest`(`internal/k8sagent/config.go:38`)の匿名 pull が GHCR で **403 Forbidden**(非公開 or 未公開パッケージ)。サイドカーは全ジョブ Pod に自動注入されるため、アーティファクトを使わないジョブも含め**新規インストールでは全ジョブが起動不能**。ランは Running のまま滞留。
- **補足:** ランナーイメージ `unified-cd-runner:v0.0.3` は public で pull 可能。また PullPolicy 未指定+`:latest` タグ= Always なので、レジストリ到達不能時は常に fail-closed。
- **修正案:** サイドカーイメージを public 化してバージョンタグで pin。runner と同じリリースパイプラインに載せる。

### 14. HEAD のサイドカーイメージ(distroless)とエージェントの転送方式(bash スクリプト)が不整合 — k8s アーティファクト全滅

- **症状(実機確認):** HEAD の `docker/artifact-sidecar.Dockerfile`(commit 338034c)は distroless + `unified-sidecar` バイナリのみ(bash/tar/zstd/curl なし)。しかし `internal/k8sagent/agent.go:307,336` は依然 `tar | zstd | curl` の **bash スクリプト**を `bash -lc` でサイドカーに exec しており、bash 不在で即失敗(exit 1、ログも空)。
- Dockerfile のコメントは「agent は argv でバイナリを exec(シェル不要)」と新方式を前提にしているが、**agent 側の切り替えが未実装**。direct-to-S3 スペック(docs/superpowers/specs/2026-07-04-k8s-sidecar-direct-s3-design.md)の実装が中途の状態でイメージだけ先行。
- **検証:** 1つ前の alpine+bash 版イメージ(600c06d)に差し替えるとアーティファクト転送は動作する(→ #15/#16 の制約下で)。

### 15. k8s のステップが `/workspace` で実行されない(WorkingDir 未設定)

- **症状(実機確認):** `run:` ステップはコンテナのデフォルト cwd で実行され、workspace ボリューム(`/workspace`)ではない。相対パスで作ったファイルは workspace に入らず、アーティファクト転送(mountPath と結合)から見えない。`internal/k8sagent/` に WorkingDir 設定なし。自プロジェクトの e2e テスト(artifact_k8s_test.go)自体が絶対パス `/workspace/f.txt` を使っており、相対パス運用が成立しないことを示唆。
- **修正案:** Pod コンテナ(または exec)に WorkingDir=/workspace を設定。標準エージェント(cwd=workspace)との動作統一。

### 16. k8s uploadArtifact: 単一ファイル不可+失敗しても Succeeded になる(サイレント破損)

- **症状(実機確認):**
  - `tar cf - -C <path> .` は path を**ディレクトリ前提**で扱うため、`path: out.txt`(docs の例はファイル)だと `tar: Cannot open` で常に失敗。ディレクトリなら成功。
  - さらにパイプラインに `pipefail` がないため、**tar が失敗しても curl の PUT が成功すれば step は Succeeded(exit 0)** と報告され、壊れた(空の)アーティファクトが保存される。後続の downloadArtifact が「not a tar archive」で初めて発覚。
- **修正案:** `set -eo pipefail` を付ける。path がファイルの場合は `-C $(dirname) $(basename)` 形式に。ファイル/ディレクトリ両対応のテスト追加(現行 k8s テストは転送を in-memory でフェイクしており実経路が未検証)。
- **良い点(参考):** k8s-agent は失敗時に即 FinishRun(Failed) し、標準エージェントのようなハング(TODO #1)は起きない。ただし後続ステップの Skipped 報告もないため run show に残りステップが表示されない。アーティファクト失敗が agent ログに出ない(標準エージェントは出す)ロギング欠落もあり。

### 21. unified-sidecar の `idle` が S3 未設定で起動即死し、degraded モードが成立しない(TODO 対応レビューで発見)

- **分類:** 致命的(k8s)。#13/#14 の修正(unified-sidecar direct-S3 化)に対するコードレビューで検出(CONFIRMED)。
- **症状:** `cmd/unified-sidecar/main.go:13-22` が **`idle` サブコマンドのディスパッチ前に** `S3ConfigFromEnv` + `NewS3ObjectStore` を実行し、失敗時 `os.Exit(2)` する。`sidecarS3SecretName` 未設定だと Pod に S3 の EnvFrom が注入されない(`podbuilder.go:94-98`)ため env 解決に失敗し、**全ジョブ Pod のサイドカーコンテナ(command: `unified-sidecar idle`)が起動直後に exit 2 で死ぬ**。RestartPolicy=Never のため再起動されず死んだまま。
- **矛盾:** k8s-agent は起動時に「sidecarS3SecretName 未設定 → cache は no-op、artifact は明示的に失敗」という degraded モードを警告付きでサポートすると宣言している(cmd/k8s-agent/main.go、docs/kubernetes-integration.md)。実際には:
  - artifact ステップ: 死んだコンテナへの exec が「container not running」で失敗(意図した「明示的な失敗」よりも不可解なエラー)
  - cache ステップ: exec 結果が破棄される(`internal/k8sagent/agent.go:346` の `_, _ =`)ため **Succeeded と偽装される**
  - さらに `WaitForPodRunning` は Pod の phase しか見ない(`podmanager.go:89-91`)ためサイドカー死亡を検知しない
- **副次(効率):** S3 store 構築には `BucketExists`(+`MakeBucket`)のネットワーク往復が含まれ(`internal/objectstore/s3.go:39-44`)、S3 設定済みでも全 Pod 起動のクリティカルパスに同期 S3 往復が乗る。また cache/artifact 転送は毎回新しい `unified-sidecar` プロセスを exec するため、転送1回ごとに bucket-ensure 往復が発生する。
- **修正案:**
  - `main()` で args を見て `idle` の場合は store を構築せず即 `run()` へ(store は cache/artifact サブコマンド内で遅延構築)
  - S3 env 不足時の cache/artifact サブコマンドは「明示的なエラーメッセージ + 非ゼロ exit」で degraded モードの宣言どおりに
  - 転送時の bucket-ensure をスキップする `NewS3ObjectStore` バリアント(バケットは事前プロビジョニング前提)を検討
  - 回帰テスト: 「S3 env なしで `unified-sidecar idle` が exit 0 で常駐する」ことの単体テスト(現状 run() の nil-store 分岐はテストからしか到達できず、本番経路と乖離している)

### 検証環境メモ(k8s)

- Docker Desktop 29.6.1 の Kubernetes(kind モード、k8s 1.35)は **WSL2 が cgroup v1 のままだと kubelet が起動せず** `failed to start` になる。`.wslconfig` に `kernelCommandLine = cgroup_no_v1=all systemd.unified_cgroup_hierarchy=1` を追加し `wsl --shutdown` で解消(このマシンで実証)。unified-cd の問題ではないが、開発環境要件として docs/kubernetes-integration.md に注記の価値あり。

### 17. [IMPLEMENTED] `kind: Schedule` が CLI から apply できない

- **状態: 実装済み**(apply の kind ディスパッチに Schedule を追加 — commit `e146d29`。`schedule list` / `schedule delete` サブコマンドも `internal/cli/schedule.go` に存在。branch `feature/cli-gaps` での確認時点で対応済みであることを確認し、本エントリを更新)。
- 以下は原記載:

- **症状(実機確認):** `unified-cli apply -f schedule.yaml` が「field cron not found in type dsl.Spec」で失敗。CLI の apply の kind ディスパッチ(`internal/cli/apply.go:88-147`)は GitCredential / WebhookReceiver / AppSource のみ対応で、**Schedule だけ Job として送信される**。`schedule` サブコマンドも存在しない。
- docs/resources.md と README は Schedule を apply 可能なリソースとして記載。API(`POST /api/v1/schedules`)直叩きなら作成でき、cron 発火自体は正常動作(毎分スケジュールで `schedule:verify-cron` のランが起きるのを確認)。
- **修正案:** apply.go の kind スイッチに Schedule を追加(+`schedule list/delete` サブコマンド検討)。

### 18. `artifact download` がデフォルト宛先(`.`)だと必ず失敗する

- **症状(実機確認):** `unified-cli artifact download <run> <name>` が `invalid path "./out.txt" in artifact archive` で失敗。`--dest <dir>` を明示すれば成功。
- **原因:** `internal/artifact/targz.go:36-38` の zip-slip ガード。`filepath.Join(dest, name)` はパスを正規化するため、dest=`.` のとき target から `./` が消え(`out.txt`)、`HasPrefix("out.txt\", ".\")` が常に false になる。tar エントリが `./` 始まり(k8s サイドカーの `tar -C dir .` 形式)のアーカイブ+相対 dest の組み合わせで全滅。同じパターンが `internal/cache/cache.go:196-198` にもある(cache は絶対パス dest で呼ばれるため今は顕在化せず)。
- **修正案:** 比較前に dest も target も `filepath.Abs`/`Clean` で正規化する定石実装に。デフォルト dest のテスト追加。

### 19. `call:` ステップの自己デッドロック(同一エージェントスロット待ち)

- **症状(実機確認):** `call:` で同じ agentSelector のジョブを呼ぶと、親ランがエージェントの唯一のスロット(max-concurrent=1)を占有したまま子ランを待ち、**子は永久に Queued、親は永久に Running**。タイムアウトも警告もなし。親をキャンセルすると finally は実行され、解放されたスロットで子は完走する。
- **修正案:** 最低限 docs/jobs.md の call: セクションに「子が同じエージェントプールを要する場合 max-concurrent≥2 が必要」と明記。可能なら call 待機中はスロットを解放する設計(または循環検出)を検討。
- 補足: キャンセルされた call ステップの表示が `Failed (exit 0)` になる(Cancelled が妥当)。

### 20b. CLI の設定優先順位が逆(config ファイル > 環境変数)

- **症状(実機確認):** `unified-cli login` が `~/.config/unified-cd/config.yaml` に server/token を書いた後は、`UNIFIED_SERVER`/`UNIFIED_TOKEN` 環境変数が**黙って無視される**。HA 検証中、env で 18080 を指定したのに config の 8080 に接続し、別サーバーにジョブを apply してしまった。
- **原因:** `internal/cli/root.go:34,40` — env は「config の値が空のときのみ」採用。慣例(flag > env > config ファイル)と逆。
- **修正案:** 優先順位を flag > env > config に変更。少なくとも docs/cli.md に現仕様を明記。

### 20. SSO セッションでハッシュルートに直接アクセスすると本文が空になる

- **症状(実機確認、Playwright):** OIDC SSO でログインしたセッションで `http://localhost:8080/ui/#/jobs` をハードナビゲーション(ディープリンク/リロード)すると、ヘッダー(ユーザー名等)は描画されるが**本文が完全に空**。失敗リクエストもコンソールエラーもゼロのまま沈黙。ルート `/ui/` からの遷移は正常。トークン認証セッションでは同じ操作が正常に描画される。
- **修正案:** SSO セッション初期化とルーター描画の順序を調査(セッション確認完了前にルートガードが空を返している可能性)。

---

## TODO 対応レビューで発見(2026-07-04、コードレビュー CONFIRMED)

### 22. Windows: Job Object ハンドルが正常終了ステップで毎回リークする(#9c 修正の退行)

- **症状:** #9c 修正で導入されたプロセスツリー kill 用の Job Object(`internal/agent/exec_windows.go:67-69` の `jobHandles[cmd] = job`)は、解放(CloseHandle + マップ delete)が **`killTree`(キャンセル経路)にしかない**。正常終了パス(`internal/agent/exec_tree.go:44-45` の `case err := <-waitDone`)は後始末をしないため、**普通に完走したステップ1つにつきカーネルハンドル1個+マップエントリ1個が永久にリーク**する。
- **影響:** 常駐 Windows エージェントで数千ステップ実行するとハンドル/メモリが無制限に増加(マップキーが `*exec.Cmd` のためコマンドの出力バッファごと GC 不能に固定)。キャンセルは例外系、リークは正常系=毎回。
- **付随(同 #9c 修正の隙):** ①`cmd.Start()` から `AssignProcessToJobObject` までの間に shell が fork した孫プロセスは Job 外で killTree を生き延びる。②`assignJob` の失敗は `_ =` で黙殺され、直接の子しか kill できないモードに静かに落ちる(`exec_tree.go:29-36`)。
- **修正案:** 正常終了パスにも共通クリーンアップ(マップ delete + CloseHandle)を追加。可能なら Job Object への割当てを開始前に行う設計(または assignJob 失敗を警告ログに)。回帰テスト: 正常終了後に `jobHandles` が空であること。

### 23. エージェントのラベルが一度登録されると二度と削除できない(#12 修正の退行)

- **症状:** #12 修正で claim 時 UPSERT が導入された際、ラベルが上書きから **DISTINCT 和集合マージ**に変更された(`internal/store/postgres.go:668` の `labels || EXCLUDED.labels`)。このマージが起動時のフル登録(`internal/controller/api_agent.go:47`)にも適用されるため、**エージェント設定からラベルを外して再起動しても DB から永久に消えない**(手動 SQL でしか削除不能)。
- **影響範囲(検証済み):** ルーティングは無事 — `ClaimNextRun`(`postgres.go:494`)は DB のラベルでなく claim クエリのラベルで照合するため、ジョブが誤ったエージェントに流れることはない。実害は `agent list` / UI Agents ページのインベントリ表示が恒久的に嘘をつくこと(gpu:true を外した機体が gpu 持ちとして表示され続ける等、棚卸し・監査に影響)。
- **修正案:** 「登録」(起動時、ラベル**置換**)と「生存確認」(claim 時、マージ or `last_seen_at` のみ更新)で SQL を分離。回帰テスト: ラベルを減らして再登録 → 減った状態が DB に反映されること(現行の `TestAgentAPI_Claim_DoesNotClobberRegisteredAgent` はラベル削除ケースを覆っていない)。

---

## コミットログ回帰レビューで発見(2026-07-05、7/3〜7/5の全変更を9領域で並列レビュー)

3日間(7/3〜7/5)に入った機能(RBAC+SSO / 監査ログ / matrix ステップ / runsIn 実行コンテキスト / AppSource multi-kind / ジョブ階層ツリー / アーティファクト・サイドカー / heartbeat+reaper / テンプレート群)がデグレを起こしていないか、テストが正しいかをコミットログ起点でレビュー。`go build ./...` / `go vet ./...` は通過。Go テストは e2e 1件、Web テストは 5件が失敗しているが**いずれもテスト側が古い**(下記 #34)。以下はテストで捕捉できていない実問題。

> **状態: #25〜#36 実装済み(2026-07-05)。** 上記レビューで見つかった問題を全件修正。検証: `go build ./...` / `go vet ./...` クリーン、`go test ./...` 全16パッケージ ok(e2e 含む)、web `npm test` 44/44 パス。follow-up(#25 の store 層リネーム、#33 の sync-stuck reaper、CLI の残り `http.NewRequest` 修正、docker-compose のローカルビルドフォールバック)も対応済み。詳細は各項目末尾の「実装」注記を参照。

### 25. AppSource の prune がアップグレード後の初回同期で既存ジョブを削除/孤児化(データロス)

- **分類:** 致命的(Critical)。2エージェントが独立に確認、再現手順まで特定。
- **症状:** サブディレクトリ配下のジョブ(例 `jobs/team-a/build.yaml`)は従来ベア名 `build` で保存・`managed_resources` に記録されていたが、commit `51ce318` で保存キーが qualified name `team-a/build` に変更された。移行バックフィル(`003_appsource_managed_resources.up.sql`)は**旧ベア名のまま**記録するため、アップグレード後の初回同期で prune ループが「desired=`team-a/build` に無い prev=`build`」と誤判定し、**Git に存在し続けているジョブを削除**する(`appsource_reconciler.go:130-170`)。
- **影響:** `prune:true` → 旧ジョブ削除+新名義で再作成され、run 履歴・UI ブックマーク・**旧名を参照する Schedule/WebhookReceiver が黙って壊れる**(参照整合性チェックなし、発火時 404)。`prune:false` → 旧ジョブが**永久に孤児**として残り Schedule が孤児を叩き続ける。**`spec.path` 直下のフラット構成は影響なし**(qualify で名前不変)。サブディレクトリ構成のみ発症。
- **原因コミット:** `51ce318`(dir を qualified name 化)+ `8672417`(managed_resources 移行)+ `e3f8474`(per-kind prune)。すべて 7/5 同日。
- **修正案:** ①移行 SQL のバックフィルで Job の name に `dir/` を前置して qualify する、または ②prune 比較前に prev 側も正規化する。移行前 managed_resources をシードした状態でのアップグレード同期の回帰テストを追加(現行 `TestReconciler_DirectoryBecomesQualifiedName` は新規 AppSource しか見ていない)。

### 26. アーティファクト名のパストラバーサルでクロスラン書き込み(実 PoC 確認)

- **分類:** 重要(High、セキュリティ)。実際に動く PoC で確認済み。一部は既存(`51b148d`)だが `026bc78` の sidecar CLI 配線で YAML から到達可能になった。
- **症状:** `artifactKey`(`internal/artifact/store.go:77`)が `name` を無害化せず `fmt.Sprintf` するだけ。`uploadArtifact.name` は非空チェックのみ(`dsl/parse.go:332`)で `/` や `..` を許容。ローカルオブジェクトストア(S3 未設定時のデフォルト)で `name="../victim-run/pwned"` が**別ランのアーティファクト名前空間へ書き込み**、`cache/` 名前空間への脱出も再現。ジョブを apply/trigger できる権限(developer 以上)で発動可能。
- **補足:** `cache` パッケージは同じキーを SHA-256 ハッシュ化して安全(`cache.go:34`)。アーティファクトだけ同処理が抜けている。S3 バックエンドはキーがフラット文字列のため `..` 解決は起きにくいが前段ゲートウェイ依存。トラバーサルを含む名前のテストは皆無。
- **修正案:** `artifactKey`(および HTTP ルート `api_artifacts.go:25,47,79`)で name を検証/ハッシュ化。`/`・`..` を含む name の拒否テスト追加。

### 27. heartbeat が claimCtx に紐づき、ドレイン中の正常ランを reaper が失敗させる

- **分類:** 重要(High)。ホストエージェントのみ。
- **症状:** heartbeat goroutine が `claimCtx`(SIGTERM/cordon で即キャンセル、`agent.go:135`)に紐づく。一方、実行中ステップは `DrainTimeout` まで生き残る別コンテキストで継続。ローリングデプロイ/ノード cordon でドレイン中、heartbeat だけ即停止 → 約120秒後(staleAfter 90s + reaper tick 30s)に reaper が**まだ生きて正常にドレイン中のランを "agent lost" で Failed 化**。まさにこの機能が防ぐはずの誤検知をドレイン経路で誘発。
- **原因コミット:** `edee77c`。`c0db89f` でも未対処。`TestAgent_DrainTimeout` はドレイン中に heartbeat が継続することを検証していない。
- **修正案:** heartbeat を run 実行が生きている間は継続する `ctx`(runCtx 相当)に紐づける。ドレイン中 heartbeat 継続の回帰テスト追加。

### 28. Pod GC が依存する controller の 404 が、一時的な DB エラーと区別されない

- **分類:** 重要(High)。GC ロジック自体は正しいが依存先が不正確。一部既存(`51b148d`)。
- **症状:** `handleGetRun`(`api_runs.go:63`)が `GetRun` の**あらゆるエラー(DB 接続断・タイムアウト含む)を 404 にマップ**(他 9 メソッドは `pgx.ErrNoRows` を区別しているのにここだけ非対応)。k8s-agent の GC は 404 を「ラン消滅」と判定するため、DB の一瞬の不調中に**まだ稼働中の Pod を削除**しランを失敗させ得る(pod-per-run は再開しない)。
- **修正案:** `GetRun`/`handleGetRun` で `pgx.ErrNoRows` のみ 404、他は 500 に区別。GC を HTTP/DB 実往復で検証するテスト追加(現行 `podgc_test.go` は `getRun` の返り値を直接フェイク)。

### 29. Store squash が旧 DB を取り残し、新カラムが永久に欠落(検知不能)

- **分類:** 中(Medium)。commit `79c1074` のメッセージで意図的破壊と明記済みだが、デプロイ時に無音で危険。
- **症状:** 001-017 を単一 `001_init` に squash した結果、旧チェーンで version 17 まで進んだ DB は新チェーン(最大 version 6)より上にいるため `m.Up()` が `ErrNoChange` を返す。**マイグレーション時はエラーも出ないのに** `role`/`managed_resources`/`audit_logs`/matrix `variant`/`sync_status` が適用されず、実行時に「column does not exist」で落ちる。
- **修正案:** 既存デプロイ向けの移行手順を docs 化(新規 DB 前提を明記)、または旧 version からの橋渡しマイグレーションを提供。

### 30. テンプレートのシェル引数クオート漏れ(2エージェントが独立指摘)

- **分類:** 重要(High)。`ec4272e` が helm/s3 等で修正したのと**同じバグクラスが同日追加の他テンプレートに残存**。
- **症状(High):** `templates/docker-build-push.yaml`(`$_TAG_ARGS`/`$_BUILD_ARG_ARGS`/`$_PUSH_ARGS` を非クオート展開、`237771d`)、`templates/unity-build.yaml`(`$_LICENSE_ARGS`/`$EXTRA_ARGS`、`3e2443f`)。`build_args: ["GREETING=hello world"]` のような空白/グロブ値が単語分割・グロブ展開され、ビルド失敗や引数注入(license 秘密の破損含む)。
- **症状(Medium):** `rsync-deploy.yaml`(`$EXTRA_ARGS`/`port` を経由した `--rsh=`/ProxyCommand 注入、`7ef094f`)、`notify-email.yaml`(`$_RCPT_ARGS` 非クオート+`SUBJECT`/`MAIL_TO` 経由の SMTP ヘッダ注入、`0e29ef3`)、`github-release.yaml`(`draft`/`prerelease` を JSON へ生挿入、アセット名の未 URL エンコード、`237771d`)、`gitlab-commit-status.yaml`(クエリ文字列未エンコード、`0e29ef3`)。
- **良い点:** `ab25e83` の semver ガードは POSIX `case` 実装で anchoring 問題なし(正しい)。params は実 env 変数経由で渡るため `{{ .Params }}` のシェル直挿入によるテンプレート注入クラスは存在しない。
- **修正案:** `ec4272e` と同じ `set -- "$@" ...` + クオート展開パターンを各テンプレートに適用。JSON は `_json_escape` 経由に統一。URL はエンコード。

### 31. matrix の組み合わせ上限が全直積をメモリ構築した後にチェック(OOM リスク)

- **分類:** 中(Medium、堅牢性/DoS)。`internal/dsl/matrix.go:30-64`、commit `2e3c10d`。
- **症状:** `UNIFIED_MATRIX_MAX_COMBINATIONS` の上限判定(`len(filtered) > maxCombos`)が**全カルテシアン積を `[]MatrixCombo` に構築した後**かつ exclude フィルタ後にしか行われない。5次元×各100要素のような定義は 10^8〜10^10 要素を確保してから初めて上限を見るため、エラーを返す前にエージェントが OOM/GC スラッシング。標準・k8s 両エージェントが同経路。
- **修正案:** 次元ループ内で走行中の積サイズを逐次チェックし、上限超過で早期エラー。exclude 前の raw 積にも上限を効かせる。

### 32. cache.path テンプレートの失敗時挙動が両エージェントで不一致

- **分類:** 中(Medium、運用性)。commit `17f1e4e`。
- **症状:** cache.path のテンプレート展開が空/不正なとき、**標準エージェントはステップを失敗させる**(`agent.go:768`)が、**k8s エージェントは黙ってキャッシュ操作をスキップしてステップ成功扱い**(`slog.Warn` のみ、UI/run report 不可視、`k8sagent/agent.go:328`)。同じ YAML がエージェント次第で挙動が変わる。タイポ(`{{ .Params.workingdir }}` vs 宣言 `working_dir`)が k8s では永久に無音で劣化。
- **付随(Medium/Low):** ①`resolveParams`(`params.go:27`)が空文字パラメータで `default:` をバイパス。②標準エージェントの cache.path はワークスペースでなくエージェントプロセス CWD 基準で解決(アーティファクトは workspace 基準なのに不一致)。③k8s の `path.Join(mountPath, expandedPath)` は `../` を confine しない。
- **修正案:** k8s も展開失敗をステップ失敗に統一。cache.path をワークスペース基準で解決。

### 33. その他の Medium(単独では小さいが要記録)

- **deep-link after SSO(`c1d8f1a`)はコミット名ほどのことをしていない:** 実際に直したのは authReady のレンダリング競合(`auth_oidc.go` は未変更)。SSO 往復でハッシュ経路 `#/runs/xyz` は失われ、ログイン後はデフォルト Jobs 画面に着地したまま。オープンリダイレクトは無し(RedirectTo は攻撃者非制御)。
- **AppSource sync が "Syncing" のままスタックし得る:** reconciler の panic / リーダー交代で終端書き込みに至らないと復帰しない。ラン向けの stuck-run reaper 相当が app_sources には無い(`api_appsources.go:79`)。Web ポーリングは 60s でタイムアウト表示するが、保存された status は誤ったまま残る。
- **`last_error` に認証情報入り `repoURL` が無編集で露出:** `spec.repoURL` に `https://user:secret@host/...` を許容(`Validate` 未拒否)。git 失敗の stderr が `SetAppSourceSyncStatus(..., err.Error())` 経由で API(`lastError`)と UI(`title={s.lastError}`)に露出。`34c509c` で表示経路が新設されたため露出面が拡大。
- **遅延した finish/step 報告がサイレントに成功扱い:** `MarkRunFinished` の CAS が `RowsAffected()` を見ず、reaper で Failed 化済みのランへの遅延 finish 報告に 204 を返す(`api_agent.go:355`)。step 報告も親ラン終端との整合チェック無し(終端ラン配下に stale な step 状態が書ける)。
- **Docker quickstart compose のヘルスチェック(要現物確認、判断が割れた):** `da600c7` が controller に `wget` ヘルスチェックを追加したが、controller は ghcr `:latest` を pull する。「公開済み `:latest` が distroless(wget 無し)なら永久 unhealthy → agent 起動せず」との指摘と「Dockerfile は alpine で busybox wget 同梱」との反論があり、**公開済みイメージが現行 Dockerfile と一致しているか**の確認が必要。

### 34. 失敗している自動テスト2件は「テストが古い」— プロダクトは正しい

- **`TestPhase8_FullOIDCFlow`(Go e2e)が 403:** 7/4 の RBAC 導入で「ロール未割当のログインは 403 拒否(secure-by-default)」が仕様化(`auth_oidc.go:221`)。テストの `OIDCConfig` に `DefaultRole` もロールクレームも無いため意図通り拒否される。**修正:** テストに `DefaultRole` 追加、またはモックトークンに `groups` クレーム+`RoleMap` を付与。
- **`web/src/components/AuthSetup.test.js` 5件失敗:** ①7/4 の英語化(`723ca9b`)は `App.svelte` を変更したのにテストは `AuthSetup.svelte` に日本語文言(`🔒 SSOでログイン`)を期待。②ログアウト/メール表示はそもそも `App.svelte` 側にあり、`AuthSetup` は `{#if !$currentUser}` でログイン後は何も描画しない設計。**テストが対象コンポーネントを間違えている。** 修正はテスト側。

### 35. テストの正しさ・網羅性のギャップ

- **タウトロジーなテスト:** `internal/agent/heartbeat_test.go:33` の条件 `!= got && < got` は `hits` が単調増加のため**永久に成立せず**、キャンセル後に heartbeat が止まる保証を何も検証していない(`edee77c`)。
- **目玉機能の分岐が未テスト:** ①runsIn の image/container ディスパッチ分岐が両エージェントとも未カバー(リーフ関数のみ、条件反転しても全テスト通過)②AppSource の再キー/prune/孤児化の移行パス(#25)③matrix の `GetStepOutputs` が variant を last-wins で潰す件(現状本番呼び出し元なしだが将来の地雷、`postgres.go:901`)④アーティファクト名トラバーサル(#26)⑤人向けアーティファクトダウンロードの非エージェント認証パス。
- **CLI テストが HTTP メソッド未検証:** `captureTransport` が path/body のみ記録し GET/POST 取り違えを検知不能。appsource 系・一部 run 系に非 2xx テストも欠落。
- **`http.NewRequest` のエラー握り潰し:** 新 CLI コマンド全般で `req, _ :=` により不正 URL 時に `Do(nil)` で nil パニック(`jobs.go:40` 他)。
- **参考(正しく機能しているもの):** SSE 単一初期化回帰テスト(`66faecf`)は本物、matrix のデータ競合修正は CI の `-race` で検証済み、Windows Job Object のプロセスツリー kill、store 共有 Postgres コンテナの分離、RBAC の拒否パス・per-route ロール・監査ログ(シークレット非記録・apply/delete 発火)、PAT ロール clamp、AppSource dedupe(決定的・ログ出力)などは実挙動を検証できている。
- **実装:** heartbeat タウトロジーを実 assert に書き換え、CLI の `captureTransport` にメソッド記録+検証追加、全 CLI コマンドの `http.NewRequest` エラー握り潰しを修正。runsIn ディスパッチ/matrix GetStepOutputs/人向けDL認証の網羅は follow-up 候補として残置。

### 36. 親ジョブのキャンセルが子ジョブ(call: ステップ)に伝播しない

- **分類:** 重要(High)。実機報告(親ランをキャンセルしても子ランが走り続ける)から発見。
- **症状:** `call:` ステップの子ランは親とは独立した通常ランとして起動され、親はポーリング監視するだけ。親子関係が永続化されておらず(`TriggerRunRequest`/`Run` に親ランID無し)、キャンセルが両側で伝播しない。①`handleCancelRun`(`api_runs.go`)は対象ラン1件だけを Cancelled にする ②`executeCallStep`(`agent.go`)は親の ctx キャンセル時に「child run may be orphaned」とログするだけで子をキャンセルしない。結果、子ランは完走(または30分タイムアウト)まで走り続ける。既存 #19(call デッドロック)とは別のギャップ。
- **実装(案1+2 両方):**
  - マイグレーション `007_run_parent.{up,down}.sql`: `runs.parent_run_id`(nullable, `REFERENCES runs(id) ON DELETE SET NULL`)+ 子検索インデックス追加。
  - store: `SetRunParent` / `ListChildRunIDs` を追加(`postgres.go` / `store.go` インターフェース)。
  - API: `TriggerRunRequest.ParentRunID` 追加。
  - **案2(コントローラ側カスケード)**: `handleTriggerRun` が親IDを保存、`handleCancelRun` が `cancelDescendantRuns` で子孫ランを BFS(cycle ガード付き)で再帰的に Cancelled 化。親エージェント死亡時も効く。実行中の子は、直接キャンセルと同じ経路で担当エージェントが Cancelled ステータスを検知して停止。
  - **案1(エージェント側カスケード)**: `client.CancelRun` を新設、`executeCallStep` が親 ctx キャンセル時にログではなく子ランへ実際にキャンセルを POST(detached context)。子作成時に親ID(`c.RunID`)を渡す。
  - テスト: `TestPostgres_RunParentLinkage`、`TestAPI_CancelRun_CascadesToChildRuns`(親→子→孫の再帰カスケード)、`TestClient_CreateRun`(parentRunId 送信)、`TestClient_CancelRun` を追加。

---

## agent / k8s-agent 実装パリティ監査(2026-07-06)

標準エージェント(`internal/agent`)と k8s-agent(`internal/k8sagent`)は同じステップ DSL を独立実装しており、片側にしか無い機能が黙って発生する。全 20 観点の突き合わせ監査で発見した未文書ギャップ。**恒久対策(共通オーケストレータ化・パリティ適合テスト)は #44 参照。**

### 37. k8s: `{{ .Secrets.X }}` が一切解決されない(Critical)— **対応済み(2026-07-06, b5521aa)**

- ホストは claim 後に `FetchSecrets`(`internal/agent/agent.go:340`)。k8sagent は `ClaimResponse.SecretsNeeded` を参照すらせず、`tplData.Secrets` が常に空。secrets を使うジョブが k8s に載るとテンプレートが**黙って空文字**に展開される。出力テンプレート用 `outCtx` にも `Secrets` フィールド自体が無い。

### 38. k8s: ログのシークレットマスキング無し(Critical)— **対応済み(2026-07-06, b5521aa)**

- ホストは全 LogPusher に `SetMasker`。k8sagent は stderr 用 LogPusher に `SetMasker` を呼ばず、stdout 用 `logLineWriter`(`internal/k8sagent/agent.go:792` 付近)にはマスカーのフィールド自体が無い。#37 を直すと同時に必須(直さないとシークレットが平文でログに載る)。

### 39. k8s: `call:` ステップが黙って成功する no-op(Critical)— **対応済み(2026-07-06, 6bd7a1a feat-k8s-call)**

- k8sagent に `step.Call` の分岐が無く、空スクリプト実行で **Succeeded** を報告。別作業(branch `feat-k8s-call`, worktree `../unified-cd-k8s-call`)が対応中のため本監査からは着手しない。

### 40. k8s: 通常ステップで `env:` と `UNIFIED_AGENT_OS` が消える(High)— **対応済み(2026-07-06, 9077735)**

- メイン Pod exec 経路は `stepForExec.Env` を計算する(`agent.go:633` 付近)のに `ExecStep`(`executor.go:53-63`, `PodExecOptions` に Env 無し)へ渡さない。最頻出のステップ形状でユーザー定義 env が黙って落ち、`$UNIFIED_AGENT_OS` も未設定。`runsIn.image` / scope Pod は Pod 生成時に注入されるので無事。docs/jobs.md は `env:` を全エージェント共通と記載しており矛盾。

### 41. k8s: `timeoutMinutes` が未執行(High)— **対応済み(2026-07-06, 1488de8)**

- ジョブレベル: `c.TimeoutMinutes` の参照ゼロ(ホストは `agent.go:285-289` で WithTimeout)。ステップレベル: 通常/scope exec に WithTimeout 無し(ホストは `agent.go:464-467`)。`runsIn.image` のみ Pod の `ActiveDeadlineSeconds` で近似あり。暴走ステップは手動キャンセルまで走り続ける。

### 42. k8s: `post:` フックが一切実行されない(High)— **対応済み(2026-07-06, 06b7eef)**

- `step.Post` の参照ゼロ(ホストは hookStack LIFO、`agent.go:684-694, 733-753`)。クリーンアップ処理が黙ってスキップされる。

### 43. k8s: stderr が step 終了まで UI に出ないことがある(Medium)— **対応済み(2026-07-06, b5521aa)**

- stdout は `logLineWriter` の行単位即時送信で無事。stderr は LogPusher を step 終端でしか Flush せず、`StartAutoFlush`(ホストは 2026-07-06 に導入、2 秒間隔)相当が無い。まばらな stderr 出力は step 終了まで滞留する。

### 44. 恒久対策: 実行オーケストレーションの共通化 + パリティ適合テスト(Feature)— **段階1・文書修正・段階2 すべて対応済み(2026-07-07)**

- **段階1 実装済み(aa6841b)**: パリティ適合テストスイート — `internal/paritycases`(共有ケース定義+期待値+Assert)+ 両エージェントのドライバ(`internal/agent/parity_host_test.go` が実 executeRun、`internal/k8sagent/parity_k8s_test.go` が実 orchestrate をローカル bash 実行のフェイク executor で駆動)。10ケース(if / env / continueOnError / finally / post LIFO / matrix / secrets+マスキング / step timeout / stdout outputs / call+子リンク)が両エージェントで同一期待値をパス。**新しい DSL 挙動は両ドライバに同じケースを足すこと**。付随発見(未対応・Low): `post:` フックの stdout/stderr は両エージェントともログ配管に載らない(host は RunStepCapture に nil、k8s は io.Discard)— 対称なのでパリティ違反ではないが、フック失敗の調査がログからできない。
- **文書修正 実装済み(fd5f25b)**: kubernetes-integration.md のパリティ主張を実差分の列挙に置換、jobs.md の cache 失敗時挙動と parallel: の k8s 逐次実行を明記。
- **段階2 対応済み(2026-07-07)**: `agentlib` への共有オーケストレータ抽出。`internal/agent/orchestrator.go` の `RunClaim(ctx, client, agentID, c, b ExecBackend)` が host/k8s 共通のステップ分岐・DSL 意味論・報告ループを一本化し、host/k8s それぞれの `executeRun` は `ExecBackend`(script 実行・scope 確保・cache/artifact 転送)を組み立てて `RunClaim` に渡すだけの薄いラッパーになった。k8s 側の旧 `orchestrate`(約650行)は完全削除。9タスクに分けて段階的に集約し、各段階で段階1の適合スイート(`internal/paritycases`)を回帰ゲートに使用。移行過程で以下の挙動差分が発見・意図的に統一された:
  - k8s の report(ステップ/FinishRun)リトライが host と同じ `RetryUntilSuccess` に統一(T2)。
  - cache の空 key / 空 path 展開時のスキップ挙動を host/k8s で統一(T3)。
  - matrix 展開の構造的エラーによる abort(残りのステップを打ち切る挙動)が k8s にも適用されるようになった(`RunPipeline` の stage-abort-on-error 契約を host/k8s 共通で採用、T5)。
  - `SetRunOutputs` のリトライと `cancelledByMaster` による Cancelled/Failed の判別ロジックを統一(T8)。
  - コミット範囲: `fbc48c9`〜本コミット(`feat-shared-orchestrator` ブランチ、Task 1〜9)。
  - その他、共有ループ化に伴い k8s は細部でも host 意味論に統一(cache 保存ドレインと post: フックの順序、job 出力昇格のタイミング、報告コンテキストの扱い、空展開スクリプトのフォールバック廃止等)。

---

## 機能要望(Feature)

### 24. 監査ログ(API 操作の記録)— 実装済み(branch `feature/audit-log`)

- **背景:** 現在監査証跡があるのは承認/却下(`run_approvals` テーブル、decidedBy 付き)のみ。シークレットの変更、Job の apply/削除、手動トリガー、トークン発行/削除、キャンセルなどの操作は誰がいつ実行したか記録が残らない。エンタープライズ用途・障害調査(「誰がこのジョブを書き換えたか」)で必須になる。
- **内容:** 状態を変更する API 操作(POST/PUT/DELETE 系)について、操作者(トークン/OIDC サブジェクト)・操作種別・対象リソース・タイムスタンプ・結果を記録する。シークレットは名前のみ記録し値は残さない。
- **実装案:** `audit_logs` テーブルを追加し、コントローラの認証済みハンドラ層(ミドルウェア)で一括記録。閲覧は admin ロール限定で `GET /api/v1/audit` + `unified-cli audit list`。保持期間は設定可能(デフォルト例: 90日)にし、リーダー限定タスクで期限切れを削除。
- **実装:** マイグレーション `internal/store/migrations/004_audit_logs.{up,down}.sql`。ミドルウェア `internal/controller/audit.go` を `/api/v1`(jobs/runs/secrets/gitcredentials/tokens)・`/api/v1/webhooks`・`/api/v1/schedules`・`/api/v1/appsources` の各ルートグループに追加(POST/PUT/DELETE のみ記録、GET・エージェント向け・webhook ingress・auth/OIDC は対象外)。`GET /api/v1/audit`(admin 限定、`internal/controller/api_audit.go`)、`unified-cli audit list --limit N`(`internal/cli/audit.go`)、保持期間クリーンアップ `internal/controller/audit_retention.go`(リーダー限定、`--audit-retention-days` / `UNIFIED_AUDIT_RETENTION_DAYS`、デフォルト90日、0=無期限)。詳細は [docs/audit.md](docs/audit.md)。

---

## 軽微(Low)— UI / CLI

- `favicon.ico` が 404(全ページロードでコンソールエラーが1件出る)
- ヘッダーの UI 言語が英語(Jobs / Agents / Resources / Tokens)なのに「ログアウト」ボタンだけ日本語。`index.html` も `lang="ja"`。言語を統一するか i18n 化する
- `unified-cli login` だけ `UNIFIED_SERVER` 環境変数を読まず `--server` フラグ必須(他コマンドと非一貫)
- AppSource のパスに Job 以外の kind(WebhookReceiver 等)が混在すると WARN でスキップされる(仕様どおりだが docs/resources.md に明記すると親切)。同期エラーがコントローラログでしか見えず、CLI/API から AppSource の同期状態を確認する手段がない

---

## 検証済みで正常だった機能(参考)

- apply → trigger → logs -f → run show のコアフロー
- 並列ステップ実行(ステージ順序の保証)
- シークレット: env 注入 + ログマスキング(`***`)
- Webhook: `auth: none` 受信、`filters:`(Go テンプレート)による選別、`paramsMapping` の解決
- mutex 同時実行制御(2本目が Pending で待機 → 順次実行)
- エラー系: 不正トークン(unauthorized)、存在しないジョブ、不正 YAML(フィールド名まで明示)
- Schedule: cron 発火(API 経由で作成、毎分スケジュールが分境界で正しくランを起動、triggeredBy=schedule:名前)※CLI からの作成は #17
- Webhook HMAC 認証: 正しい署名=200+ラン起動 / 不正署名・署名なし=401(X-Signature / X-Hub-Signature-256 両対応)
- cache: ステップ: Garage(S3)への保存→ワークスペース消去→別ランでの復元(CACHE_HIT)
- finally: 成功時の常時実行、**キャンセル時の実行**も docs 仕様どおり
- call: 子ランの起動と完走(※同一プール単一スロットではデッドロック — #19)
- PAT: `token create` → PAT で認証 → `token delete` → unauthorized のフルサイクル
- artifact CLI: `artifact list` / `artifact download --dest <dir>`(※デフォルト dest は #18)
- `uses:` git テンプレート: 公開 GitHub リポジトリの Job YAML を `git://github.com/...@main` で取得し、ステップを `tmpl__<name>` 形式で正しくインライン展開・実行
- AppSource(GitOps): 公開リポジトリの `examples/jobs` を約1分で同期し Job を自動登録(再帰スキャン)。壊れた YAML はファイル名+理由付き WARN でスキップし他は続行
- GitCredential: apply / list(※非公開リポジトリでの実フェッチはテスト用非公開リポジトリがなく未検証)
- OIDC SSO(Dex): Web UI の SSO ログイン → セッション確立 → API 呼び出し・ユーザー表示(※ディープリンクは #20)
- OIDC デバイスフロー: `login --server` → 検証 URL 表示 → ブラウザ承認 → トークン保存(期限付き)→ 保存トークンで API 認証成功
- HA(nginx LB + controller×3 + agent×2、test/ha 構成で検証):
  - LB 経由の apply / trigger / logs、2エージェントへの負荷分散
  - **リーダー kill 後 約20秒で別コントローラがリーダー継承**、新規トリガー・ログ取得とも正常続行
  - ※実行中エージェント kill → ランが Running のまま放置されるのを実測(45秒+)。セクション A(reaper 未実装)の分析どおり。死んだエージェントは agent list に残存
- Windows ネイティブエージェント(ホスト実機で検証):
  - `go build ./cmd/agent` → 起動・登録(os=windows、hostname ラベル自動付与)
  - `agentSelector: [kind:windows]` のルーティング(docker ジョブと相互に混線なし)
  - Git Bash(MINGW64)でのステップ実行、`UNIFIED_AGENT_OS=windows` の注入
  - ステップ間のワークスペース共有・ファイル受け渡し
  - ※キャンセル時の問題は上記 9c
- k8s-agent(docker-desktop クラスタ + ローカル k8s-agent プロセスで検証):
  - 登録(`kubernetes` ラベル自動付与)、`agentSelector: [kind:k8s]` ルーティング
  - Pod-per-run 生成(`ucd-run-<id>`)、runner コンテナ内でのステップ exec、ログストリーム
  - ラン完了後の Pod 自動削除
  - アーティファクト round-trip(※alpine サイドカー+絶対パス+ディレクトリ指定の条件下でのみ。制約は #13〜#16)
- Web UI(Playwright による実ブラウザ操作で検証):
  - Connection カードでのトークン入力 → 認証成功でカード消滅
  - Jobs 一覧(フィルタ付き)・ジョブ詳細(History / ▶ Run / YAML タブ)
  - ▶ Run タブからの UI トリガー(default プレフィル、required の `*` 表示と未入力時のボタン無効化)
  - Run Detail: ステップ毎のステータス・所要時間・exit code、SSE ライブログ、Rerun ボタン
  - Agents ページ(docker-agent-1 のラベル表示)

## 検証環境メモ

- ローカルの git-ignore された `vendor/` が古いと dev compose のコンテナ内ビルドが壊れる(`go mod vendor` で復旧)。開発ドキュメントに注記するか、compose 側で吸収を検討。
