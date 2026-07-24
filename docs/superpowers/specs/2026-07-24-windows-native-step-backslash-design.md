# Windows native step のバックスラッシュ破損 修正 — 設計

日付: 2026-07-24
対象: `unified-cd`（agent）

## 背景と問題

Windows エージェントで `native: true` ジョブのステップを実行すると、スクリプト中の
**連続するバックスラッシュが半減**し、シェルが誤動作する。

`internal/agent/runner.go` の `RunStep` / `RunStepCapture` は、ステップの
スクリプトを bash の argv として渡す:

```go
cmd := exec.Command(findShell(), "-lc", script)
```

`findShell()` は Windows では Git Bash（MSYS2）を返す。Go の `os/exec` は Windows で
argv を MSVCRT 規則（`syscall.EscapeArg`）でエスケープしてコマンドラインを組み立てるが、
MSYS2 の bash は独自規則でコマンドラインをパースする。この**食い違い**により、
バックスラッシュの連続が半減する。

実測（`exec.Command(bash, "-lc", script)` で確認）:

| 送信（SENT） | bash 受信（RECEIVED） |
|---|---|
| `s\|\\\\\|\\\|g`（`\` 4個, 2個） | `s\|\\\|\\|g`（2個, 1個） |

`s|\\\\|\\|g`（バックスラッシュ4個・2個）が `s|\\|\|g`（2個・1個）になり、末尾の
`\|` が sed の区切りをエスケープして「unterminated `s' command」で失敗する。

追加の実測で判明した重要な性質:

- **連続（2個以上）のバックスラッシュだけが半減する。**
- **単独のバックスラッシュ**（`\r`, `\n`, `\"`, 文字前の `\`）は**無傷で生存**する。
- 環境変数の**値**として渡したバイト列は**破損しない**（環境ブロックは argv
  エスケープの対象外）。

この問題は 2026-07-24 に、Unity 検出テンプレート（`resolve-unity-path`）の
`sed 's|\\\\|\\|g'` が Windows エージェントで失敗したことで表面化した。テンプレート
側はバックスラッシュ非依存に書き換えて回避済みだが、これはエージェント側の根本
バグであり、**Windows エージェントで実行されるあらゆる backslash を含む native
スクリプト**に影響する。

## ゴール

- Windows エージェントの native ステップで、スクリプト中の連続バックスラッシュが
  破損せず、書いたとおりに実行される。
- 非 Windows（Linux/macOS）の挙動は完全に不変に保つ。
- cancel / process-tree-kill / exit code / stdout・stderr の扱いを変えない。

## 非ゴール

- `RunStepWithShell`（`shell:` でカスタム interpreter を宣言した経路、例
  `shell: [python3, -c]`）の修正。カスタム interpreter は `eval` を使えず、
  interpreter 毎のローダが必要で別設計になるため、今回は対象外とする。その doc
  コメントに Windows での制限を明記する。
- 極端に大きいスクリプト（数十 KB 超）の Windows コマンドライン/環境ブロック長
  上限への対応。argv 方式でも同等の上限があり、本修正で悪化しない。

## アーキテクチャ

Windows のみ、`RunStep` / `RunStepCapture` は script を**環境変数経由**で bash に
渡し、argv には**固定のバックスラッシュ非依存ローダ**だけを渡す:

```
bash -lc 'eval "$__UCD_STEP_SCRIPT"'
```

`__UCD_STEP_SCRIPT` の値（＝実際のスクリプト）は `cmd.Env` に載る。環境ブロックは
argv エスケープの対象外なのでバイト列が保たれ、bash がそれを `eval` して実行する。
`-l`（login shell）は現行どおり維持する。

非 Windows は現行のまま `exec.Command("bash", "-lc", script)` で、挙動は一切変えない。

実測で本方式がバックスラッシュを保持することを確認済み:

| 方式 | bash 受信 |
|---|---|
| argv（現行） | `s\|\\\|\\|g`（半減） |
| env-var + eval（本方式） | `s\|\\\\\|\\\|g`（保持） |

## コンポーネント

### `buildBashStepCmd`（新規ヘルパ、DRY）

`RunStep` と `RunStepCapture` は同一のコマンド構築ロジックを持つべきなので、
1 箇所に集約する。

```go
// buildBashStepCmd builds the *exec.Cmd for running a native step's script with
// bash. On Windows the script travels via the __UCD_STEP_SCRIPT environment
// variable and the argv is a fixed, backslash-free loader: Go's Windows argv
// escaping halves runs of backslashes before MSYS bash re-parses the command
// line, which corrupts any script that spells out backslashes (e.g. a sed
// s|\\...). The environment block is not subject to that escaping, so the bytes
// survive. On every other platform the script is passed directly as the -lc
// argument, unchanged.
func buildBashStepCmd(script string, baseEnv []string) *exec.Cmd {
    if runtime.GOOS == "windows" {
        cmd := exec.Command(findShell(), "-lc", `eval "$__UCD_STEP_SCRIPT"`)
        cmd.Env = append(baseEnv, "__UCD_STEP_SCRIPT="+script)
        return cmd
    }
    cmd := exec.Command(findShell(), "-lc", script)
    cmd.Env = baseEnv
    return cmd
}
```

- `baseEnv` は呼び出し側が組み立てた `StepEnv(exposeEnv, extraEnv)` の結果。
  Windows 分岐では最後に `__UCD_STEP_SCRIPT=<script>` を追記する（`os/exec` は
  同名の最後の要素を採用）。
- ヘルパは cmd の `Stdout` / `Stderr` / `Dir` は設定しない（呼び出し側が従来どおり
  設定する）。責務は「argv と env を正しく組む」ことに限定する。

### `RunStep` の変更

現行（`runner.go:101-110` 付近）:

```go
cmd := exec.Command(findShell(), "-lc", script)
cmd.Stdout = stdout
cmd.Stderr = stderr
cmd.Env = StepEnv(exposeEnv, extraEnv)
if workDir != "" {
    cmd.Dir = workDir
}
```

変更後:

```go
cmd := buildBashStepCmd(script, StepEnv(exposeEnv, extraEnv))
cmd.Stdout = stdout
cmd.Stderr = stderr
if workDir != "" {
    cmd.Dir = workDir
}
```

### `RunStepCapture` の変更

`runner.go:160` 付近を同様に `buildBashStepCmd` 経由に置き換える（stdout は
`&stdoutBuf` を設定する既存ロジックを維持）。

### `RunStepWithShell` の doc 追記

修正はしないが、doc コメントに以下を明記する:

> Windows note: the script is passed as a process argument, so a custom
> interpreter script that contains runs of backslashes can be corrupted by
> Windows argv escaping. The default bash path (RunStep) avoids this via an
> environment variable; the explicit-shell path does not yet.

## データフロー

```
backend_host.RunDefault
  ├─ step.Shell == nil → RunStep(script, ...)
  │     → buildBashStepCmd(script, StepEnv(...))  // Windows: env+loader / 他: argv
  │     → runTreeKilled(ctx, cmd)                 // 変更なし
  └─ step.Shell != nil → RunStepWithShell(...)    // 変更なし（今回対象外）
```

`RunStepCapture` は `backend_host` の native 経路とは別に、出力を取り込む用途で
呼ばれる（例: uses:-scope の出力キャプチャ等）。同じヘルパを使う。

## エラーハンドリング

新しいエラー経路は無い。cmd の argv と env が変わるだけで、`runTreeKilled` に渡す
`*exec.Cmd` の実行・cancel・process-tree-kill・ExitError からの exit code 取り出しは
現行と同一。`__UCD_STEP_SCRIPT` は常にこちらで設定するので未定義になることはなく、
`eval "$__UCD_STEP_SCRIPT"` が空文字を eval することもない。

## テスト方針（TDD）

### 中心: バックスラッシュ保持の回帰テスト（全 OS で有効）

```go
func TestRunStep_PreservesBackslashRuns(t *testing.T) {
    var out, errb bytes.Buffer
    // sed line98 のペイロード。argv エスケープが働くと s|\\|\|g に半減する。
    exit, err := RunStep(t.Context(), `printf '%s' 's|\\\\|\\|g'`, &out, &errb, nil, nil, "")
    require.NoError(t, err)
    require.Equal(t, 0, exit)
    require.Equal(t, `s|\\\\|\\|g`, out.String())
}
```

- **現行コードでは Windows で FAIL**（`s|\\|\|g` に半減）し、修正後 PASS。
- **Unix ではバグが無いため修正前後どちらも PASS**。したがってこのテストは
  Windows での回帰ガードであり、Unix では正しさの確認になる（クロスプラット
  フォームで意味を持つ）。
- `RunStepCapture` 版（`TestRunStepCapture_PreservesBackslashRuns`）も追加し、
  返り値 `stdout` が半減していないことを確認する。

### 既存テストの不変性

`RunStep` / `RunStepCapture` / `RunStepWithShell` の既存テスト（stdout 取得、
非ゼロ exit、workDir、context cancel、`TestRunStep_CredentialsNotInheritedByChild`）は
すべて修正後も PASS すること。特に credential 非継承テストは、env に
`__UCD_STEP_SCRIPT` を足しても StepEnv の allowlist/denylist（`stepenv.go`）が
崩れないことの確認になる。

### 内部変数の非干渉（任意）

`__UCD_STEP_SCRIPT` が既存の env と衝突しないことを軽く確認する（アンダースコア
2つ始まりの内部名で衝突可能性は低い）。実行中の step からは見えるが実害はない。

## 影響ファイル

| ファイル | 変更 |
|---|---|
| `internal/agent/runner.go` | `buildBashStepCmd` 追加、`RunStep`/`RunStepCapture` を経由に変更、`RunStepWithShell` の doc に Windows 制限を追記 |
| `internal/agent/runner_test.go` | バックスラッシュ保持テストを `RunStep`/`RunStepCapture` に追加 |

`backend_host.go` は変更不要（`RunStep` のシグネチャは不変）。

## 未解決事項

なし。
