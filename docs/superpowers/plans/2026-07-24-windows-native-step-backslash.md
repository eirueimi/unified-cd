# Windows native step バックスラッシュ破損 修正 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Windows エージェントの native ステップで、スクリプト中の連続バックスラッシュが破損しないよう、`RunStep`/`RunStepCapture` を env-var 経由の実行に変える。

**Architecture:** Windows のみ、bash に script を argv でなく環境変数 `__UCD_STEP_SCRIPT` で渡し、固定のバックスラッシュ非依存ローダ `bash -lc 'eval "$__UCD_STEP_SCRIPT"'` を実行する。環境ブロックは Go の Windows argv エスケープの対象外なのでバイト列が保たれる。非 Windows は現行の argv 直渡しのまま挙動不変。

**Tech Stack:** Go (`os/exec`, `runtime.GOOS`)、testify (`require`/`assert`)。

## Global Constraints

- 修正対象は `RunStep` と `RunStepCapture` の 2 経路のみ。`RunStepWithShell`（カスタム interpreter）は**変更しない**（doc に Windows 制限を追記するのみ）。
- 非 Windows（`runtime.GOOS != "windows"`）の挙動は**完全に不変**。分岐は Windows 限定。
- 環境変数名は `__UCD_STEP_SCRIPT`（この名前で固定）。ローダ argv は `eval "$__UCD_STEP_SCRIPT"`（バックスラッシュを含めない）。
- `-l`（login shell）を維持する（現行 `bash -lc` と同じ）。
- cancel / process-tree-kill / exit code / stdout・stderr の扱いは変えない（同じ `*exec.Cmd` を `runTreeKilled` に渡す）。
- 保持すべき「壊れてはいけない」テスト対象文字列: `s|\\\\|\\|g`（バックスラッシュ 4 個・2 個）。argv エスケープが働くと `s|\\|\|g`（2 個・1 個）に半減する。

---

## File Structure

| ファイル | 責務 |
|---|---|
| `internal/agent/runner.go` | `buildBashStepCmd` ヘルパを追加し、`RunStep`/`RunStepCapture` をそれ経由に変更。`RunStepWithShell` の doc に Windows 制限を追記 |
| `internal/agent/runner_test.go` | `RunStep`/`RunStepCapture` のバックスラッシュ保持テストを追加 |

`backend_host.go` は `RunStep`/`RunStepCapture` のシグネチャが不変のため変更不要。

---

## Task 1: env-var 経由の bash 実行でバックスラッシュを保持する

**Files:**
- Modify: `internal/agent/runner.go`（`RunStep` 101-119 / `RunStepCapture` 158-178 / `RunStepWithShell` doc 121-130、`buildBashStepCmd` 新規追加）
- Test: `internal/agent/runner_test.go`

**Interfaces:**
- Consumes: 既存の `findShell() string`、`StepEnv(exposeEnv, extraEnv []string) []string`、`runTreeKilled(ctx, *exec.Cmd) error`。
- Produces: `buildBashStepCmd(script string, baseEnv []string) *exec.Cmd` — argv と `cmd.Env` を組んだ `*exec.Cmd` を返す（`Stdout`/`Stderr`/`Dir` は未設定）。Windows では env に `__UCD_STEP_SCRIPT=<script>` を追加しローダ argv を使う。`RunStep`/`RunStepCapture` のシグネチャは不変。

- [ ] **Step 1: RunStep のバックスラッシュ保持テストを書く（失敗するはず）**

`internal/agent/runner_test.go` の末尾に追記する。`bytes`・`require`・`assert` は既に import 済み。

```go
// TestRunStep_PreservesBackslashRuns guards the Windows argv-escaping bug: a
// native step script that spells out runs of backslashes (e.g. a sed
// s|\\\\|\\|g) must reach bash intact. On Windows, passing the script as an
// exec argv halves backslash runs (s|\\|\|g), corrupting the script; the fix
// routes the script through an environment variable instead. On Unix there is
// no such corruption, so this test also documents the expected behavior there.
func TestRunStep_PreservesBackslashRuns(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// printf %s of a single-quoted literal: bash echoes the argument verbatim.
	exit, err := RunStep(t.Context(), `printf '%s' 's|\\\\|\\|g'`, &stdout, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Equal(t, `s|\\\\|\\|g`, stdout.String(), "backslash runs must survive (stderr: %s)", stderr.String())
}
```

- [ ] **Step 2: テストを実行して失敗を確認（Windows）**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd && go test ./internal/agent/ -run TestRunStep_PreservesBackslashRuns -v`

Expected（この Windows 開発機）: FAIL。`stdout` が `s|\\|\|g`（半減）になり `assert.Equal` が
```
Error: Not equal: expected: "s|\\\\|\\|g"  actual: "s|\\|\|g"
```
で落ちる。（Linux 上で走らせた場合はバグが無いので PASS するが、修正対象は Windows なので開発機で FAIL することが確認になる。）

- [ ] **Step 3: `buildBashStepCmd` ヘルパを追加する**

`internal/agent/runner.go` の `RunStep` 関数の**直前**（99 行目 `// that directory as the working directory.` コメントの前、`RunStep` の doc コメント群の前）に挿入する。`runtime` は既に import 済み。

```go
// buildBashStepCmd builds the *exec.Cmd for running a native step's script with
// bash. On Windows the script travels via the __UCD_STEP_SCRIPT environment
// variable and the argv is a fixed, backslash-free loader (eval "$__UCD_STEP_SCRIPT"):
// Go's Windows argv escaping halves runs of backslashes before MSYS (Git Bash)
// re-parses the command line, which corrupts any script that spells out
// backslashes (e.g. a sed s|\\...|\\...). The environment block is not subject
// to that escaping, so the bytes survive. On every other platform the script is
// passed directly as the -lc argument, unchanged. baseEnv is the caller's
// already-built StepEnv result; the returned cmd has Env set but leaves
// Stdout/Stderr/Dir to the caller.
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

- [ ] **Step 4: `RunStep` をヘルパ経由に変更する**

`internal/agent/runner.go` の `RunStep` 本体（102-110 行）を置き換える。

置換前:

```go
	cmd := exec.Command(findShell(), "-lc", script)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Always set Env: a nil cmd.Env makes os/exec inherit the agent's whole
	// environment, which is exactly the leak StepEnv exists to prevent.
	cmd.Env = StepEnv(exposeEnv, extraEnv)
	if workDir != "" {
		cmd.Dir = workDir
	}
```

置換後:

```go
	// Env is set inside buildBashStepCmd (never nil): a nil cmd.Env makes
	// os/exec inherit the agent's whole environment, which is exactly the leak
	// StepEnv exists to prevent.
	cmd := buildBashStepCmd(script, StepEnv(exposeEnv, extraEnv))
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if workDir != "" {
		cmd.Dir = workDir
	}
```

- [ ] **Step 5: テストを実行して成功を確認**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd && go test ./internal/agent/ -run TestRunStep_PreservesBackslashRuns -v`

Expected: PASS。

- [ ] **Step 6: `RunStepCapture` のバックスラッシュ保持テストを書く（失敗するはず）**

`internal/agent/runner_test.go` に追記する。

```go
// TestRunStepCapture_PreservesBackslashRuns mirrors
// TestRunStep_PreservesBackslashRuns for the capture path: the returned stdout
// string must contain the un-halved backslash runs.
func TestRunStepCapture_PreservesBackslashRuns(t *testing.T) {
	var stderr bytes.Buffer
	stdout, exit, err := RunStepCapture(t.Context(), `printf '%s' 's|\\\\|\\|g'`, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Equal(t, `s|\\\\|\\|g`, stdout, "backslash runs must survive (stderr: %s)", stderr.String())
}
```

- [ ] **Step 7: テストを実行して失敗を確認**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd && go test ./internal/agent/ -run TestRunStepCapture_PreservesBackslashRuns -v`

Expected（Windows 開発機）: FAIL。`stdout` が `s|\\|\|g` に半減して `assert.Equal` が落ちる。

- [ ] **Step 8: `RunStepCapture` をヘルパ経由に変更する**

`internal/agent/runner.go` の `RunStepCapture` 本体（160-165 行）を置き換える。

置換前:

```go
	var stdoutBuf bytes.Buffer
	cmd := exec.Command(findShell(), "-lc", script)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = stderr
	// Always set Env: a nil cmd.Env makes os/exec inherit the agent's whole
	// environment, which is exactly the leak StepEnv exists to prevent.
	cmd.Env = StepEnv(exposeEnv, extraEnv)
	if workDir != "" {
		cmd.Dir = workDir
	}
```

置換後:

```go
	var stdoutBuf bytes.Buffer
	// Env is set inside buildBashStepCmd (never nil): a nil cmd.Env makes
	// os/exec inherit the agent's whole environment, which is exactly the leak
	// StepEnv exists to prevent.
	cmd := buildBashStepCmd(script, StepEnv(exposeEnv, extraEnv))
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = stderr
	if workDir != "" {
		cmd.Dir = workDir
	}
```

- [ ] **Step 9: テストを実行して成功を確認**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd && go test ./internal/agent/ -run TestRunStepCapture_PreservesBackslashRuns -v`

Expected: PASS。

- [ ] **Step 10: `RunStepWithShell` の doc に Windows 制限を追記する**

`internal/agent/runner.go` の `RunStepWithShell` doc コメント（121-130 行）の末尾、`// RunStep's doc comment.` の行の直後に追記する。

追記後（該当コメントの末尾がこうなる）:

```go
// (see runTreeKilled). exposeEnv is the agent's ExposeEnv allowlist; see
// RunStep's doc comment.
//
// Windows note: the script is passed as a process argument, so a custom
// interpreter script that contains runs of backslashes can be corrupted by
// Windows argv escaping (Go escapes with MSVCRT rules, MSYS bash parses with
// its own). The default bash path (RunStep) avoids this via the
// __UCD_STEP_SCRIPT environment variable; this explicit-shell path does not yet.
```

- [ ] **Step 11: エージェント全体のテストを実行して回帰が無いことを確認**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd && go test ./internal/agent/ 2>&1 | tail -20`

Expected: `ok  	github.com/eirueimi/unified-cd/internal/agent`。既存テスト（`TestRunStep_CapturesStdout`, `TestRunStep_NonZeroExit`, `TestRunStep_WorkDir`, `TestRunStep_RespectsContextCancel`, `TestRunStepCapture_ReturnsStdout`, `TestRunStepCapture_WorkDir`, `TestRunStep_CredentialsNotInheritedByChild`, `TestRunStepWithShell_*` 群）を含め全 PASS。

> `internal/agent` の一部テストは docker を使う可能性がある（メモリの CI flake 参照）。runner 系の unit テストは docker 不要。もし無関係な docker 依存テストが環境要因で落ちた場合は、`-run 'RunStep|RunStepCapture|RunStepWithShell|StepEnv'` に絞って本タスクの範囲が緑であることを確認する。

- [ ] **Step 12: コミット**

```bash
cd /c/Users/arimax/unified-cd-project/unified-cd
git add internal/agent/runner.go internal/agent/runner_test.go
git commit -F - <<'EOF'
fix(agent): Windows で native step の連続バックスラッシュ破損を修正

Windows では exec.Command(bash,"-lc",script) の script 引数を Go が MSVCRT
規則でエスケープし、MSYS の Git Bash が別規則でパースするため、連続する
バックスラッシュが半減していた（s|\\\\|\\|g -> s|\\|\|g で sed が
unterminated）。RunStep / RunStepCapture を Windows のみ env-var 経由
(eval "$__UCD_STEP_SCRIPT") に変更し、環境ブロック経由でバイト列を保つ。
非 Windows は argv 直渡しのまま不変。RunStepWithShell（カスタム
interpreter）は対象外で、doc に Windows 制限を明記。

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Self-Review

**1. Spec coverage:**
- 「Windows のみ env-var+eval」→ Task1 Step3-4, 8（`buildBashStepCmd` の Windows 分岐）。
- 「非 Windows 不変」→ Step3 の else 分岐（argv 直渡し）。
- 「`buildBashStepCmd` DRY ヘルパ」→ Step3、両経路が Step4/8 で使用。
- 「バックスラッシュ保持テスト（RunStep/RunStepCapture）」→ Step1, 6。
- 「既存テストの不変性」→ Step11。
- 「`RunStepWithShell` は変更せず doc 追記」→ Step10。
- 「cancel/tree-kill/exit code 不変」→ `runTreeKilled` と ExitError 処理は未変更（Step4/8 は cmd 構築のみ差し替え）。
- 全 spec 要件にタスクが対応。ギャップなし。

**2. Placeholder scan:** TBD/TODO/曖昧指示なし。各コード手順に完全なコードを記載。

**3. Type consistency:** `buildBashStepCmd(script string, baseEnv []string) *exec.Cmd` は Step3 定義・Step4/8 呼び出しで一致。env 変数名 `__UCD_STEP_SCRIPT` はローダ argv とヘルパ・spec・doc・commit で一致。テスト対象文字列 `s|\\\\|\\|g` は全手順で一致。
