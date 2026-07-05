# `runsIn`: 実行コンテキストの抽象化設計

- 日付: 2026-07-05
- ステータス: 設計承認済み（実装計画は未着手）

## 背景と動機

`uses:` の Git テンプレート（再利用ステップ）を、**再現性のあるコンテナ実行環境**で走らせたい。テンプレートが「どの環境で動くか」を固定できれば、ホストの差異に依存せず安定する。

同時に、通常 agent（`internal/agent`）と k8s-agent（`internal/k8sagent`）は実行モデルの思想が異なる。この差を **単一の抽象** に吸収し、テンプレートが backend を問わず同じ意味で走るようにするのが本設計の目的。

現状:
- 通常 agent は `bash -lc` でホスト実行のみ。DSL の `container:` フィールドを**完全に無視**する。
- k8s-agent の `step.Container` は「Job の `containers:` で定義された名前付き pod コンテナのどれに `kubectl exec` するか」を指す**名前参照**。空なら先頭コンテナ。
- `uses:` は実行時ユニットではなく、パース時に各ステップへ**インライン展開**される（`internal/gittemplate/inline.go` の `expandUsesStep`。名前プレフィックス＋ref書き換え）。

## 中核となる抽象: `runsIn`

ステップに **可搬な「実行コンテキスト」宣言** `runsIn` を追加する。`container:`（フラット文字列）を二重の意味でオーバーロードするのを避け、構造化フィールドで表現する。

```go
// internal/dsl
type RunsIn struct {
    Image     string // 新しい隔離環境を1つ起こす
    Container string // 事前プロビジョンされた名前付き環境を参照（k8s のみ）
}
```

`Image` と `Container` は**同階層の排他 union**。両方空（= `runsIn` 省略）は「デフォルト共有環境」。両方指定は parse エラー。

### 意味の定義（各 agent 共通の契約）

DSL の意味は**1箇所で定義**し、各 agent は resolver で具体実行に落とす。

| `runsIn` | 可搬な意味 | host-agent | k8s-agent |
|---|---|---|---|
| 省略 | 共有/デフォルト環境 | ホスト実行 (`bash -lc`) | デフォルト pod コンテナに exec |
| `image: X` | **新規の隔離環境** | `<runtime> run --rm X` | X で**使い捨て pod** を起こす |
| `container: N` | 名前付き既存環境 | **run 時エラー**（pod非対応） | pod 内コンテナ N に exec |

### 隔離契約（統一の要）

思想の差を **「共有 vs 隔離」という単一軸** に吸収する。

> **`image:`（隔離環境）は job の workspace / ファイルシステムを共有しない。**
> 入力は `with:`（→ env）で明示的に渡し、出力は `outputs:` / stdout で返す。
> これにより host でも k8s でも「純粋関数呼び出し」として**まったく同じ意味**になる。
>
> 省略時（共有環境）は従来どおり workspace を共有する。

隔離環境でも **env / secret は注入される**（`with` と env で入力を渡す契約に整合）。共有されないのは workspace（ファイルシステム）のみ。

前段の成果物に触れるテンプレートは、この契約下では `with:` / artifact 経由に書き直す必要がある。

## `uses` との統合

`uses:` はパース時に各ステップへインライン展開されるため:

- `uses:` ステップに付けた `runsIn` は、**展開時に各インラインステップへ伝播**する（内側ステップが自分の `runsIn` を持たなければ継承）。結果として「このテンプレートはこの環境で動く」を丸ごと固定できる。
- 展開後の内側ステップは通常の `run:` ステップなので、**`run:` ステップへの `runsIn` 指定も同じ仕組みで自然に成立**する。`uses` 専用に制限はしない。

## コンテナランタイム抽象

`docker run` を直書きせず、ランタイムごとの起動を **driver interface** に隔離する。CRI の *概念的ライフサイクル分割*（image pull / run / …）を interface 形状の指針として借りるが、**CRI（gRPC / containerd）はプロトコルとして採用しない**。理由: 対応したいランタイム（docker / podman / wslc / Apple `container`）はいずれも CRI エンドポイントを喋らない CLI ツールであり、CRI は pod サンドボックス＋CNI 前提で重すぎる。これらは**docker 互換 CLI 文法に収束**している（`wslc run --rm image cmd` 等）。

```go
// internal/runtime
type ContainerRuntime interface {
    Name() string
    Available() bool                    // CLI が PATH にあるか
    Pull(ctx context.Context, image string) error
    Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (exitCode int, err error)
    // 将来 exec/stop/remove が要れば CRI 準拠のライフサイクルで足す
}
```

ドライバ:
- **OCI-CLI ドライバ** 1個を `binary名 + 少数の quirk 表` でパラメータ化 → docker / podman / nerdctl / **wslc** を一括カバー。
- **Apple `container` ドライバ**（文法差があれば独立、無ければ quirk 表エントリ1行）。
- k8s-agent は同じ interface の「既存へ exec」「使い捨て pod を起こす」実装として乗る。

**選択と不在時の挙動:**
- 検出順で自動選択 + `--container-runtime` / config で明示上書き。
- `runsIn.image` 指定でホストにランタイムが無い場合は **run 時ハードエラー**。サイレントにホスト実行へフォールバックしない（再現性の意思表示を裏切らないため）。

## バリデーション

- `runsIn.image` と `runsIn.container` 同時指定 → **parse 時エラー**（排他）。
- host-agent で `runsIn.container` 指定 → **run 時エラー**（pod 非対応。parse は backend 非依存なので実行時に判定）。
- `runsIn.image` かつランタイム不在 → **run 時ハードエラー**。

## 既存 `container:` フィールドの移行

現状の `Container string`（k8s 名前参照）は `runsIn.container` へ移行する。当面はフラット `container:` を **deprecated エイリアス**として受理し、内部で `runsIn.container` に正規化。docs からは削除する。旧エージェント互換は matrix 設計と同様「全台更新前提」で扱う。

## secret / env の隔離環境への注入

既存の env 注入をランタイムの `-e` に渡す。ログの secret マスクは従来どおり効かせる（hyphen-aware collector）。

## k8s `runsIn.image`（別 pod）の実装

既存の `podmanager` / `podbuilder` / `podgc` を再利用して使い捨て pod を起こす。workspace 非共有（隔離契約どおり）、終了後に GC。

## パッケージ配置

- 新パッケージ `internal/runtime`: driver interface + OCI-CLI ドライバ + Apple / wslc ドライバ。
- host-agent（`internal/agent`）がこれを使って `runsIn` を解決。k8s-agent は自身の resolver で対応。
- DSL 変更は `internal/dsl/types.go`（`RunsIn` 追加、`Container` の正規化）と `internal/gittemplate/inline.go`（`runsIn` 伝播）。

## スコープ外（YAGNI）

- CRI（gRPC / containerd）を実行基盤にすること。
- 隔離環境間での workspace 共有（volume 引き継ぎ）。隔離は独立環境と割り切り、データは `with` / artifact で渡す。
- `--container-runtime` 以外のランタイム細粒度設定（リソース制限、ネットワーク等）は初版では扱わない。
```

## 実装の順序（目安）

1. `internal/dsl` に `RunsIn` を追加、`container:` の正規化とバリデーション。
2. `internal/gittemplate/inline.go` で `runsIn` を展開ステップへ伝播。
3. `internal/runtime` パッケージ: `ContainerRuntime` interface + OCI-CLI ドライバ（docker/podman/nerdctl/wslc）。
4. host-agent の runner で `runsIn` を解決（省略=ホスト、image=ランタイム run、container=エラー）。
5. Apple `container` ドライバ。
6. k8s-agent の `runsIn.image`（使い捨て pod）対応。
