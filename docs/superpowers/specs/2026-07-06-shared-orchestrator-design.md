# 共有オーケストレータ抽出(TODO #44 段階2)設計

- 日付: 2026-07-06
- ステータス: 実装済み(2026-07-07)
- 前提: パリティ適合スイート(段階1, `internal/paritycases`)が回帰ゲートとして稼働済み

## 背景と動機

ホストエージェント(`internal/agent` `executeRun`+`pipeline.go`)と k8s エージェント(`internal/k8sagent` `executeRun`→`orchestrate`)はステップオーケストレーション(secrets 取得→キャンセルポーラ→ステージ実行→if/timeout/approval/cache/artifact/call/run 分岐→outputs→報告→post フック→finally→FinishRun)を**二重実装**しており、これが #37〜#43 のドリフトの根因。両ループの精密な突き合わせ(構造分析 2026-07-06)により、責務の大半が IDENTICAL または PARAMETERIZABLE であることを確認済み。

## ゴール

オーケストレーション本体を `internal/agent`(agentlib)の共有実装に一本化し、実行基盤だけを `ExecBackend` インターフェースとして host / k8s が実装する。以後、DSL 挙動の変更は共有ループ1箇所+両バックエンドの型実装に閉じ、ドリフトは型システムとパリティスイートの両方で構造的に防がれる。

## ExecBackend インターフェース(確定案)

```go
// package agent (agentlib)
type ExecBackend interface {
	// スクリプト実行(4分岐は共有ループ側が判定して呼び分ける)
	RunDefault(ctx, step, script string, env []string, stdout, stderr io.Writer) (int, error)
	RunImage(ctx, step, script string, env []string, stdout, stderr io.Writer) (int, error)
	RunNamedContainer(ctx, step, container, script string, env []string, stdout, stderr io.Writer) (int, error) // host は明示エラー
	// scope(uses-level runsIn.image)ライフサイクル
	EnsureScope(ctx, step, env []string) (ScopeHandle, error)
	RunInScope(ctx, h ScopeHandle, script string, env []string, stdout, stderr io.Writer) (int, error)
	CloseScopes(ctx)
	// cache / artifact 転送(host=objectstore 直、k8s=sidecar argv。中身は完全に不透明)
	CacheRestore(ctx, scope ScopeHandle, key string, restoreKeys []string, path string) (bool, error)
	CacheSave(ctx, scope ScopeHandle, key, path string, ttlDays int) error
	UploadArtifact(ctx, scope ScopeHandle, runID, name, path string) error
	DownloadArtifact(ctx, scope ScopeHandle, runID, name, destDir string) error
	// post: フック実行ルーティング
	RunPostHook(ctx, scope ScopeHandle, container, script string, env []string) error
	// ラン単位ライフサイクル(host=workspace dir、k8s=Pod 取得/解放)
	AcquireRun(ctx, c api.ClaimResponse) (release func(ctx), err error)
	// ステージメンバー(parallel/matrix)の実行モード
	ConcurrencyMode() ConcurrencyMode // host=Concurrent, k8s=Sequential(現状維持)
}
type ScopeHandle struct{ opaque any } // host=crt.ContainerHandle, k8s=pod 名。ゼロ値=非スコープ
```

- **4つの Run メソッドを分ける理由**: どれを呼ぶかの分岐(`isScopedStep`→`runsIn.container`→`runsIn.image`→default)自体が両実装で同一のオーケストレータロジックであり、共有ループに持ち上げる。
- **ログ配管**: 共有ループは stdout/stderr の `io.Writer` を組み立てて渡す。host は両ストリーム LogPusher(tee+AutoFlush)、k8s は stdout=行単位 logLineWriter / stderr=LogPusher という**現行の非対称を維持**(Writer ファクトリはバックエンド側)。AutoFlush 前提を共有ループに置かない。

## 統合時に確定する挙動判断(重要)

構造分析で発見した両者の意味論不一致は、統合前に明示的に解消する:

1. **cache の空 key/path**(潜在バグ・パリティスイート未カバー): host は空 path でステップ失敗+空 key 無検査、k8s は両方とも warn+スキップ(成功)。**k8s の lenient-skip に統一**(cache は best-effort という設計原則に整合)。先に paritycases へシナリオ追加(TDD)。
2. **報告系のリトライ**: host は ReportStep/FinishRun/SetRunOutputs を `retryUntilSuccess` で包むが、k8s は全て単発送信(エラー破棄)。**host の retry 方式に統一**(k8s の信頼性向上。意図的でない欠落と判断)。
3. **並行実行モード**: host=goroutine 並列 / k8s=逐次は**意図的差分として維持**(`ConcurrencyMode`)。k8s の並列化は scopePods/hookStack のロック設計が必要な**別 TODO**とし、本リファクタの副作用では行わない。
4. **runsIn.container**: host は「明示エラー」をバックエンド実装として維持(docs 記載済みの意図的非対称)。

## 移行順序(各ステップでパリティスイート+両フルスイート緑を維持)

| # | 内容 | 種別 |
|---|---|---|
| 1 | 共有 `applyStepOutputs` 等ヘルパー抽出(k8s の手書き map 操作を置換) | 無挙動変更 |
| 2 | k8s の報告系に `retryUntilSuccess` 導入(挙動判断2) | 挙動変更・単独 |
| 3 | cache 空 key/path の意味論統一(挙動判断1、paritycases シナリオ先行) | 挙動変更・単独 |
| 4 | `RunPipeline` に `ConcurrencyMode` パラメータ導入(host 既定 Concurrent、k8s 未接続) | 無挙動変更 |
| 5 | k8s のステージ/finally ループを `RunPipeline`(Sequential)へ移行 | 最大の機械的移動 |
| 6 | `ExecBackend`/`ScopeHandle` 定義+host 実装、host `executeRun` を共有ループ呼び出しに | 抽出前半 |
| 7 | k8s の `ExecBackend` 実装(8クロージャ引数の `orchestrate` シグネチャ廃止、orchestrate_*_test 群の書き換え) | 抽出後半 |
| 8 | 共有ループ本体(secrets→poller→stages→post→finally→outputs→FinishRun)を agentlib に確立し、両 `executeRun` を「backend 構築+shared.Run」へ縮退 | 完成 |
| 9 | 旧重複コード削除・docs 更新・TODO #44 クローズ | 後始末 |

## テスト影響(構造分析より)

- host: `executeRun(ctx, claim, workDir)` 直呼びテスト約25箇所 → シグネチャ維持(薄いラッパー化)で無変更を狙う。`executeCacheStep` 直呼び5箇所と `pipeline_test.go` 8箇所は追随変更。
- k8s: `a.orchestrate(8引数)` 直呼び約10箇所 → ステップ7でフェイク `ExecBackend` 構築へ全面書き換え(最工数)。`executeRun(ctx, claim)` 直呼び9箇所はシグネチャ維持。
- パリティドライバ2本は各ステップでロックステップ更新。

## スコープ外

- k8s の parallel/matrix 並列実行化(別 TODO、ロック設計要)
- ログ転送方式の統一(LogPusher vs logLineWriter — Writer ファクトリの背後に隠して現状維持)
- claim ループ/drain/ハートビート等のラン外側ライフサイクル(既に十分共有済み)
