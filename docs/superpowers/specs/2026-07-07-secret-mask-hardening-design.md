# Secret マスキング強化(複数行・順序・outputs ガード)設計

- 日付: 2026-07-07
- ステータス: 設計レビュー中

## 背景と動機

現行のマスキングは GHA と同型の「値ベース一致置換」(`internal/secrets/masker.go` + `LogPusher` の行単位マスク)だが、3つの具体的なギャップがある。

1. **複数行 secret が隠せない**: `NewMasker` は値全体を1パターンとして登録するが、マスクは行単位に適用されるため、改行を含む secret(PEM 秘密鍵等)は絶対に一致しない。`cat key.pem` で全文がログに残る。
2. **部分文字列 secret の順序問題**: パターンを登録順に `strings.ReplaceAll` するため、secret A が secret B の部分文字列(接頭辞等)だと A が先に置換されて B の残部が露出する。
3. **ログ以外の経路はノーマスク**: step outputs / call 子 outputs 伝播 / run outputs は生値のまま controller へ送られ DB に永続化される。`outputs: token=$SECRET` と書けば平文保存され、将来の outputs UI 表示でそのまま画面に出る。

## 変更内容

### 1. 複数行 secret の行分割登録 — `internal/secrets/masker.go`

`NewMasker` で、値に改行(`\n`)を含む場合:

- 現行どおり値全体(+Base64/URL エンコード版)を登録したうえで、
- 値を行分割(`\r` は trim)し、**trim 後 4 文字以上**の各行を追加パターンとして登録する(4 文字未満をスキップするのは base64 末尾の `==` 等による壊滅的な誤爆防止。定数 `minMaskLineLen = 4`)。
- 行パターンにはエンコード版は登録しない(行単位の一致で十分。GHA と同等)。

### 2. 長さ降順ソート — `internal/secrets/masker.go`

`NewMasker` の最後に patterns を**長さ降順**(同長は安定)でソートする。`Mask` は変更なし。これにより長い secret が先に置換され、接頭辞関係の取り残しが消える。

### 3. outputs 送信前の secret 検出ガード — `internal/secrets/masker.go` + `internal/agent/orchestrator.go`

- `Masker` に `Detects(s string) bool` を追加: 登録済みパターンのいずれかを `strings.Contains` で含むなら true(`NoOpMasker` は常に false)。
- orchestrator の outputs 送信 3 経路の直前で各 key/value を検査し、**secret を含む値の key を送信 map から除外**する:
  - ステップ outputs(orchestrator.go:402 付近の `SetStepOutputs`)
  - call 子 outputs の親への伝播(:318 付近の `SetStepOutputs`)
  - run outputs(:529 付近の `SetRunOutputs`)
- 除外した key ごとに警告を出す: agent の `slog.Warn` に加え、**run のログに1行**(`output "<key>" skipped: value may contain a secret` 形式)を出力し、UI から気づけるようにする。出力先はステップ outputs / call 伝播では該当ステップの log writer、run outputs(pipeline 終了後でステップの writer が無い)では System 行(`stepIndex = -1`)として送る。値そのものは警告に含めない。
- ガードは共有オーケストレータ(`RunClaim`)内に置くため host/k8s 両 agent で同一挙動。ステップ間の in-memory `steps` コンテキスト(`ApplyStepOutputs`)は**除外しない**(同一 run 内の後続ステップからの参照は GHA の step outputs と同様に許容。永続化・表示経路だけを塞ぐ)。

## スコープ外(記録)

- call ステップの params 経由で子 run に secret を渡すケース(黙って落とすと子の挙動が変わるため。将来は警告のみ等を検討)
- 実行中の動的マスク登録(GHA の `::add-mask::` 相当)
- artifacts / step summary 等その他の経路

## テスト

- masker 単体(`internal/secrets/masker_test.go`): 複数行 PEM 風の値が行単位でマスクされる、4 文字未満の行はパターン化されない、接頭辞ペア(A が B の接頭辞)で B が完全にマスクされる、`Detects` の真偽(含む/含まない/NoOp)。
- orchestrator ガード(`internal/agent/`): secret を含む outputs の key が `SetStepOutputs`/`SetRunOutputs` に渡らず、警告ログ行が run ログへ流れる。クリーンな key は素通り。実 bash 実行のパリティケース追加は任意(両バックエンドが同一経路を通るため単体で足りると判断したら省略可)。
