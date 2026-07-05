# matrix ステップ設計(多次元マトリックス展開)

日付: 2026-07-05
ステータス: 承認済み(実装計画未着手)

## 目的

`foreach:`(1次元・文字列リストのみ)を多次元の `matrix:` に一般化する。OS×arch のようなクロスビルドを、シェルでの文字列分解なしに宣言的に書けるようにする。

## 決定事項(ユーザー承認済み)

| 論点 | 決定 |
|---|---|
| 出力参照 | **集約マップ**。matrixステップの出力は組み合わせキー→値のマップになる |
| v1スコープ | **exclude のみ**(include は対象外) |
| foreach の扱い | **シュガーとして存続**。パーサで1次元matrixに変換。`foreach` と `matrix` の同時指定は apply 時エラー |
| 互換性 | **バージョンゲートなし**。本番前のため、matrixリリース時にエージェント全台更新を必須とする(ドキュメントに明記) |

## DSL構文

```yaml
- name: build
  matrix:
    os: [linux, windows, darwin]
    arch: [amd64, arm64]
    exclude:
      - os: windows
        arch: arm64
  run: GOOS={{ .Matrix.os }} GOARCH={{ .Matrix.arch }} go build -o out/{{ .Matrix.os }}-{{ .Matrix.arch }}
```

- 各次元の値は既存の `ForeachSource` を再利用: リテラル配列 / `$param`(JSON配列パラメータ参照)/ テンプレート式。動的matrixを最初からサポートする
- `exclude:` は次元名→値のマップのリスト。指定された全キーが一致する組み合わせをデカルト積から除外する。部分指定(一部の次元のみ)の場合、一致する全組み合わせを除外(GitHub Actions と同じ)
- `exclude` のキーに存在しない次元名があれば apply 時エラー
- 予約語: 次元名に `exclude` は使えない(apply 時エラー)
- 組み合わせ数の上限: サーバ設定 `matrix-max-combinations`(デフォルト 64)。動的ソースがあるため検査は**展開時**(エージェント側)。超過はステップ失敗として扱う

## 展開セマンティクス

- 展開は現行 foreach と同じく**エージェント側**(`internal/agent/pipeline.go`)で行う: 各次元を `EvalForeachSource` で評価 → デカルト積 → exclude 適用 → 全組み合わせを並列実行
- 順序の正規形: **次元は宣言順、値はリスト順**。組み合わせキーは宣言順に値を `/` で結合した文字列(例: `linux/amd64`)
- テンプレート変数: `{{ .Matrix.<次元名> }}`。foreachシュガー経由の場合、`{{ .Foreach.<key> }}` も同じ値を返す(後方互換)
- **`parallel:` ブロック内のステップでも展開する**。現行の「parallel内foreachがバリデーションを通るのに実行時に展開されない」バグ(pipeline.go の parallel 分岐に展開処理がない)はこの変更で解消する
- 空の次元(評価結果が0要素)は組み合わせ0件 → ステップはスキップ扱い(実行されない)。ランは失敗しない

## 出力の集約

- 非matrixステップの出力: 従来どおり文字列(変更なし)
- matrixステップの出力: `{{ .Steps.build.Outputs.version }}` は**組み合わせキー→値のマップ**を返す(例: `{"linux/amd64": "1.2", "linux/arm64": "1.2"}`)
- CEL(`if:`)からは `steps.build.Outputs.version["linux/amd64"]` でアクセス可能
- 下流の `matrix` / `foreach` の `in:` 式にキー列・値列を渡してファンインできる(テンプレート関数 `keys` / `values` を提供する)
- 出力の保存: 既存の出力ストアのキーに組み合わせキーを付加する(ステップ名 + 組み合わせキー)。展開コピー間の衝突・最後勝ちは発生しない
- 注意: 次元の値に `/` が含まれると組み合わせキーが曖昧になるため、次元の値に `/` を含むmatrixステップの出力参照は未定義とせず、**値に `/` を含む場合は展開時エラー**とする(単純・安全側)

## ワイヤ形式(破壊的変更)

- `api.ClaimStep` の `ForeachKey string` / `ForeachValue string` を `MatrixValues map[string]string` に置換する
- foreachシュガーも同じ `MatrixValues`(1エントリ)に正規化される
- **後方互換なし**: 旧エージェントは matrix/foreach ステップを正しく実行できない。リリースノートと docs/agents.md に「コントローラ更新時はエージェントも同時更新必須」と明記する

## UI / 表示

- 展開されたステップの表示名: `build (linux, amd64)`(`ReportStep.StepName` に組み合わせを付加)
- `run show`(CLI)も同じ表示名を使う
- ログは展開コピーごとに分かれる(現行 foreach と同じ)

## バリデーション(apply 時)

- `foreach` と `matrix` の同時指定 → エラー(相互排他)
- `matrix` の次元が0個 → エラー
- 次元名の重複 / 予約語 `exclude` → エラー
- `exclude` 内の未知の次元名 → エラー
- 既存の `validateStepFull` に統合(parallel 内・finally 内も同経路で検証)

## テスト方針

- `internal/dsl`: デカルト積・exclude(完全一致/部分指定)・上限超過・同時指定エラー・foreach→matrix変換・値の `/` 拒否のユニットテスト
- `internal/agent`: pipeline 展開テスト(単独ステップ / **parallel 内** / 動的ソース / 空次元スキップ)
- 出力集約: 多重展開ステップの出力がマップに正しく入る結合テスト(コントローラ側ストア)
- k8s-agent: 標準エージェントと同じ展開経路を通ることの確認(orchestrate_test 拡張)

## 対象外(YAGNI)

- `include:`(組み合わせ注入・変数追加)— 必要になったら別仕様で
- 組み合わせごとの `agentSelector` 上書き
- matrixステップへの添字直接参照構文(集約マップで代替)
- 旧エージェントとのプロトコル互換レイヤ
