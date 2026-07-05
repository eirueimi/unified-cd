# matrix ステップ実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `foreach:`(1次元)を多次元 `matrix:` に一般化し、parallel内foreach非展開バグを解消する。

**Architecture:** DSL層に `MatrixDef` と `EvalMatrix`(デカルト積+exclude+上限)を追加し、foreach は1次元matrixへの変換シュガーにする。ワイヤ形式は `ClaimStep.ForeachKey/ForeachValue` を `MatrixValues map[string]string` + `MatrixKey string` に置換(後方互換なし)。展開は従来どおりエージェント側。出力は組み合わせキーで集約したマップになる。

**Tech Stack:** Go 1.24+, gopkg.in/yaml.v3, cel-go, PostgreSQL(golang-migrate 形式マイグレーション), testify

**仕様書:** `docs/superpowers/specs/2026-07-05-matrix-design.md`(承認済み。判断に迷ったらこちらが正)

## Global Constraints

- 組み合わせキーの正規形: **次元宣言順に値を `/` で結合**(例: `linux/amd64`)。次元の値に `/` が含まれたら展開時エラー
- 組み合わせ数上限: コントローラ設定 `--matrix-max-combinations` / env `UNIFIED_MATRIX_MAX_COMBINATIONS`、デフォルト **64**。ClaimResponse でエージェントに配布
- `foreach` と `matrix` の同時指定は apply 時エラー(相互排他)
- 旧エージェント互換なし(ForeachKey/ForeachValue フィールドは削除する。コントローラとエージェントは同時更新)
- matrixステップの出力は集約マップ: `{{ .Steps.build.Outputs.version }}` → `map[組み合わせキー]値`
- 展開後の表示名: `build (linux, amd64)`(= MatrixKey の `/` を `, ` に置換して括弧付与)
- 空の次元 → 組み合わせ0件 → ステップは実行されない(ランは失敗しない)。上限超過・`/`混入・式エラーは展開エラー → ラン失敗
- テスト実行: `go test ./...`(unified-cd リポジトリルートで)。PostgreSQL 必須のストアテストは環境変数が無ければ自動スキップされる(既存の慣行に従う)
- コミットは各タスク末尾で行う。メッセージは `feat(matrix): ...` / `test(matrix): ...` 形式

---

### Task 1: DSL — MatrixDef 型・YAMLパース・バリデーション

**Files:**
- Modify: `internal/dsl/types.go`(MatrixDef 追加、Step/StepEntry に Matrix フィールド)
- Modify: `internal/dsl/parse.go`(validateStepFull 拡張)
- Test: `internal/dsl/parse_test.go`(追記)

**Interfaces:**
- Consumes: 既存 `ForeachSource`(UnmarshalYAML 実装済み)
- Produces: `dsl.MatrixDef{Dimensions []MatrixDimension; Exclude []map[string]string}`、`dsl.MatrixDimension{Name string; Source ForeachSource}`、`StepEntry.Matrix *MatrixDef` / `Step.Matrix *MatrixDef`(yaml: `matrix`)

- [ ] **Step 1: 失敗するテストを書く**

`internal/dsl/parse_test.go` に追記(既存テストのパース用ヘルパがあればそれに合わせる。無ければ `Parse([]byte(...))` 直呼び — 既存テストの流儀を最初に確認して同じ形式で書くこと):

```go
func TestParse_MatrixStep(t *testing.T) {
	yamlDoc := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: matrix-job
spec:
  steps:
    - name: build
      matrix:
        os: [linux, windows]
        arch: [amd64, arm64]
        exclude:
          - os: windows
            arch: arm64
      run: echo {{ .Matrix.os }}/{{ .Matrix.arch }}
`
	job, err := Parse([]byte(yamlDoc))
	require.NoError(t, err)
	m := job.Spec.Steps[0].Matrix
	require.NotNil(t, m)
	require.Len(t, m.Dimensions, 2)
	// 宣言順が保存されること
	require.Equal(t, "os", m.Dimensions[0].Name)
	require.Equal(t, []string{"linux", "windows"}, m.Dimensions[0].Source.Literal)
	require.Equal(t, "arch", m.Dimensions[1].Name)
	require.Equal(t, []map[string]string{{"os": "windows", "arch": "arm64"}}, m.Exclude)
}

func TestParse_MatrixValidation(t *testing.T) {
	cases := []struct {
		name    string
		snippet string // steps[0] の中身
		wantErr string
	}{
		{"foreachと同時指定", `
      matrix:
        os: [linux]
      foreach:
        key: x
        in: [a]
      run: echo`, "mutually exclusive"},
		{"次元ゼロ", `
      matrix: {}
      run: echo`, "at least one dimension"},
		{"excludeに未知の次元", `
      matrix:
        os: [linux]
        exclude:
          - arch: amd64
      run: echo`, "unknown dimension"},
		{"次元名が不正", `
      matrix:
        "os-name": [linux]
      run: echo`, "dimension name"},
		{"空のソース", `
      matrix:
        os: []
      run: echo`, "non-empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yamlDoc := `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: j
spec:
  steps:
    - name: s` + tc.snippet
			_, err := Parse([]byte(yamlDoc))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
```

注意: `Parse` のシグネチャ・エラー整形は既存の parse_test.go に合わせて調整すること(関数名が異なる場合はそれに従う)。「次元名が不正」は識別子規則 `^[A-Za-z_][A-Za-z0-9_]*$`(テンプレートで `.Matrix.name` とアクセスするため)。

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/dsl/ -run TestParse_Matrix -v`
Expected: FAIL(`Matrix` フィールド未定義のコンパイルエラー)

- [ ] **Step 3: 実装**

`internal/dsl/types.go` — `ForeachDef` の直後に追加。yaml.v3 の `*yaml.Node` 方式でマップの宣言順を保存する(import に `gopkg.in/yaml.v3` を追加):

```go
// MatrixDef expands a step into one copy per combination of dimension values
// (cartesian product minus exclude entries). Dimensions preserve YAML
// declaration order; the combination key joins values with "/" in that order
// (e.g. "linux/amd64").
type MatrixDef struct {
	Dimensions []MatrixDimension
	Exclude    []map[string]string
}

type MatrixDimension struct {
	Name   string
	Source ForeachSource
}

// UnmarshalYAML parses the matrix mapping while preserving key order.
// The reserved key "exclude" holds combination filters; every other key is a
// dimension whose value is a ForeachSource (list or expression string).
func (m *MatrixDef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("matrix must be a mapping of dimension name to a list or expression")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valNode := node.Content[i], node.Content[i+1]
		if keyNode.Value == "exclude" {
			if err := valNode.Decode(&m.Exclude); err != nil {
				return fmt.Errorf("matrix.exclude: %w", err)
			}
			continue
		}
		var src ForeachSource
		if err := valNode.Decode(&src); err != nil {
			return fmt.Errorf("matrix.%s: %w", keyNode.Value, err)
		}
		m.Dimensions = append(m.Dimensions, MatrixDimension{Name: keyNode.Value, Source: src})
	}
	return nil
}
```

※ `valNode.Decode(&src)` が旧式 `UnmarshalYAML(func(interface{}) error)` の ForeachSource で動くことはテストが検証する。もし動かなければ、valNode.Kind が SequenceNode なら `[]string` にデコードして Literal へ、ScalarNode なら Value を Expr へ、と手動分岐に切り替える。

`StepEntry` と `Step` の両方に(`Foreach` フィールドの直後):

```go
	Matrix           *MatrixDef            `yaml:"matrix,omitempty"`
```

`internal/dsl/parse.go` — `validateStepFull` にパラメータ `matrix *MatrixDef` を追加し(全呼び出し箇所 2 箇所に `st.Matrix` / `entry.Matrix` を渡す)、関数末尾の foreach 検証の後に:

```go
	if foreach != nil && matrix != nil {
		return fmt.Errorf("%s (%s): foreach and matrix are mutually exclusive", path, name)
	}
	if matrix != nil {
		if len(matrix.Dimensions) == 0 {
			return fmt.Errorf("%s (%s): matrix requires at least one dimension", path, name)
		}
		dimNames := map[string]bool{}
		for _, d := range matrix.Dimensions {
			if !matrixDimNameRe.MatchString(d.Name) {
				return fmt.Errorf("%s (%s): matrix dimension name %q must match %s", path, name, d.Name, matrixDimNameRe.String())
			}
			if dimNames[d.Name] {
				return fmt.Errorf("%s (%s): duplicate matrix dimension %q", path, name, d.Name)
			}
			dimNames[d.Name] = true
			if len(d.Source.Literal) == 0 && d.Source.Expr == "" {
				return fmt.Errorf("%s (%s): matrix.%s must be a non-empty list or expression", path, name, d.Name)
			}
		}
		for _, ex := range matrix.Exclude {
			for k := range ex {
				if !dimNames[k] {
					return fmt.Errorf("%s (%s): matrix.exclude references unknown dimension %q", path, name, k)
				}
			}
		}
	}
```

ファイル冒頭付近に(既存の regexp import を利用):

```go
var matrixDimNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/dsl/ -v`
Expected: PASS(既存テスト含め全緑)

- [ ] **Step 5: コミット**

```bash
git add internal/dsl/types.go internal/dsl/parse.go internal/dsl/parse_test.go
git commit -m "feat(matrix): add MatrixDef DSL type with ordered dimensions and validation"
```

---

### Task 2: DSL — EvalMatrix・出力集約型・テンプレート関数

**Files:**
- Create: `internal/dsl/matrix.go`
- Create: `internal/dsl/matrix_test.go`
- Modify: `internal/dsl/template.go`(TemplateData.Matrix、StepData.Outputs 型変更、keys/values 関数、OutputValueString)
- Modify: `internal/dsl/condition.go`(Outputs 型変更追従)
- Modify: `internal/dsl/template_test.go`, `internal/dsl/condition_test.go`(型変更追従)

**Interfaces:**
- Consumes: Task 1 の `MatrixDef` / `MatrixDimension`、既存 `EvalForeachSource(src ForeachSource, data TemplateData) ([]string, error)`
- Produces:
  - `dsl.MatrixCombo{Values map[string]string; Key string}`
  - `dsl.EvalMatrix(def MatrixDef, data TemplateData, maxCombos int) ([]MatrixCombo, error)`
  - `dsl.DefaultMatrixMaxCombinations = 64`
  - `dsl.TemplateData.Matrix map[string]string`(`{{ .Matrix.os }}` 用)
  - `dsl.StepData.Outputs map[string]any`(**型変更**: 従来 map[string]string)
  - `dsl.StringOutputs(m map[string]string) map[string]any`(変換ヘルパ)
  - `dsl.OutputValueString(v any) string`(string はそのまま、map[string]string は JSON)
  - テンプレート関数 `keys`(ソート済みキー列)/ `values`(キーソート順の値列)

- [ ] **Step 1: 失敗するテストを書く**

`internal/dsl/matrix_test.go`:

```go
package dsl

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func lit(items ...string) ForeachSource { return ForeachSource{Literal: items} }

func TestEvalMatrix_CartesianOrderAndKey(t *testing.T) {
	def := MatrixDef{Dimensions: []MatrixDimension{
		{Name: "os", Source: lit("linux", "windows")},
		{Name: "arch", Source: lit("amd64", "arm64")},
	}}
	combos, err := EvalMatrix(def, TemplateData{}, 0)
	require.NoError(t, err)
	keys := make([]string, len(combos))
	for i, c := range combos {
		keys[i] = c.Key
	}
	// 次元は宣言順、値はリスト順
	require.Equal(t, []string{"linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"}, keys)
	require.Equal(t, map[string]string{"os": "linux", "arch": "amd64"}, combos[0].Values)
}

func TestEvalMatrix_Exclude(t *testing.T) {
	def := MatrixDef{
		Dimensions: []MatrixDimension{
			{Name: "os", Source: lit("linux", "windows")},
			{Name: "arch", Source: lit("amd64", "arm64")},
		},
		Exclude: []map[string]string{{"os": "windows", "arch": "arm64"}},
	}
	combos, err := EvalMatrix(def, TemplateData{}, 0)
	require.NoError(t, err)
	require.Len(t, combos, 3)
	for _, c := range combos {
		require.NotEqual(t, "windows/arm64", c.Key)
	}
}

func TestEvalMatrix_ExcludePartialMatch(t *testing.T) {
	// 部分指定は一致する全組み合わせを除外(GHA互換)
	def := MatrixDef{
		Dimensions: []MatrixDimension{
			{Name: "os", Source: lit("linux", "windows")},
			{Name: "arch", Source: lit("amd64", "arm64")},
		},
		Exclude: []map[string]string{{"os": "windows"}},
	}
	combos, err := EvalMatrix(def, TemplateData{}, 0)
	require.NoError(t, err)
	require.Len(t, combos, 2)
}

func TestEvalMatrix_EmptyDimensionYieldsZeroCombos(t *testing.T) {
	def := MatrixDef{Dimensions: []MatrixDimension{
		{Name: "os", Source: ForeachSource{Expr: "{{ .Params.none }}"}},
	}}
	combos, err := EvalMatrix(def, TemplateData{Params: map[string]string{}}, 0)
	require.NoError(t, err)
	require.Empty(t, combos)
}

func TestEvalMatrix_SlashInValueRejected(t *testing.T) {
	def := MatrixDef{Dimensions: []MatrixDimension{{Name: "os", Source: lit("linux/gnu")}}}
	_, err := EvalMatrix(def, TemplateData{}, 0)
	require.ErrorContains(t, err, "must not contain")
}

func TestEvalMatrix_CapExceeded(t *testing.T) {
	def := MatrixDef{Dimensions: []MatrixDimension{
		{Name: "a", Source: lit("1", "2", "3")},
		{Name: "b", Source: lit("1", "2", "3")},
	}}
	_, err := EvalMatrix(def, TemplateData{}, 8)
	require.ErrorContains(t, err, "exceed")
}

func TestEvalMatrix_DynamicSource(t *testing.T) {
	def := MatrixDef{Dimensions: []MatrixDimension{
		{Name: "env", Source: ForeachSource{Expr: "$envs"}},
	}}
	combos, err := EvalMatrix(def, TemplateData{Params: map[string]string{"envs": `["dev","prod"]`}}, 0)
	require.NoError(t, err)
	require.Len(t, combos, 2)
	require.Equal(t, "dev", combos[0].Values["env"])
}

func TestOutputValueString(t *testing.T) {
	require.Equal(t, "plain", OutputValueString("plain"))
	require.Equal(t, `{"linux/amd64":"1.2"}`, OutputValueString(map[string]string{"linux/amd64": "1.2"}))
}

func TestTemplate_MatrixVarAndAggregatedOutputs(t *testing.T) {
	data := TemplateData{
		Matrix: map[string]string{"os": "linux"},
		Steps: map[string]StepData{
			"build": {Outputs: map[string]any{
				"version": map[string]string{"linux/amd64": "1.2", "linux/arm64": "1.3"},
			}},
		},
	}
	out, err := ExpandTemplate(`{{ .Matrix.os }}`, data)
	require.NoError(t, err)
	require.Equal(t, "linux", out)

	out, err = ExpandTemplate(`{{ index .Steps.build.Outputs.version "linux/arm64" }}`, data)
	require.NoError(t, err)
	require.Equal(t, "1.3", out)

	// keys はソート済み
	out, err = ExpandTemplate(`{{ keys .Steps.build.Outputs.version }}`, data)
	require.NoError(t, err)
	require.Equal(t, "[linux/amd64 linux/arm64]", out)
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/dsl/ -run 'TestEvalMatrix|TestOutputValueString|TestTemplate_Matrix' -v`
Expected: FAIL(EvalMatrix 未定義のコンパイルエラー)

- [ ] **Step 3: 実装**

`internal/dsl/matrix.go`(新規):

```go
package dsl

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultMatrixMaxCombinations is the cap applied when the controller does not
// supply one (or supplies 0).
const DefaultMatrixMaxCombinations = 64

// MatrixCombo is one expanded combination of a matrix step.
type MatrixCombo struct {
	Values map[string]string // dimension name → value
	Key    string            // values joined with "/" in dimension declaration order
}

// EvalMatrix resolves every dimension source, builds the cartesian product in
// declaration order, removes exclude matches, and enforces the combination cap.
// A dimension that resolves to an empty list yields zero combinations (skip).
// Values containing "/" are rejected because "/" is the key separator.
func EvalMatrix(def MatrixDef, data TemplateData, maxCombos int) ([]MatrixCombo, error) {
	if maxCombos <= 0 {
		maxCombos = DefaultMatrixMaxCombinations
	}
	if len(def.Dimensions) == 0 {
		return nil, fmt.Errorf("matrix has no dimensions")
	}
	combos := []MatrixCombo{{Values: map[string]string{}}}
	for _, dim := range def.Dimensions {
		items, err := EvalForeachSource(dim.Source, data)
		if err != nil {
			return nil, fmt.Errorf("matrix.%s: %w", dim.Name, err)
		}
		next := make([]MatrixCombo, 0, len(combos)*len(items))
		for _, c := range combos {
			for _, v := range items {
				if strings.Contains(v, "/") {
					return nil, fmt.Errorf("matrix.%s: value %q must not contain \"/\" (reserved as the combination key separator)", dim.Name, v)
				}
				values := make(map[string]string, len(c.Values)+1)
				for k, val := range c.Values {
					values[k] = val
				}
				values[dim.Name] = v
				key := v
				if c.Key != "" {
					key = c.Key + "/" + v
				}
				next = append(next, MatrixCombo{Values: values, Key: key})
			}
		}
		combos = next
	}
	filtered := combos[:0]
	for _, c := range combos {
		if !matchesAnyExclude(c.Values, def.Exclude) {
			filtered = append(filtered, c)
		}
	}
	if len(filtered) > maxCombos {
		return nil, fmt.Errorf("matrix expands to %d combinations, exceeding the limit of %d", len(filtered), maxCombos)
	}
	return filtered, nil
}

// matchesAnyExclude reports whether values matches at least one exclude entry
// (an entry matches when all of its key/value pairs equal the combination's).
func matchesAnyExclude(values map[string]string, exclude []map[string]string) bool {
	for _, ex := range exclude {
		if len(ex) == 0 {
			continue
		}
		all := true
		for k, v := range ex {
			if values[k] != v {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// StringOutputs converts a plain string output map to the any-typed form
// stored in StepData.Outputs.
func StringOutputs(m map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// OutputValueString renders a StepData output value as a string: plain strings
// pass through, aggregated matrix maps are JSON-encoded (stable key order via
// encoding/json), anything else falls back to fmt.
func OutputValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]string:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", v)
	}
}
```

`internal/dsl/template.go` の変更:

1. `TemplateData` に `Matrix map[string]string`(`Foreach` フィールドの直後、コメント付き):

```go
	Matrix  map[string]string // set during matrix/foreach step execution; key = dimension name
```

2. `StepData` を:

```go
// StepData holds the captured outputs of a previously executed step.
// Non-matrix steps store plain strings; matrix steps store an aggregated
// map[string]string keyed by combination key (e.g. "linux/amd64").
type StepData struct {
	Outputs map[string]any
}
```

3. `funcMap` に追加(import に `sort` は既にある):

```go
	// keys returns the sorted keys of a map (matrix aggregated outputs helper).
	"keys": func(m any) []string {
		var out []string
		switch t := m.(type) {
		case map[string]string:
			for k := range t {
				out = append(out, k)
			}
		case map[string]any:
			for k := range t {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	},
	// values returns map values ordered by sorted key.
	"values": func(m any) []string {
		var ks []string
		switch t := m.(type) {
		case map[string]string:
			for k := range t {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			out := make([]string, len(ks))
			for i, k := range ks {
				out[i] = t[k]
			}
			return out
		case map[string]any:
			for k := range t {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			out := make([]string, len(ks))
			for i, k := range ks {
				out[i] = OutputValueString(t[k])
			}
			return out
		}
		return nil
	},
```

`internal/dsl/condition.go` の変更(89-95行付近): `outputs := sd.Outputs; if outputs == nil { outputs = map[string]string{} }` の nil フォールバック型を合わせる:

```go
		outputs := sd.Outputs
		if outputs == nil {
			outputs = map[string]any{}
		}
```

コンパイルエラーになる既存テスト(`template_test.go`, `condition_test.go` の `StepData{Outputs: map[string]string{...}}` リテラル)は `map[string]any{...}` に機械的に書き換える。**アサーションの期待値は変えない。**

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/dsl/ -v`
Expected: PASS(全緑。既存の condition/template テストも含む)

- [ ] **Step 5: コミット**

```bash
git add internal/dsl/
git commit -m "feat(matrix): add EvalMatrix expansion, aggregated output types, keys/values template funcs"
```

---

### Task 3: API ワイヤ型とコントローラの claim 変換・上限設定

**Files:**
- Modify: `internal/api/types.go`
- Modify: `internal/controller/server.go`(Config に MatrixMaxCombinations)
- Modify: `internal/controller/api_agent.go`(buildOneClaimStep と parallel 変換、ClaimResponse への上限設定)
- Modify: `cmd/controller/main.go`(フラグ/env)
- Test: `internal/controller/api_agent_test.go`(追記)

**Interfaces:**
- Consumes: Task 1 の `dsl.MatrixDef`
- Produces(ワイヤ型 — Task 4/5/6 が依存):

```go
// internal/api/types.go
type ClaimMatrixDef struct {
	Dimensions []ClaimMatrixDimension `json:"dimensions"`
	Exclude    []map[string]string    `json:"exclude,omitempty"`
}
type ClaimMatrixDimension struct {
	Name   string             `json:"name"`
	Source ClaimForeachSource `json:"source"`
}
// ClaimStep: Foreach/ForeachKey/ForeachValue を削除し、以下に置換
//   Matrix       *ClaimMatrixDef   `json:"matrix,omitempty"`
//   MatrixValues map[string]string `json:"matrixValues,omitempty"`
//   MatrixKey    string            `json:"matrixKey,omitempty"`
// ClaimStep メソッド: func (s ClaimStep) DisplayName() string
// ClaimResponse: MatrixMaxCombinations int `json:"matrixMaxCombinations,omitempty"`
// StepReportRequest / StepReport: Variant string `json:"variant,omitempty"`
// controller.Config: MatrixMaxCombinations int
```

- [ ] **Step 1: 失敗するテストを書く**

`internal/controller/api_agent_test.go` に追記(既存の buildOneClaimStep 系テストの流儀に合わせる):

```go
func TestBuildClaimStep_MatrixAndForeachNormalization(t *testing.T) {
	// matrix はそのまま次元列に変換される
	entry := dsl.StepEntry{
		Name: "build",
		Run:  "echo",
		Matrix: &dsl.MatrixDef{
			Dimensions: []dsl.MatrixDimension{
				{Name: "os", Source: dsl.ForeachSource{Literal: []string{"linux", "windows"}}},
				{Name: "arch", Source: dsl.ForeachSource{Expr: "$archs"}},
			},
			Exclude: []map[string]string{{"os": "windows"}},
		},
	}
	cs := buildOneClaimStep(0, 0, entry)
	require.NotNil(t, cs.Matrix)
	require.Len(t, cs.Matrix.Dimensions, 2)
	require.Equal(t, "os", cs.Matrix.Dimensions[0].Name)
	require.Equal(t, []string{"linux", "windows"}, cs.Matrix.Dimensions[0].Source.Literal)
	require.Equal(t, "$archs", cs.Matrix.Dimensions[1].Source.Expr)
	require.Equal(t, []map[string]string{{"os": "windows"}}, cs.Matrix.Exclude)

	// foreach は1次元 matrix に正規化される
	fe := dsl.StepEntry{
		Name:    "deploy",
		Run:     "echo",
		Foreach: &dsl.ForeachDef{Key: "env", Source: dsl.ForeachSource{Literal: []string{"dev", "prod"}}},
	}
	cs = buildOneClaimStep(1, 1, fe)
	require.NotNil(t, cs.Matrix)
	require.Len(t, cs.Matrix.Dimensions, 1)
	require.Equal(t, "env", cs.Matrix.Dimensions[0].Name)
	require.Equal(t, []string{"dev", "prod"}, cs.Matrix.Dimensions[0].Source.Literal)
}

func TestClaimStep_DisplayName(t *testing.T) {
	s := api.ClaimStep{Name: "build"}
	require.Equal(t, "build", s.DisplayName())
	s.MatrixKey = "linux/amd64"
	require.Equal(t, "build (linux, amd64)", s.DisplayName())
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/controller/ -run 'TestBuildClaimStep_Matrix|TestClaimStep_DisplayName' -v`
Expected: FAIL(型未定義のコンパイルエラー)

- [ ] **Step 3: 実装**

`internal/api/types.go`:

1. `ClaimStep` から `Foreach *ClaimForeachDef`、`ForeachKey string`、`ForeachValue string` の3フィールドを削除し、同じ位置に:

```go
	Matrix           *ClaimMatrixDef       `json:"matrix,omitempty"`
	MatrixValues     map[string]string     `json:"matrixValues,omitempty"`
	MatrixKey        string                `json:"matrixKey,omitempty"`
```

2. `ClaimForeachDef` 型を削除し、代わりに(`ClaimForeachSource` は残す):

```go
// ClaimMatrixDef is the wire form of a matrix (or foreach, normalized to one
// dimension) definition. The agent expands it at runtime.
type ClaimMatrixDef struct {
	Dimensions []ClaimMatrixDimension `json:"dimensions"`
	Exclude    []map[string]string    `json:"exclude,omitempty"`
}

type ClaimMatrixDimension struct {
	Name   string             `json:"name"`
	Source ClaimForeachSource `json:"source"`
}
```

3. `ClaimStep` 定義の直後にメソッド(import に `strings` 追加):

```go
// DisplayName returns the human-facing step name: matrix copies get the
// combination appended, e.g. `build (linux, amd64)`. Safe because dimension
// values may not contain "/" (enforced at expansion).
func (s ClaimStep) DisplayName() string {
	if s.MatrixKey == "" {
		return s.Name
	}
	return s.Name + " (" + strings.ReplaceAll(s.MatrixKey, "/", ", ") + ")"
}
```

4. `ClaimResponse` に `MatrixMaxCombinations int `json:"matrixMaxCombinations,omitempty"`` を追加。
5. `StepReportRequest` と `StepReport` の両方に `Variant string `json:"variant,omitempty"`` を追加。

`internal/controller/server.go` — `Config` 構造体(22行目)に:

```go
	// MatrixMaxCombinations caps matrix step expansion; 0 means the default (64).
	MatrixMaxCombinations int
```

`internal/controller/api_agent.go`:

1. `buildOneClaimStep` の foreach 変換ブロック(237-245行)を置換:

```go
	if entry.Foreach != nil {
		cs.Matrix = &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{{
			Name:   entry.Foreach.Key,
			Source: api.ClaimForeachSource{Literal: entry.Foreach.Source.Literal, Expr: entry.Foreach.Source.Expr},
		}}}
	}
	if entry.Matrix != nil {
		dims := make([]api.ClaimMatrixDimension, len(entry.Matrix.Dimensions))
		for i, d := range entry.Matrix.Dimensions {
			dims[i] = api.ClaimMatrixDimension{
				Name:   d.Name,
				Source: api.ClaimForeachSource{Literal: d.Source.Literal, Expr: d.Source.Expr},
			}
		}
		cs.Matrix = &api.ClaimMatrixDef{Dimensions: dims, Exclude: entry.Matrix.Exclude}
	}
```

2. parallel 内ステップ(`dsl.Step`)→ClaimStep の変換箇所を探し(このファイル内で `stage.Parallel` / `entry.Parallel` を組み立てている場所。`buildOneClaimStep` を経由していなければ同じ Foreach/Matrix 変換を追加する。経由するよう Step→StepEntry 詰め替えで共通化しても良い)、**parallel 内の foreach/matrix も同じ変換が走ること**を確認する。
3. `api.ClaimResponse{` を構築している箇所(handleAgentClaim 内)で `MatrixMaxCombinations: s.cfg.MatrixMaxCombinations,` を設定。

`cmd/controller/main.go` — フラグ群(50行目付近)に追加し、Config へ渡す:

```go
	matrixMax := flag.Int("matrix-max-combinations", envIntOr("UNIFIED_MATRIX_MAX_COMBINATIONS", 64), "max combinations a matrix step may expand to (env: UNIFIED_MATRIX_MAX_COMBINATIONS)")
```

ファイル末尾にヘルパ:

```go
// envIntOr parses an integer environment variable, falling back to def when
// unset or malformed.
func envIntOr(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
```

`controller.Config{...}` を組み立てている箇所に `MatrixMaxCombinations: *matrixMax,` を追加(import に `strconv`)。

**この時点で internal/agent / internal/k8sagent はコンパイルエラーになる**(ForeachKey 等の削除)。Task 5/6 で直すので、このタスクのテストはパッケージを絞って実行する。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/api/ ./internal/controller/ ./internal/dsl/ -v`
Expected: PASS(controller のテストが PG 必須でスキップされる場合は build が通ることまで確認)

- [ ] **Step 5: コミット**

```bash
git add internal/api/types.go internal/controller/ cmd/controller/main.go
git commit -m "feat(matrix): wire types (ClaimMatrixDef, MatrixValues/MatrixKey, Variant), claim conversion, combination cap config"
```

---

### Task 4: ストア — variant 列マイグレーションと報告/出力の複合キー化

**Files:**
- Create: `internal/store/migrations/003_matrix_variant.up.sql`
- Create: `internal/store/migrations/003_matrix_variant.down.sql`
- Modify: `internal/store/postgres.go`(UpsertStepReport, SetStepOutput, GetRunSteps)
- Modify: `internal/controller/api_agent.go`(handleAgentStepReport / handleAgentSetStepOutputs が variant を透過)
- Modify: 呼び出し側のコンパイル追従(controller 内の全 UpsertStepReport / SetStepOutput 呼び出し)
- Test: `internal/store/postgres_test.go` 相当(既存のストアテストファイルに追記。無ければ `internal/controller/api_agent_test.go` の PG 使用テストに追記)

**Interfaces:**
- Consumes: Task 3 の `StepReportRequest.Variant` / `StepReport.Variant`
- Produces:
  - `UpsertStepReport(ctx, runID string, stepIndex, stageIndex int, stepName, variant, status string, exitCode *int, startedAt, endedAt *time.Time) error`
  - `SetStepOutput(ctx, runID string, stepIndex int, variant, key, value string) error`
  - `GetRunSteps` は variant 昇順を第2ソートキーにし `StepReport.Variant` を埋める
  - HTTP: `POST .../steps/{stepIndex}/outputs?variant=<key>`(クエリパラメータ。未指定は空文字)

- [ ] **Step 1: 失敗するテストを書く**

既存の PG 統合テストの流儀(セットアップヘルパ)に合わせて追記:

```go
func TestStepReports_MatrixVariantsDoNotClobber(t *testing.T) {
	pg := newTestStore(t) // 既存ヘルパ名に合わせる
	runID := createTestRun(t, pg)

	ec := 0
	now := time.Now().UTC()
	require.NoError(t, pg.UpsertStepReport(ctx, runID, 0, 0, "build (linux, amd64)", "linux/amd64", "Succeeded", &ec, &now, &now))
	require.NoError(t, pg.UpsertStepReport(ctx, runID, 0, 0, "build (linux, arm64)", "linux/arm64", "Running", nil, &now, nil))

	steps, err := pg.GetRunSteps(ctx, runID)
	require.NoError(t, err)
	require.Len(t, steps, 2) // 同一 step_index でも variant 別に2行
	require.Equal(t, "linux/amd64", steps[0].Variant)
	require.Equal(t, "linux/arm64", steps[1].Variant)

	// 同一 (run, index, variant) は upsert
	require.NoError(t, pg.UpsertStepReport(ctx, runID, 0, 0, "build (linux, arm64)", "linux/arm64", "Succeeded", &ec, &now, &now))
	steps, err = pg.GetRunSteps(ctx, runID)
	require.NoError(t, err)
	require.Len(t, steps, 2)
}

func TestStepOutputs_VariantKeyed(t *testing.T) {
	pg := newTestStore(t)
	runID := createTestRun(t, pg)
	require.NoError(t, pg.SetStepOutput(ctx, runID, 0, "linux/amd64", "version", "1.2"))
	require.NoError(t, pg.SetStepOutput(ctx, runID, 0, "linux/arm64", "version", "1.3"))
	// 衝突しない(2行保存される)ことを確認 — GetStepOutputs は従来シグネチャのまま
}
```

※ ヘルパ名(newTestStore / createTestRun)は既存テストの実名に置き換えること。

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/store/ ./internal/controller/ -run 'Variant' -v`
Expected: FAIL(シグネチャ不一致のコンパイルエラー)

- [ ] **Step 3: 実装**

`internal/store/migrations/003_matrix_variant.up.sql`:

```sql
ALTER TABLE public.step_reports ADD COLUMN variant text NOT NULL DEFAULT '';
ALTER TABLE public.step_reports DROP CONSTRAINT step_reports_pkey;
ALTER TABLE public.step_reports ADD CONSTRAINT step_reports_pkey PRIMARY KEY (run_id, step_index, variant);

ALTER TABLE public.step_outputs ADD COLUMN variant text NOT NULL DEFAULT '';
ALTER TABLE public.step_outputs DROP CONSTRAINT step_outputs_pkey;
ALTER TABLE public.step_outputs ADD CONSTRAINT step_outputs_pkey PRIMARY KEY (run_id, step_index, variant, key);
```

`internal/store/migrations/003_matrix_variant.down.sql`:

```sql
DELETE FROM public.step_reports WHERE variant <> '';
ALTER TABLE public.step_reports DROP CONSTRAINT step_reports_pkey;
ALTER TABLE public.step_reports ADD CONSTRAINT step_reports_pkey PRIMARY KEY (run_id, step_index);
ALTER TABLE public.step_reports DROP COLUMN variant;

DELETE FROM public.step_outputs WHERE variant <> '';
ALTER TABLE public.step_outputs DROP CONSTRAINT step_outputs_pkey;
ALTER TABLE public.step_outputs ADD CONSTRAINT step_outputs_pkey PRIMARY KEY (run_id, step_index, key);
ALTER TABLE public.step_outputs DROP COLUMN variant;
```

`internal/store/postgres.go`:

```go
func (p *Postgres) UpsertStepReport(ctx context.Context, runID string, stepIndex int, stageIndex int, stepName, variant, status string, exitCode *int, startedAt, endedAt *time.Time) error {
	const q = `
		INSERT INTO step_reports(run_id, step_index, variant, stage_index, step_name, status, exit_code, started_at, ended_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (run_id, step_index, variant) DO UPDATE
		  SET stage_index = EXCLUDED.stage_index,
		      step_name   = EXCLUDED.step_name,
		      status      = EXCLUDED.status,
		      exit_code   = COALESCE(EXCLUDED.exit_code, step_reports.exit_code),
		      started_at  = COALESCE(EXCLUDED.started_at, step_reports.started_at),
		      ended_at    = COALESCE(EXCLUDED.ended_at, step_reports.ended_at);
	`
	_, err := p.pool.Exec(ctx, q, runID, stepIndex, variant, stageIndex, stepName, status, exitCode, startedAt, endedAt)
	return err
}
```

`SetStepOutput` に variant を追加(挿入列と ON CONFLICT キーを `(run_id, step_index, variant, key)` に)。`GetStepOutputs` はシグネチャ据え置きで `ORDER BY variant` を付けるだけ(variant 違いの同名キーは最後の行が勝つ — 従来 foreach の挙動と同等で退行なし)。

`GetRunSteps`: SELECT に variant を加え `ORDER BY step_index, variant`、`api.StepReport.Variant` に詰める。

`internal/controller/api_agent.go`:
- `handleAgentStepReport`: `req.Variant` を UpsertStepReport に渡す。
- `handleAgentSetStepOutputs`: `variant := r.URL.Query().Get("variant")` を SetStepOutput に渡す。
- その他の UpsertStepReport 呼び出し(承認 reaper、StuckRunReaper 等で使っていれば)は variant に `""` を渡す。`grep -rn "UpsertStepReport" internal/` で全箇所を潰す。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/store/ ./internal/controller/ -v`
Expected: PASS(PG が無い環境ではスキップ。その場合 `go vet ./...` でコンパイルを確認)

- [ ] **Step 5: コミット**

```bash
git add internal/store/ internal/controller/
git commit -m "feat(matrix): store step reports/outputs keyed by (run, step, variant)"
```

---

### Task 5: 標準エージェント — matrix展開(parallel内含む)・出力集約・報告

**Files:**
- Modify: `internal/agent/pipeline.go`(展開の一般化 + parallel内バグ修正 + setStepMatrixOutputs)
- Delete: `internal/agent/foreach.go`(ExpandMatrixStep に置換)
- Create: `internal/agent/matrix.go`
- Modify: `internal/agent/agent.go`(tplData.Matrix、DisplayName/Variant 報告、出力集約、SetStepOutputs 変更)
- Modify: `internal/agent/client.go`(SetStepOutputs に variant)
- Test: `internal/agent/pipeline_test.go`(foreach テストを matrix に書き換え+parallel内展開テスト追加)、`internal/agent/client_test.go` 追従

**Interfaces:**
- Consumes: Task 2 `dsl.EvalMatrix`/`StringOutputs`/`OutputValueString`、Task 3 ワイヤ型、Task 4 `?variant=` クエリ
- Produces:
  - `agent.ExpandMatrixStep(step api.ClaimStep, data dsl.TemplateData, maxCombos int) ([]api.ClaimStep, error)`(k8s-agent が Task 6 で使う。**exported**)
  - `RunPipeline(ctx, stages, getData, maxCombos int, run)`(maxCombos パラメータ追加)
  - `Client.SetStepOutputs(ctx, agentID, runID string, stepIndex int, variant string, outputs map[string]string) error`

- [ ] **Step 1: 失敗するテストを書く**

`internal/agent/pipeline_test.go` — 既存の foreach テスト2本(115行・140行付近)を Matrix 形に書き換え、parallel 内展開テストを追加:

```go
func TestRunPipeline_MatrixExpansion(t *testing.T) {
	stages := []api.ClaimStage{{Step: &api.ClaimStep{
		Name: "build",
		Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
			{Name: "os", Source: api.ClaimForeachSource{Literal: []string{"linux", "windows"}}},
			{Name: "arch", Source: api.ClaimForeachSource{Literal: []string{"amd64", "arm64"}}},
		}},
	}}}
	var mu sync.Mutex
	var keys []string
	err := RunPipeline(t.Context(), stages, emptyData, 0, func(_ context.Context, s api.ClaimStep) error {
		mu.Lock()
		defer mu.Unlock()
		keys = append(keys, s.MatrixKey)
		require.Equal(t, s.MatrixValues["os"]+"/"+s.MatrixValues["arch"], s.MatrixKey)
		return nil
	})
	require.NoError(t, err)
	sort.Strings(keys)
	require.Equal(t, []string{"linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"}, keys)
}

func TestRunPipeline_MatrixInsideParallelExpands(t *testing.T) {
	// 従来バグ: parallel 内の foreach/matrix が展開されず1回だけ実行されていた
	stages := []api.ClaimStage{{Parallel: []api.ClaimStep{
		{Name: "plain", Run: "echo"},
		{Name: "fanout", Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
			{Name: "env", Source: api.ClaimForeachSource{Literal: []string{"dev", "stg", "prod"}}},
		}}},
	}}}
	var mu sync.Mutex
	count := map[string]int{}
	err := RunPipeline(t.Context(), stages, emptyData, 0, func(_ context.Context, s api.ClaimStep) error {
		mu.Lock()
		defer mu.Unlock()
		count[s.Name]++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, count["plain"])
	require.Equal(t, 3, count["fanout"])
}

func TestRunPipeline_MatrixCapFailsRun(t *testing.T) {
	stages := []api.ClaimStage{{Step: &api.ClaimStep{
		Name: "big",
		Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
			{Name: "a", Source: api.ClaimForeachSource{Literal: []string{"1", "2", "3"}}},
		}},
	}}}
	err := RunPipeline(t.Context(), stages, emptyData, 2, func(_ context.Context, _ api.ClaimStep) error { return nil })
	require.ErrorContains(t, err, "exceed")
}

func TestSafeStepCtx_MatrixOutputAggregation(t *testing.T) {
	sctx := &safeStepCtx{data: dsl.TemplateData{Steps: map[string]dsl.StepData{}}}
	sctx.setStepMatrixOutputs("build", "linux/amd64", map[string]string{"version": "1.2"})
	sctx.setStepMatrixOutputs("build", "linux/arm64", map[string]string{"version": "1.3"})
	snap := sctx.snapshot()
	agg, ok := snap.Steps["build"].Outputs["version"].(map[string]string)
	require.True(t, ok)
	require.Equal(t, map[string]string{"linux/amd64": "1.2", "linux/arm64": "1.3"}, agg)
}
```

既存 `$envs` 参照の動的 foreach テスト(旧140行付近)は 1 次元 Matrix + `Expr: "$envs"` の形に書き換えて残す(動的ソースのカバレッジ維持)。`emptyData`(19行)はそのまま使う。

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/agent/ -run TestRunPipeline -v`
Expected: FAIL(コンパイルエラー: RunPipeline の引数、ClaimMatrixDef など)

- [ ] **Step 3: 実装**

`internal/agent/foreach.go` を削除し、`internal/agent/matrix.go` を新規作成:

```go
package agent

import (
	"fmt"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// ExpandMatrixStep expands a matrix-bearing ClaimStep into one copy per
// combination (MatrixValues/MatrixKey set, Matrix cleared). Non-matrix steps
// are returned as a single-element slice unchanged. Shared by the standard
// agent pipeline and the k8s agent orchestrator.
func ExpandMatrixStep(step api.ClaimStep, data dsl.TemplateData, maxCombos int) ([]api.ClaimStep, error) {
	if step.Matrix == nil {
		return []api.ClaimStep{step}, nil
	}
	def := dsl.MatrixDef{Exclude: step.Matrix.Exclude}
	for _, d := range step.Matrix.Dimensions {
		def.Dimensions = append(def.Dimensions, dsl.MatrixDimension{
			Name:   d.Name,
			Source: dsl.ForeachSource{Literal: d.Source.Literal, Expr: d.Source.Expr},
		})
	}
	combos, err := dsl.EvalMatrix(def, data, maxCombos)
	if err != nil {
		return nil, fmt.Errorf("matrix expansion for step %q: %w", step.Name, err)
	}
	out := make([]api.ClaimStep, len(combos))
	for i, c := range combos {
		s := step
		s.Matrix = nil
		s.MatrixValues = c.Values
		s.MatrixKey = c.Key
		out[i] = s
	}
	return out, nil
}
```

`internal/agent/pipeline.go` — `RunPipeline` を置換(旧 foreach 分岐を削除。**parallel 内も展開する**):

```go
// RunPipeline executes stages sequentially. Within a stage, matrix expansion
// applies to the single step and to every member of a parallel group; all
// resulting copies run concurrently. When a stage fails, subsequent stages are
// not executed.
func RunPipeline(
	ctx context.Context,
	stages []api.ClaimStage,
	getData func() dsl.TemplateData,
	maxCombos int,
	run func(ctx context.Context, step api.ClaimStep) error,
) error {
	for _, stage := range stages {
		members := stage.Parallel
		if stage.Step != nil {
			members = []api.ClaimStep{*stage.Step}
		}
		var expanded []api.ClaimStep
		for _, st := range members {
			ex, err := ExpandMatrixStep(st, getData(), maxCombos)
			if err != nil {
				return err
			}
			expanded = append(expanded, ex...)
		}
		// Preserve the historical single-step path (no goroutine) for a plain
		// non-matrix single-step stage.
		if stage.Step != nil && stage.Step.Matrix == nil {
			if err := runOne(ctx, expanded[0], run); err != nil {
				return err
			}
			continue
		}
		if err := runParallel(ctx, expanded, run); err != nil {
			return err
		}
	}
	return nil
}
```

同ファイルに集約メソッドを追加(コピーオンライトで snapshot との競合を避ける — 内側マップは毎回作り直す):

```go
// setStepMatrixOutputs merges one matrix copy's outputs into the aggregated
// per-combination map under the base step name. Inner maps are rebuilt on
// every write so snapshots never observe concurrent mutation.
func (s *safeStepCtx) setStepMatrixOutputs(name, comboKey string, outputs map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Steps == nil {
		s.data.Steps = make(map[string]dsl.StepData)
	}
	sd := s.data.Steps[name]
	if sd.Outputs == nil {
		sd.Outputs = map[string]any{}
	}
	for k, v := range outputs {
		merged := map[string]string{comboKey: v}
		if prev, ok := sd.Outputs[k].(map[string]string); ok {
			for pk, pv := range prev {
				merged[pk] = pv
			}
			merged[comboKey] = v
		}
		sd.Outputs[k] = merged
	}
	s.data.Steps[name] = sd
}
```

`internal/agent/agent.go` の run コールバック内:

1. `RunPipeline` 呼び出しに maxCombos を渡す: claim レスポンス `c.MatrixMaxCombinations` をそのまま渡す(0 なら EvalMatrix 側でデフォルト 64)。finally 側の RunPipeline 呼び出しも同様。
2. 392-394行の Foreach 分岐を置換:

```go
			if step.MatrixValues != nil {
				tplData.Matrix = step.MatrixValues
				tplData.Foreach = step.MatrixValues // foreach シュガー互換: {{ .Foreach.key }}
			}
```

3. **全 `ReportStep` 呼び出し**(Running/Succeeded/Failed/Skipped/Cancelled/WaitingApproval、承認・cache・artifact 含む)で `StepName: step.Name` → `StepName: step.DisplayName()` にし、`Variant: step.MatrixKey` を追加する。`grep -n "StepName:" internal/agent/agent.go` で全箇所を潰す。
4. 出力の保存(465行付近)を分岐:

```go
					if step.MatrixKey != "" {
						sctx.setStepMatrixOutputs(step.Name, step.MatrixKey, capturedOutputs)
					} else {
						sctx.setStep(step.Name, dsl.StepData{Outputs: dsl.StringOutputs(capturedOutputs)})
					}
					if len(capturedOutputs) > 0 {
						_ = a.Client.SetStepOutputs(stepCtx, a.ID, c.RunID, step.Index, step.MatrixKey, capturedOutputs)
					}
```

call ステップの出力(402-405行)は `dsl.StringOutputs(childOutputs)` を挟み、SetStepOutputs に `step.MatrixKey` を渡す。
5. ジョブ出力の昇格(finalData から run outputs を作る箇所、561行以降)で `sd.Outputs[name]` が `any` になるため `dsl.OutputValueString(val)` を通して文字列化する。
6. 出力キャプチャ用 outputCtx(450行付近)に `Matrix: tplData.Matrix, Foreach: tplData.Foreach` を追加(matrixコピー内の outputs: テンプレートで `{{ .Matrix.os }}` を使えるように)。

`internal/agent/client.go` — `SetStepOutputs` に variant パラメータを追加し、空でなければ `?variant=` クエリを付与:

```go
// SetStepOutputs sends the step outputs to the master server. variant is the
// matrix combination key ("" for non-matrix steps).
func (c *Client) SetStepOutputs(ctx context.Context, agentID, runID string, stepIndex int, variant string, outputs map[string]string) error {
	u := fmt.Sprintf("%s/api/v1/agents/%s/runs/%s/steps/%d/outputs", c.BaseURL, agentID, runID, stepIndex)
	if variant != "" {
		u += "?variant=" + url.QueryEscape(variant)
	}
	// 以下は既存実装の POST 部分をそのまま流用
	...
}
```

(既存実装の URL 構築方法に合わせて調整。`net/url` を import。)`client_test.go:95` の呼び出しは `variant ""` を追加。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/agent/ -v`
Expected: PASS

- [ ] **Step 5: コミット**

```bash
git add internal/agent/
git commit -m "feat(matrix): agent-side matrix expansion (incl. inside parallel blocks), aggregated outputs, variant reporting"
```

---

### Task 6: k8s エージェント — 展開・集約の追従

**Files:**
- Modify: `internal/k8sagent/agent.go`
- Test: `internal/k8sagent/orchestrate_test.go`(foreach テストを matrix 形に更新)

**Interfaces:**
- Consumes: Task 5 の `agentlib.ExpandMatrixStep`、Task 3 ワイヤ型、Task 2 の `dsl.StringOutputs`/`OutputValueString`

- [ ] **Step 1: 失敗するテストを書く**

`internal/k8sagent/orchestrate_test.go:212` の `ClaimForeachDef` 使用箇所を新形式に書き換え:

```go
			Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
				{Name: "item", Source: api.ClaimForeachSource{Literal: []string{"a", "b"}}},
			}},
```

さらに2次元展開のテストを追加(既存のオーケストレーションテストのモック流儀に合わせ、実行されたステップ名/回数を検証する。期待値: `build (x, 1)` など4回、report の Variant が `x/1` 形式)。

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/k8sagent/ -v`
Expected: FAIL(コンパイルエラー)

- [ ] **Step 3: 実装**

`internal/k8sagent/agent.go`:

1. main ループの foreach 分岐(474-493行)を置換:

```go
		for _, step := range api.StageSteps(stage) {
			data := dsl.TemplateData{Params: c.Params, Steps: stepCtx.Steps}
			variants, err := agentlib.ExpandMatrixStep(step, data, c.MatrixMaxCombinations)
			if err != nil {
				slog.Error("k8s: matrix expansion failed", "step", step.Name, "error", err)
				anyStepFailed.Store(true)
				continue
			}
			for _, v := range variants {
				mainRun(v)
			}
		}
```

2. finally ループ(543-561行)も同形に置換(エラー時は `finallyFailed.Store(true)`)。
3. 286-288行の Foreach 分岐を置換:

```go
			if step.MatrixValues != nil {
				tplData.Matrix = step.MatrixValues
				tplData.Foreach = step.MatrixValues
			}
```

4. 出力保存(442-443行): matrix コピーは集約(stepCtx.Steps は単一 goroutine アクセスなので直接マージでよい):

```go
				if step.MatrixKey != "" {
					sd := stepCtx.Steps[step.Name]
					if sd.Outputs == nil {
						sd.Outputs = map[string]any{}
					}
					for k, v := range capturedOutputs {
						m, _ := sd.Outputs[k].(map[string]string)
						if m == nil {
							m = map[string]string{}
						}
						m[step.MatrixKey] = v
						sd.Outputs[k] = m
					}
					stepCtx.Steps[step.Name] = sd
				} else {
					stepCtx.Steps[step.Name] = dsl.StepData{Outputs: dsl.StringOutputs(capturedOutputs)}
				}
				_ = a.client.SetStepOutputs(ctx, a.cfg.AgentID, c.RunID, step.Index, step.MatrixKey, capturedOutputs)
```

5. ジョブ出力昇格(500-507行): `runOutputs[outName] = dsl.OutputValueString(val)` に変更。
6. `ReportStep` を発行している箇所(makeRunStep 内部ほか)を `step.DisplayName()` + `Variant: step.MatrixKey` に変更(`grep -n "StepName:" internal/k8sagent/agent.go`)。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/k8sagent/ -v`
Expected: PASS

- [ ] **Step 5: リポジトリ全体のビルドとテスト**

Run: `go build ./... ; go test ./...`
Expected: 全パッケージ PASS(PG 必須テストはスキップ可)。CLI(internal/cli)に ForeachKey 参照が残っていればここで露見するので追従修正する。

- [ ] **Step 6: コミット**

```bash
git add internal/k8sagent/ internal/cli/
git commit -m "feat(matrix): k8s-agent matrix expansion and aggregated outputs"
```

---

### Task 7: ドキュメント・サンプル・仕上げ

**Files:**
- Modify: `docs/jobs.md`(matrix セクション追加、foreach をシュガーとして再記述)
- Modify: `docs/agents.md`(全台同時更新の注意)
- Create: `examples/jobs/matrix.yaml`
- Modify: `TODO.md`

**Interfaces:** なし(ドキュメントのみ)

- [ ] **Step 1: サンプルを書く**

`examples/jobs/matrix.yaml`:

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: matrix-build
spec:
  steps:
    - name: build
      matrix:
        os: [linux, windows, darwin]
        arch: [amd64, arm64]
        exclude:
          - os: windows
            arch: arm64
      outputs:
        built: "{{ .Matrix.os }}-{{ .Matrix.arch }}"
      run: |
        echo "building for {{ .Matrix.os }}/{{ .Matrix.arch }}"
    - name: report
      run: |
        echo "built variants: {{ keys .Steps.build.Outputs.built }}"
```

- [ ] **Step 2: サンプルが apply を通ることを検証**

Run: `go run ./cmd/... の apply 相当は不要 — ユニットで代替`: `internal/dsl/parse_test.go` に `examples/jobs/matrix.yaml` を読み込んで Parse が通るテストを1本追加(既存に examples を読むテストの前例があればその形式に従う。無ければ `os.ReadFile("../../examples/jobs/matrix.yaml")`)。
Expected: PASS

- [ ] **Step 3: docs/jobs.md に matrix セクションを追加**

追加内容(既存の foreach セクションの直後。トーンと見出しレベルは前後に合わせる):
- `matrix:` の構文(次元、リテラル/式ソース、exclude)
- 組み合わせキーの正規形(宣言順 `/` 結合)と値に `/` を使えない制約
- 上限(デフォルト64、`--matrix-max-combinations` / `UNIFIED_MATRIX_MAX_COMBINATIONS`)
- 出力の集約セマンティクス: `{{ .Steps.NAME.Outputs.KEY }}` はマップになる、`keys`/`values`/`index` の使い方、CEL からは `steps.NAME.outputs.KEY["linux/amd64"]`
- foreach は1次元 matrix のシュガーである旨、同時指定はエラーである旨
- 空次元 → 0組み合わせ → ステップ未実行(ラン成功)
- ジョブ outputs に matrix 出力を昇格すると JSON 文字列になる旨

- [ ] **Step 4: docs/agents.md に互換性注意を追加**

「アップグレード」相当の箇所(無ければ末尾)に: matrix 対応リリースでは claim ワイヤ形式が変わるため、**コントローラとエージェント(標準/k8s とも)を同時に更新すること**。旧エージェントは foreach/matrix ステップを展開できない。

- [ ] **Step 5: TODO.md の更新**

「TODO 対応レビューで発見」セクション等に parallel内foreach非展開バグの項目があれば「matrix実装で解消済み」と追記(項目が無ければ何もしない)。仕様書 `docs/superpowers/specs/2026-07-05-matrix-design.md` のステータス行を「実装済み」に更新。

- [ ] **Step 6: 最終確認とコミット**

Run: `go build ./... ; go vet ./... ; go test ./...`
Expected: 全緑

```bash
git add docs/ examples/ TODO.md internal/dsl/parse_test.go
git commit -m "docs(matrix): document matrix steps, add example, note agent upgrade requirement"
```

---

## セルフレビュー記録(計画作成時)

- 仕様カバレッジ: DSL構文(T1)、展開セマンティクス+parallel内バグ(T2/T5/T6)、出力集約(T2/T4/T5/T6)、ワイヤ形式(T3)、UI表示=DisplayName埋め込みでUI変更不要(T3/T5/T6)、バリデーション(T1)、上限設定(T2/T3)、docs(T7)— 全セクションに対応タスクあり
- 型整合: `ExpandMatrixStep` / `setStepMatrixOutputs` / `SetStepOutputs(…, variant, …)` / `UpsertStepReport(…, variant, …)` のシグネチャは各タスクの Interfaces 節に明記し相互参照済み
- 既知の許容: `GetStepOutputs` はシグネチャ据え置き(variant 違いは最後勝ち — 従来 foreach と同等)。人間向け出力APIの variant 対応は本計画のスコープ外
