# Secret Mask Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 複数行 secret の行単位マスク・パターンの長さ降順置換・outputs 送信前の secret 検出ガード(スキップ+警告)を実装する。

**Architecture:** `internal/secrets/masker.go` の `NewMasker` を強化(行分割登録+長さ降順ソート)し、`Detects` を追加。共有オーケストレータ(`internal/agent/orchestrator.go` の `RunClaim`)の outputs 送信 3 経路(step outputs / call 子 outputs 伝播 / run outputs)の直前に純関数 `FilterSecretOutputs` を挟み、除外 key ごとに slog + run ログへ警告 1 行を送る。host/k8s 両 agent は同一経路を通るため一箇所の変更で両対応。

**Tech Stack:** Go(標準ライブラリのみ)、testify/assert(既存テスト慣行)。

## Global Constraints

- 行分割登録の最小行長は **4 文字**(trim 後)。定数名 `minMaskLineLen = 4`。
- 行パターンには Base64/URL エンコード版を**登録しない**(値全体には現行どおり登録)。
- 警告行の書式: `output "<key>" skipped: value may contain a secret`(値そのものを警告に含めない)。
- 警告行の宛先: step outputs / call 伝播 = 該当ステップの `step.Index`、run outputs = `stepIndex = -1`(UI は -1 を "System" と表示する — RunDetail.svelte:465)。stream は `stderr`。
- in-memory の steps コンテキスト(`sctx.setStep` / `setStepMatrixOutputs`)は**フィルタしない**(同一 run 内の後続ステップ参照は許容。永続化経路だけ塞ぐ)。
- 既存インターフェース名を変えない: `NewMasker`, `Mask`, `NoOpMasker`, `SetMasker`。
- テストゲート: `go build ./... && go test -count=1 ./internal/secrets/ ./internal/agent/`(PG 不要のユニットのみ。Docker 起動不要)。

---

### Task 1: masker — 行分割登録・長さ降順ソート・Detects

**Files:**
- Modify: `internal/secrets/masker.go`(全 51 行の小ファイル、下記の完成形に置き換え)
- Test: `internal/secrets/masker_test.go`(既存テストの末尾に追記。既存テストは無改修で通ること)

**Interfaces:**
- Consumes: なし(自己完結)。
- Produces: `func (m *Masker) Detects(s string) bool` — Task 2 が使用。`NewMasker([]string) *Masker` / `Mask(string) string` / `NoOpMasker` はシグネチャ不変。

- [ ] **Step 1: 失敗するテストを書く** — `internal/secrets/masker_test.go` の末尾に追記:

```go
func TestMasker_MultilineSecretMasksPerLine(t *testing.T) {
	key := "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkq\nhkiG9w0BAQEFAASC\n-----END PRIVATE KEY-----"
	m := NewMasker([]string{key})
	// each line of a multi-line secret is masked on its own
	assert.Equal(t, "***", m.Mask("MIIEvgIBADANBgkq"))
	assert.Equal(t, "leaked: ***", m.Mask("leaked: hkiG9w0BAQEFAASC"))
	assert.Equal(t, "***", m.Mask("-----BEGIN PRIVATE KEY-----"))
}

func TestMasker_MultilineSecretTrimsCR(t *testing.T) {
	m := NewMasker([]string{"lineone-value\r\nlinetwo-value"})
	assert.Equal(t, "got ***", m.Mask("got lineone-value"))
	assert.Equal(t, "got ***", m.Mask("got linetwo-value"))
}

func TestMasker_ShortLinesNotRegistered(t *testing.T) {
	// "==" and "zz" are shorter than minMaskLineLen after trimming and must
	// not become patterns (they would over-mask unrelated output).
	m := NewMasker([]string{"abcdefgh\n==\nzz"})
	assert.Equal(t, "x == y", m.Mask("x == y"))
	assert.Equal(t, "zz top", m.Mask("zz top"))
	assert.Equal(t, "*** !", m.Mask("abcdefgh !"))
}

func TestMasker_PrefixSecretPairMasksLongestFirst(t *testing.T) {
	// registration order must not matter: the longer secret wins first
	m := NewMasker([]string{"tok_abc", "tok_abcdef"})
	assert.Equal(t, "have ***", m.Mask("have tok_abcdef"))
	assert.Equal(t, "have ***", m.Mask("have tok_abc"))
}

func TestMasker_Detects(t *testing.T) {
	m := NewMasker([]string{"s3cr3t"})
	assert.True(t, m.Detects("prefix s3cr3t suffix"))
	// encoded variants count as detections too
	assert.True(t, m.Detects("czNjcjN0"))
	assert.False(t, m.Detects("clean value"))
	assert.False(t, NoOpMasker.Detects("s3cr3t"))
}
```

- [ ] **Step 2: RED 確認**

Run: `go test -count=1 ./internal/secrets/ -run TestMasker -v`
Expected: `TestMasker_Detects` はコンパイルエラー(`m.Detects undefined`)。コンパイルを通すには先に Step 3 の一部が要るため、「未定義エラーで落ちる」ことを RED として確認すればよい。

- [ ] **Step 3: 実装** — `internal/secrets/masker.go` を以下の完成形に置き換え:

```go
package secrets

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
)

// minMaskLineLen is the minimum trimmed length for a line of a multi-line
// secret to become its own pattern. Shorter fragments (e.g. the "==" tail of
// a base64 block) would catastrophically over-mask unrelated output.
const minMaskLineLen = 4

// Masker masks sensitive information in output.
type Masker struct {
	patterns []string
}

// NoOpMasker is a Masker that masks nothing.
var NoOpMasker = &Masker{}

// NewMasker creates a Masker from a list of sensitive values.
// For every value it registers exact-match, Base64-encoded, and URL-encoded
// patterns. Multi-line values additionally register each trimmed line
// (>= minMaskLineLen) as its own pattern, because masking is applied per log
// line and the whole value can never match a single line. Patterns are kept
// longest-first so a secret that contains another secret as a substring is
// replaced before the shorter one can split it.
func NewMasker(values []string) *Masker {
	seen := map[string]struct{}{}
	var patterns []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			patterns = append(patterns, s)
		}
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		add(v)
		add(base64.StdEncoding.EncodeToString([]byte(v)))
		add(url.QueryEscape(v))
		if strings.Contains(v, "\n") {
			for _, ln := range strings.Split(v, "\n") {
				ln = strings.TrimSpace(ln)
				if len(ln) >= minMaskLineLen {
					add(ln)
				}
			}
		}
	}
	sort.SliceStable(patterns, func(i, j int) bool { return len(patterns[i]) > len(patterns[j]) })
	return &Masker{patterns: patterns}
}

// Mask replaces all registered patterns with "***".
func (m *Masker) Mask(line string) string {
	for _, p := range m.patterns {
		line = strings.ReplaceAll(line, p, "***")
	}
	return line
}

// Detects reports whether s contains any registered secret pattern.
func (m *Masker) Detects(s string) bool {
	for _, p := range m.patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: GREEN 確認**

Run: `go test -count=1 ./internal/secrets/ -v`
Expected: 既存テスト含め全 PASS(既存の `TestMasker_MasksExactValue` 等は無改修で通る)。

- [ ] **Step 5: Commit**

```bash
git add internal/secrets/masker.go internal/secrets/masker_test.go
git commit -m "feat(secrets): per-line masking for multiline secrets, longest-first replace, Detects"
```

---

### Task 2: orchestrator — outputs 送信前の secret 検出ガード

**Files:**
- Create: `internal/agent/outputsguard.go`
- Create: `internal/agent/outputsguard_test.go`
- Modify: `internal/agent/orchestrator.go`(3 送信経路: call 伝播 ≈ :318、step outputs ≈ :402、run outputs ≈ :529。行番号は目安 — `SetStepOutputs`/`SetRunOutputs` の呼び出し箇所を grep で特定して適用)

**Interfaces:**
- Consumes: Task 1 の `(*secrets.Masker).Detects(string) bool`、既存の `client.AppendLogBulk(ctx, agentID, runID, stepIndex, []api.LogAppendRequest) error`、`api.LogAppendRequest{RunID, StepIndex, Stream, Timestamp, Line}`。
- Produces: `func FilterSecretOutputs(outputs map[string]string, m *secrets.Masker, onSkip func(key string)) map[string]string`(後続タスクなし — 完結)。

- [ ] **Step 1: 失敗するテストを書く** — `internal/agent/outputsguard_test.go` を新規作成:

```go
package agent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/stretchr/testify/assert"
)

func TestFilterSecretOutputs_SkipsSecretValues(t *testing.T) {
	m := secrets.NewMasker([]string{"s3cr3t-value"})
	var skipped []string
	out := FilterSecretOutputs(map[string]string{
		"clean": "hello",
		"leaky": "token=s3cr3t-value",
	}, m, func(k string) { skipped = append(skipped, k) })
	assert.Equal(t, map[string]string{"clean": "hello"}, out)
	assert.Equal(t, []string{"leaky"}, skipped)
}

func TestFilterSecretOutputs_EncodedVariantAlsoSkipped(t *testing.T) {
	m := secrets.NewMasker([]string{"s3cr3t"})
	var skipped []string
	// base64("s3cr3t") = "czNjcjN0"
	out := FilterSecretOutputs(map[string]string{"b64": "czNjcjN0"}, m,
		func(k string) { skipped = append(skipped, k) })
	assert.Empty(t, out)
	assert.Equal(t, []string{"b64"}, skipped)
}

func TestFilterSecretOutputs_NoOpMaskerPassesThrough(t *testing.T) {
	out := FilterSecretOutputs(map[string]string{"k": "anything"}, secrets.NoOpMasker,
		func(k string) { t.Fatalf("unexpected skip: %s", k) })
	assert.Equal(t, map[string]string{"k": "anything"}, out)
}

func TestFilterSecretOutputs_NilMaskerPassesThrough(t *testing.T) {
	out := FilterSecretOutputs(map[string]string{"k": "v"}, nil,
		func(k string) { t.Fatalf("unexpected skip: %s", k) })
	assert.Equal(t, map[string]string{"k": "v"}, out)
}

func TestFilterSecretOutputs_DoesNotMutateInput(t *testing.T) {
	m := secrets.NewMasker([]string{"s3cr3t"})
	in := map[string]string{"leaky": "s3cr3t", "clean": "ok"}
	_ = FilterSecretOutputs(in, m, nil)
	assert.Equal(t, map[string]string{"leaky": "s3cr3t", "clean": "ok"}, in)
}
```

- [ ] **Step 2: RED 確認**

Run: `go test -count=1 ./internal/agent/ -run TestFilterSecretOutputs -v`
Expected: コンパイルエラー `undefined: FilterSecretOutputs`。

- [ ] **Step 3: 実装(純関数)** — `internal/agent/outputsguard.go` を新規作成:

```go
package agent

import "github.com/eirueimi/unified-cd/internal/secrets"

// FilterSecretOutputs returns a copy of outputs without entries whose value
// contains a known secret (per m.Detects); onSkip is called once per removed
// key. The input map is never mutated. A nil masker passes everything
// through. Persisted output channels (SetStepOutputs / SetRunOutputs) go
// through this guard; the in-run steps context deliberately does not — later
// steps in the same run may still reference the value, mirroring how GitHub
// Actions allows step outputs within a job but drops secret-bearing job
// outputs.
func FilterSecretOutputs(outputs map[string]string, m *secrets.Masker, onSkip func(key string)) map[string]string {
	filtered := make(map[string]string, len(outputs))
	for k, v := range outputs {
		if m != nil && m.Detects(v) {
			if onSkip != nil {
				onSkip(k)
			}
			continue
		}
		filtered[k] = v
	}
	return filtered
}
```

- [ ] **Step 4: GREEN 確認**

Run: `go test -count=1 ./internal/agent/ -run TestFilterSecretOutputs -v`
Expected: 5/5 PASS。

- [ ] **Step 5: 3 送信経路への配線** — `internal/agent/orchestrator.go`。

まず `RunClaim` 内、masker 構築(`b.SetMasker(masker)` の直後)に警告エミッタを追加:

```go
	// warnSkippedOutput surfaces a dropped output both to the agent log and
	// into the run's own logs (stepIndex -1 renders as "System" in the UI).
	warnSkippedOutput := func(ctx context.Context, stepIndex int, key string) {
		slog.Warn("output skipped: value may contain a secret",
			"runId", c.RunID, "stepIndex", stepIndex, "key", key)
		_ = client.AppendLogBulk(ctx, agentID, c.RunID, stepIndex, []api.LogAppendRequest{{
			RunID:     c.RunID,
			StepIndex: stepIndex,
			Stream:    "stderr",
			Timestamp: time.Now().UTC(),
			Line:      fmt.Sprintf("output %q skipped: value may contain a secret", key),
		}})
	}
```

(`fmt`/`time`/`api` は既にインポート済みのはず — 無ければ追加。)

**経路 1 — call 子 outputs 伝播**(`ExecuteCallStep` 成功後の送信。in-memory `sctx.setStep*` は childOutputs のまま変更しない):

```go
					if len(childOutputs) > 0 {
						safe := FilterSecretOutputs(childOutputs, masker, func(k string) {
							warnSkippedOutput(stepCtx, step.Index, k)
						})
						if len(safe) > 0 {
							_ = client.SetStepOutputs(stepCtx, agentID, c.RunID, step.Index, step.MatrixKey, safe)
						}
					}
```

**経路 2 — step outputs**(capturedOutputs の送信。直前の `sctx.setStep*` は無変更):

```go
					if len(capturedOutputs) > 0 {
						safe := FilterSecretOutputs(capturedOutputs, masker, func(k string) {
							warnSkippedOutput(stepCtx, step.Index, k)
						})
						if len(safe) > 0 {
							_ = client.SetStepOutputs(stepCtx, agentID, c.RunID, step.Index, step.MatrixKey, safe)
						}
					}
```

**経路 3 — run outputs**(`retryUntilSuccess` の外でフィルタし、空になったら送信自体をやめる。stepIndex は -1):

```go
	if len(runOutputs) > 0 {
		safeRunOutputs := FilterSecretOutputs(runOutputs, masker, func(k string) {
			warnSkippedOutput(finishCtx, -1, k)
		})
		if len(safeRunOutputs) > 0 {
			// Retried until success (unlike the pre-refactor host single-shot
			// call): a transient failure here must not silently drop job outputs,
			// matching the pre-refactor k8s agent's RetryUntilSuccess wrapping.
			// Deliberate reconciliation pick — see Task 8 report.
			retryUntilSuccess(finishCtx, func(callCtx context.Context) error {
				return client.SetRunOutputs(callCtx, agentID, c.RunID, safeRunOutputs)
			})
		}
	}
```

- [ ] **Step 6: ビルド+フルゲート**

Run: `go build ./... && go test -count=1 ./internal/secrets/ ./internal/agent/`
Expected: build クリーン、両パッケージ `ok`。

- [ ] **Step 7: Commit**

```bash
git add internal/agent/outputsguard.go internal/agent/outputsguard_test.go internal/agent/orchestrator.go
git commit -m "feat(agent): skip persisted outputs that may contain secrets, warn into run logs"
```

---

### Task 3: spec ステータス更新

**Files:**
- Modify: `docs/superpowers/specs/2026-07-07-secret-mask-hardening-design.md`(ステータス行のみ)

- [ ] **Step 1: ステータスを「実装済み」へ** — `- ステータス: 設計レビュー中` を `- ステータス: 実装済み` に変更。

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/specs/2026-07-07-secret-mask-hardening-design.md
git commit -m "docs(spec): secret mask hardening implemented"
```
