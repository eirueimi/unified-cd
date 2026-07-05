# runsIn.resources 設計（Plan B-2 ①: CPU/メモリ制限）

**日付:** 2026-07-05
**前提:** runsIn Plan A（host + k8s の①②）と Plan B（k8s `runsIn.image` 使い捨て pod）はマージ済み。本設計は `runsIn.image` ステップに CPU/メモリの requests/limits を付けられるようにする。

## Goal

`runsIn.image` のステップに、DSL で CPU/メモリの resources（requests/limits）を宣言できるようにする。k8s では使い捨て pod の `container.Resources` に、host では `docker`/`podman`/`nerdctl` の `--cpus`/`--memory` にマッピングする。

## スコープ

- **含む:** DSL `runsIn.resources`（requests/limits × cpu/memory）、apply 時のバリデーション、k8s（container.Resources）と host（OCI-CLI フラグ）両対応、スキーマ/docs 再生成。
- **含まない（別イテレーション）:** PodTemplate/agent 設定による既定値・二段継承（未指定は制限なし）、Apple `container` CLI の resource フラグ（実機未検証のため据え置き）、ネットワーク/volume（Plan B-2 ②③）。

## 決定事項

- **backend:** host + k8s 両対応。
- **既定値:** ステップ単位の `runsIn.resources` のみ。未指定は制限なし（現状維持）。
- **適用対象:** `runsIn.image` ステップ専用。`runsIn.container`/デフォルト（named pod）は既存 PodTemplate の resources を使うため、本フィールドの対象外。

## DSL サーフェス

```yaml
- name: build
  run: go build ./...
  runsIn:
    image: golang:1.22
    resources:
      requests:
        cpu: "500m"
        memory: "256Mi"
      limits:
        cpu: "1"
        memory: "512Mi"
```
- `requests`/`limits` はそれぞれ任意。`cpu`/`memory` もそれぞれ任意。
- 値は **k8s quantity 文字列**（`"500m"`, `"1"`, `"256Mi"`, `"1Gi"` 等）。

### 型（`internal/dsl/types.go`）
```go
type RunsIn struct {
	Image     string        `yaml:"image,omitempty" json:"image,omitempty"`
	Container string        `yaml:"container,omitempty" json:"container,omitempty"`
	Resources *ResourceSpec `yaml:"resources,omitempty" json:"resources,omitempty"`
}

type ResourceSpec struct {
	Requests *ResourceList `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits   *ResourceList `yaml:"limits,omitempty" json:"limits,omitempty"`
}

type ResourceList struct {
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
}
```

## バリデーション（`internal/dsl/parse.go`, apply 時）

- 各 quantity 文字列を `resource.ParseQuantity`（`k8s.io/apimachinery/pkg/api/resource`）で parse し、**不正値はハードエラー**（apply 時に弾く）。空文字はスキップ。
- `Resources` が非 nil かつ `Image` が空（＝image ステップでない）→ **ハードエラー**（`runsIn.resources requires runsIn.image`）。
- 設計判断: DSL パッケージが apimachinery/resource を import（新規）。モジュールは既に vendor 済み（k8sagent 経由）で追加依存は無い。quantity 形式は DSL が採用した標準なので、その正準パーサで検証するのが妥当。

## k8s パス（`internal/k8sagent`）

- `buildImageStepPod(..., resources *dsl.ResourceSpec)` を受け、非 nil なら `container.Resources = corev1.ResourceRequirements{Requests, Limits}` を設定。各 quantity は `resource.ParseQuantity`（検証済みなのでエラーは内部エラー扱い）で `corev1.ResourceList` に変換。nil ならフィールド未設定（現状どおり無制限）。
- `stepExec`/`runImageStep` が `step.RunsIn.Resources` を `buildImageStepPod` に渡す。

## host パス（`internal/runtime` + `internal/agent`）

- `RunSpec` に `CPULimit, MemLimit string` を追加（変換済みの値: cpu=コア小数, memory=バイト）。
- `ociCLI.runArgs`（docker/podman/nerdctl/wslc）で、`run --rm` 直後に `--cpus=<CPULimit>` / `--memory=<MemLimit>` を、それぞれ非空のとき追加。
- **host は limits のみ**マッピング（docker に requests 概念なし）。requests は k8s スケジューリング専用として host では無視。
- 変換: memory は `ParseQuantity(limits.memory).Value()`＝バイト整数、cpu は `ParseQuantity(limits.cpu).MilliValue()/1000`＝コア小数（例 "500m"→"0.5", "2"→"2"）。変換は host agent 側（`RunStepContainer` 呼び出し前）で行い、`RunSpec` には確定値を載せる。
- **Apple `container` ドライバは resource フラグ未対応（据え置き）**。`appleContainer.runArgs` は従来どおり（resource 無視）。ドライバ単位で対応差がある旨をコメントで明記。

## ワイヤ + 配線

- `api.ClaimStep.RunsIn` は `*dsl.RunsIn` をそのまま運ぶので、**`Resources` は自動的に agent へ届く**（controller の配線変更は不要）。
- host agent の dispatch（`internal/agent/agent.go` の runsIn.image 分岐）が `step.RunsIn.Resources.Limits` を変換して `RunSpec` に載せる。k8s agent の `stepExec` が `step.RunsIn.Resources` を `buildImageStepPod` に渡す。

## エラー処理

- 不正 quantity・`resources` without `image` は **apply 時にハードエラー**（run 前に弾く）。
- 変換後の値が空（未指定）なら該当フラグ/フィールドを付けない（無制限）。
- limit 超過時の挙動は backend 任せ（k8s: OOMKill/スロットル、docker: 同様）— 本設計はマッピングのみ。

## テスト

- **DSL:** 正常 parse（requests/limits × cpu/memory）／不正 quantity → エラー／`resources` + `image` 空 → エラー。
- **runtime(host):** `ociCLI.runArgs` が `--cpus`/`--memory` を正しい位置・値で出す（"500m"→"0.5", "512Mi"→"536870912"）。未指定でフラグ無し。
- **host agent:** limits を変換して RunSpec に載せ、requests は無視することの検証（変換ヘルパーの単体テスト）。
- **k8s:** `buildImageStepPod` が `container.Resources`（requests/limits 両方）を正しく設定。nil で未設定。
- **スキーマ:** `go generate ./internal/dsl/` で `runsIn.resources` がスキーマ/`field-reference` に反映され差分がコミットされること。

## Plan B-2 の残り（本スコープ外）

- resources のデフォルト/二段継承（PodTemplate/agent 設定）。
- ネットワーク / NetworkPolicy（②）。
- volume の選択的引き継ぎ（③）。
- Apple `container` CLI の resource フラグ実機検証。
