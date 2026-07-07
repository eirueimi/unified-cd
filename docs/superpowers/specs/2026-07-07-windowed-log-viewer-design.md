# ウィンドウド・ログビューア(truncate廃止)設計

- 日付: 2026-07-07
- ステータス: 設計レビュー中

## 背景と動機

WebUI のログビューは仮想スクロールで**描画**は解決済みだが、SSE バックフィルが末尾 10,000 行に truncate される(`sseBackfillLimit`)。理由は転送(数MB バースト)・メモリ(全行 JS 配列常駐)・O(n) 処理(フィルタ/検索/wrap オフセット)。その結果、巨大ログでは (a) 先頭側のステップが見えない(per-step 補充 `e424037` で緩和済み)、(b) ログ全体をスクロールで遡れない。

**ゴール**: クライアントは常にログの一部(ウィンドウ)だけを保持し、スクロール位置に応じてサーバーから範囲取得する。truncate という概念を廃止し、任意サイズのログを最初から最後までシームレスに閲覧できる。検索はサーバー側で全文に対して行う。

## サーバー側(store + controller)

logs テーブルは (run_id, seq) 索引済み・seq はグローバル連番(run 内では単調増加だが**非連続**)。スクロールバー位置→行の対応には「run 内での行番号(ROW_NUMBER)」を使う。

### 新エンドポイント(いずれも view ロール)

1. **`GET /runs/{id}/logs/stats?steps=0,2`**(steps 省略可)
   → `{ "count": 24686, "minSeq": ..., "maxSeq": ... }`。スクロールバーの全長と初期位置の計算用。store: `CountLogs(ctx, runID string, steps []int) (count int64, minSeq, maxSeq int64, err error)`。
2. **`GET /runs/{id}/logs/range?offset=N&limit=M&steps=0,2`**(steps 省略可、limit 上限 10,000)
   → 対象ビュー(全体 or 指定ステップ集合)の行番号順で offset から M 行(`api.LogLine` 配列)。store: `ListLogsRange(ctx, runID string, steps []int, offset, limit int)` — `ORDER BY seq OFFSET/LIMIT`(index scan、O(offset) は許容。将来必要なら keyset 併用)。steps は `step_index = ANY($2)`。
3. **`GET /runs/{id}/logs/search?q=...&steps=0,2`**
   → `{ "total": 37, "matches": [{ "row": 123, "seq": ..., "stepIndex": 2 }, ...] }`(matches は先頭 1,000 件で cap、cap 時は total で示す)。`row` は対象ビュー内の 0-based 行番号(ジャンプ先スクロール位置の計算用)。SQL: ROW_NUMBER ウィンドウ + `line ILIKE '%'||$q||'%'`(エスケープ必須)。run 単位のシーケンシャルスキャンは許容(将来 pg_trgm 索引を注記)。空 q は 400。

### 廃止

- `GET /runs/{id}/steps/{stepIndex}/logs` と `TailLogsRecentByStep`(本日 `e424037` で追加、未 push)は range+stats に包含されるため**削除**。テストは range/stats 側に移行。

### SSE は変更なし

バックフィル(末尾10k)+ライブ配信+`truncated` イベントは現行のまま。クライアント側での意味づけだけ変わる(下記)。

## クライアント側(RunDetail.svelte — 本丸)

### データモデル

`logLines`(全量配列)を**ビュー付きウィンドウ**に置換:

```js
// view: 現在の表示対象。all | steps(単一ステップ or parallel グループ)
// window: 対象ビューの連続区間 [startRow, startRow+lines.length)
let logView = { steps: null /* null=all | number[] */ };
let logWindow = { startRow: 0, lines: [], totalCount: 0 };
```

- **初期化**: SSE バックフィルをそのまま初期ウィンドウとして採用(`startRow = totalCount - lines.length`、totalCount は stats から。`truncated` イベントは「上に続きがある」の意味になり、バナーは廃止 — スクロールバー自体が全長を表すため)。
- **ライブ追記**: ウィンドウが末尾に接している(stick 状態)場合のみ lines に append+必要なら先頭 evict。非 stick 時は totalCount++ だけ行い、スクロールバーが伸びる。
- **スクロール**: 仮想スクローラは `totalCount * LOG_ROW_H` を全高とし、可視範囲の行番号がウィンドウ外に出たら `range` を取得してウィンドウを**置換**(チャンク: 要求行を中心に前後拡張、ウィンドウ上限 30,000 行 — 契約: ウィンドウは常に単一の連続区間)。取得中はスピナー行を表示。
- **ステップ/グループ選択**: ビュー切替として扱う。stats(steps)→末尾 range を取得して新ウィンドウ(今日入れた per-step 補充ロジックはこの一般形に**置換**)。選択解除で all ビューへ戻る(all のウィンドウはキャッシュしてよいが v1 は再取得で可)。
- **検索**: 入力デバウンス後 `search` を叩き、`total`/matches を保持。n/N ジャンプは match.row へスクロール(→range 取得が連鎖)。ハイライトは現行ロジック(ウィンドウ内の行に対して適用)。cap 超過時は「1,000+ 件」の表示。
- **wrap モード**: ウィンドウ内は現行の累積オフセット、ウィンドウ外の全高は `LOG_ROW_H` 近似(スクロール位置の多少のジャンプは許容し、range 取得後に再アンカー)。v1 の既知制限として明記。

### 既存挙動の維持

- tail 追従(stick)/上スクロールで解除/フィルタ変更時の末尾ジャンプ(`31f2f67`)は同一。
- Cancelled/終了 run の閲覧、SSE 再接続時のリセット(ウィンドウ・stats 再取得)。
- 空フィルタ文言(`e424037`)は「取得中(Loading…)/本当に 0 行(No log lines…)」として引き継ぐ。

## テスト

- store: CountLogs / ListLogsRange(offset・limit・steps 境界、複数 run 混在で他 run の行が混ざらない)/ SearchLogs(ILIKE エスケープ: `%` `_` を含むクエリ、cap)。実 PG。
- controller: 3 エンドポイントの HTTP テスト(steps パラメータ解析、limit 上限、空 q 400)。既存 steplogs テストの移行(削除エンドポイント分)。
- web(jsdom): (1) スクロールでウィンドウ外に出ると range が呼ばれ行が差し替わる、(2) ステップ選択がビュー切替として stats+range を呼ぶ、(3) 検索がサーバーを叩き n ジャンプで range 連鎖、(4) 非 stick 中のライブ行が totalCount のみ伸ばす、(5) 既存 19 テストの改修(全量配列前提のものをウィンドウ前提に)。スタブは既存の scrollTop/scrollHeight パターン。

## スコープ外

- pg_trgm による検索索引(必要になったら)
- アーカイブ(S3)からの閲覧(DB 行が消えない現状では不要)
- ログの正規表現検索・大文字小文字区別オプション
- CLI の変更(全量取得は従来どおり CLI の役割)
