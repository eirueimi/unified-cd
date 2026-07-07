# ウィンドウド・ログビューア 実装プラン

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** WebUI のログビューを「全量配列+truncate」から「サーバー範囲取得+単一連続ウィンドウ」に置換し、任意サイズのログの全域閲覧とサーバー全文検索を実現する(spec: docs/superpowers/specs/2026-07-07-windowed-log-viewer-design.md)。

**Architecture:** サーバーに stats/range/search の3エンドポイント(ROW_NUMBER ベースの行番号アドレッシング)を追加し、`/steps/{i}/logs` を廃止。フロントは `logLines` 全量配列を `logWindow {startRow, lines, totalCount}` に置換、仮想スクローラの全高を totalCount で張り、可視域が窓外に出たら range で窓を差し替える。SSE は無変更(バックフィル=初期窓、ライブは stick 時のみ窓へ append)。

**Tech Stack:** Go 1.26+ / chi / pgx(実PGテスト、Docker稼働中) / Svelte + vitest(jsdom、`@rolldown/binding-win32-x64-msvc` が消えていたら `npm install --no-save` で再導入)。

## Global Constraints

- 行アドレスは「対象ビュー内の 0-based 行番号」(seq はグローバル連番で非連続のため UI アドレスに使わない)。
- range の limit 上限 10,000。ウィンドウ上限 `WINDOW_MAX = 30000` 行、1回の取得 `FETCH_CHUNK = 5000` 行。ウィンドウは常に**単一の連続区間**。
- 検索 matches cap = 1,000(`total` は真の件数)。ILIKE メタ文字(`%` `_` `\`)はエスケープ。空 `q` は 400。
- steps パラメータはカンマ区切り整数列(`steps=0,2`)。不正値は 400。
- 既存挙動の維持: tail 追従/上スクロール解除/フィルタ変更時末尾ジャンプ(31f2f67)/空フィルタ文言(e424037 の Loading・No log lines)。SSE サーバーコードは無変更。
- jsdom テストの既知制約: scrollHeight スタブは固定 4000、コンポーネントはマウント時に末尾へジャンプするため、描画assertには窓オフセット(約185行)を超える行数のフィクスチャが必要(RunDetail.test.js 内の既存コメント参照)。
- 各タスクのゲート: `go build ./... && go vet ./internal/store/ ./internal/controller/ && go test -count=1 ./internal/store/ ./internal/controller/`(Go 変更時)+ `cd web && npx vitest run`(web 変更時)。
- コミット末尾: `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`

---

### Task 1: store — CountLogs / ListLogsRange / SearchLogs

**Files:**
- Modify: `internal/store/store.go`(interface、`TailLogs` 群の近く)
- Modify: `internal/store/postgres.go`(`TailLogsRecent` の近く)
- Test: `internal/store/postgres_taillogs_test.go`(既存ファイルに追記)

**Interfaces:**
- Produces(後続タスクが依存する正確なシグネチャ):

```go
// CountLogs returns the number of log lines (and the min/max seq) for the
// run, optionally restricted to the given step indexes (nil/empty = all).
CountLogs(ctx context.Context, runID string, steps []int) (count, minSeq, maxSeq int64, err error)
// ListLogsRange returns `limit` lines starting at 0-based row `offset` in
// seq order, optionally restricted to steps. Row numbering is per-view:
// with steps set, offset 0 is the view's first line.
ListLogsRange(ctx context.Context, runID string, steps []int, offset, limit int) ([]api.LogLine, error)
// LogSearchMatch locates one search hit: Row is the 0-based row number
// within the same view ListLogsRange addresses.
type LogSearchMatch struct { Row int64 `json:"row"`; Seq int64 `json:"seq"`; StepIndex int `json:"stepIndex"` }
// SearchLogs returns up to `capN` case-insensitive substring matches plus the
// TOTAL match count (which may exceed capN). q is a raw substring; ILIKE
// metacharacters are escaped internally.
SearchLogs(ctx context.Context, runID string, steps []int, q string, capN int) (total int64, matches []LogSearchMatch, err error)
```

- [ ] **Step 1: 失敗するテストを書く**(postgres_taillogs_test.go に追記)

```go
func TestPostgres_LogsRangeStatsSearch(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	other, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	now := time.Now().UTC()
	// 他 run の行が混入しないことの検証用
	_, err = pg.AppendLog(ctx, other.ID, 0, "stdout", now, "other-run-line")
	require.NoError(t, err)
	// step0: 3行 / step2: 5行(うち1行に "needle_50%" — ILIKE メタ文字入り)
	for i := 0; i < 3; i++ {
		_, err := pg.AppendLog(ctx, run.ID, 0, "stdout", now, fmt.Sprintf("zero-%d", i))
		require.NoError(t, err)
	}
	for i := 0; i < 5; i++ {
		line := fmt.Sprintf("two-%d", i)
		if i == 3 {
			line = "found needle_50% here"
		}
		_, err := pg.AppendLog(ctx, run.ID, 2, "stdout", now, line)
		require.NoError(t, err)
	}

	// --- CountLogs ---
	count, minSeq, maxSeq, err := pg.CountLogs(ctx, run.ID, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 8, count)
	assert.Less(t, minSeq, maxSeq)
	count, _, _, err = pg.CountLogs(ctx, run.ID, []int{2})
	require.NoError(t, err)
	assert.EqualValues(t, 5, count)
	count, _, _, err = pg.CountLogs(ctx, run.ID, []int{7})
	require.NoError(t, err)
	assert.EqualValues(t, 0, count)

	// --- ListLogsRange(全体ビュー: 行番号は step0 の3行が 0..2、step2 が 3..7)---
	lines, err := pg.ListLogsRange(ctx, run.ID, nil, 2, 3)
	require.NoError(t, err)
	require.Len(t, lines, 3)
	assert.Equal(t, "zero-2", lines[0].Line)
	assert.Equal(t, "two-0", lines[1].Line)
	// steps ビュー: offset はビュー内アドレス
	lines, err = pg.ListLogsRange(ctx, run.ID, []int{2}, 0, 2)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	assert.Equal(t, "two-0", lines[0].Line)
	// 末尾越え offset は空
	lines, err = pg.ListLogsRange(ctx, run.ID, nil, 100, 10)
	require.NoError(t, err)
	assert.Empty(t, lines)

	// --- SearchLogs(ILIKE メタ文字がリテラル扱いされること)---
	total, matches, err := pg.SearchLogs(ctx, run.ID, nil, "needle_50%", 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, matches, 1)
	assert.EqualValues(t, 6, matches[0].Row) // 全体ビューで 0-based 7行目(zero×3 + two-0..2 の次)
	assert.Equal(t, 2, matches[0].StepIndex)
	// "_" はリテラル: "needle_" は1件、ワイルドカードとして "needleX" 等を拾わない
	total, _, err = pg.SearchLogs(ctx, run.ID, nil, "zero-", 2) // cap < total
	require.NoError(t, err)
	assert.EqualValues(t, 3, total)
	total, matches, err = pg.SearchLogs(ctx, run.ID, []int{0}, "o", 100) // steps ビュー内 Row
	require.NoError(t, err)
	require.NotEmpty(t, matches)
	assert.EqualValues(t, 0, matches[0].Row)
	_ = total
}
```

- [ ] **Step 2: RED 確認** — `go test ./internal/store/ -run TestPostgres_LogsRangeStatsSearch -v` → undefined メソッドでビルド失敗。
- [ ] **Step 3: 実装**(postgres.go、TailLogsRecent の直後)

```go
// logsStepFilter renders the optional step_index filter shared by the
// windowed-viewer queries. steps nil/empty = no filter.
// Returns the SQL fragment and the arg (or nil).
func logsStepFilter(steps []int) (string, any) {
	if len(steps) == 0 {
		return "", nil
	}
	return " AND step_index = ANY($2)", steps
}

func (p *Postgres) CountLogs(ctx context.Context, runID string, steps []int) (count, minSeq, maxSeq int64, err error) {
	frag, arg := logsStepFilter(steps)
	q := `SELECT COUNT(*), COALESCE(MIN(seq),0), COALESCE(MAX(seq),0) FROM logs WHERE run_id = $1` + frag
	args := []any{runID}
	if arg != nil {
		args = append(args, arg)
	}
	err = p.pool.QueryRow(ctx, q, args...).Scan(&count, &minSeq, &maxSeq)
	return count, minSeq, maxSeq, err
}

func (p *Postgres) ListLogsRange(ctx context.Context, runID string, steps []int, offset, limit int) ([]api.LogLine, error) {
	frag, arg := logsStepFilter(steps)
	args := []any{runID}
	n := 2
	if arg != nil {
		args = append(args, arg)
		n = 3
	}
	q := fmt.Sprintf(`SELECT seq, step_index, stream, ts, line FROM logs
		WHERE run_id = $1%s ORDER BY seq OFFSET $%d LIMIT $%d`, frag, n, n+1)
	args = append(args, offset, limit)
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.LogLine
	for rows.Next() {
		var l api.LogLine
		if err := rows.Scan(&l.Seq, &l.StepIndex, &l.Stream, &l.Timestamp, &l.Line); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// escapeILIKE makes q a literal ILIKE substring pattern.
func escapeILIKE(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

func (p *Postgres) SearchLogs(ctx context.Context, runID string, steps []int, q string, capN int) (int64, []LogSearchMatch, error) {
	frag, arg := logsStepFilter(steps)
	args := []any{runID}
	n := 2
	if arg != nil {
		args = append(args, arg)
		n = 3
	}
	// Row numbers are computed over the VIEW (same ordering/filter as
	// ListLogsRange) BEFORE the match filter, so they are addressable rows.
	sql := fmt.Sprintf(`
		SELECT COUNT(*) OVER (), rn - 1, seq, step_index FROM (
			SELECT seq, step_index, line, ROW_NUMBER() OVER (ORDER BY seq) AS rn
			FROM logs WHERE run_id = $1%s
		) v WHERE line ILIKE $%d ESCAPE '\' ORDER BY seq LIMIT $%d`, frag, n, n+1)
	args = append(args, escapeILIKE(q), capN)
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var total int64
	var out []LogSearchMatch
	for rows.Next() {
		var m LogSearchMatch
		if err := rows.Scan(&total, &m.Row, &m.Seq, &m.StepIndex); err != nil {
			return 0, nil, err
		}
		out = append(out, m)
	}
	return total, out, rows.Err()
}
```

interface(store.go、TailLogsRecentByStep の宣言位置)に上記3メソッド+`LogSearchMatch` 型を追加。**注意**: `COUNT(*) OVER ()` は LIMIT 適用前の全マッチ数を返すこと(ウィンドウ関数は LIMIT 前に評価される)を Step 1 の cap<total ケースが検証する。
- [ ] **Step 4: GREEN + ゲート**
- [ ] **Step 5: Commit** — `feat(store): windowed log queries (count/range/search by row number)`

---

### Task 2: controller — stats/range/search エンドポイント + 旧 /steps/{i}/logs 廃止

**Files:**
- Modify: `internal/controller/api_runs.go`(`handleStepLogs` を置換)
- Modify: `internal/controller/server.go:276-277` 付近(ルート差し替え)
- Modify: `internal/controller/api_steplogs_test.go`(全面書き換え → `api_logwindow_test.go` にリネーム)

**Interfaces:**
- Consumes: Task 1 の 3 store メソッド。
- Produces(フロントが叩く HTTP 契約):
  - `GET /api/v1/runs/{id}/logs/stats?steps=0,2` → `{"count":8,"minSeq":101,"maxSeq":109}`
  - `GET /api/v1/runs/{id}/logs/range?offset=2&limit=3&steps=0,2` → `[api.LogLine…]`(limit 省略時 1000、上限 10000、offset 省略時 0、負値は 400)
  - `GET /api/v1/runs/{id}/logs/search?q=needle&steps=0,2` → `{"total":37,"matches":[{"row":123,"seq":456,"stepIndex":2}]}`(matches cap 1000、空/欠落 q は 400)
  - steps パラメータ解析の共通ヘルパー: `parseStepsParam(r *http.Request) ([]int, error)`(空文字列 → nil、非整数 → error)。

- [ ] **Step 1: 失敗するテストを書く** — `api_steplogs_test.go` を `api_logwindow_test.go` へ `git mv` し、`TestAPI_StepLogs` を削除して置換:

```go
// TestAPI_LogWindow covers the three windowed-viewer endpoints end to end
// (real PG + router): stats, ranged fetch by view row number, and server
// search with ILIKE-literal semantics.
func TestAPI_LogWindow(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		_, _ = pg.AppendLog(ctx, run.ID, 0, "stdout", now, fmt.Sprintf("zero-%d", i))
	}
	for i := 0; i < 5; i++ {
		_, _ = pg.AppendLog(ctx, run.ID, 2, "stdout", now, fmt.Sprintf("two-%d", i))
	}

	getJSON := func(path string, into any) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), into))
		}
		return rec.Code
	}

	var stats struct{ Count, MinSeq, MaxSeq int64 }
	require.Equal(t, http.StatusOK, getJSON("/api/v1/runs/"+run.ID+"/logs/stats", &stats))
	assert.EqualValues(t, 8, stats.Count)
	require.Equal(t, http.StatusOK, getJSON("/api/v1/runs/"+run.ID+"/logs/stats?steps=2", &stats))
	assert.EqualValues(t, 5, stats.Count)

	var lines []api.LogLine
	require.Equal(t, http.StatusOK, getJSON("/api/v1/runs/"+run.ID+"/logs/range?offset=2&limit=3", &lines))
	require.Len(t, lines, 3)
	assert.Equal(t, "zero-2", lines[0].Line)
	require.Equal(t, http.StatusOK, getJSON("/api/v1/runs/"+run.ID+"/logs/range?steps=2&limit=2", &lines))
	assert.Equal(t, "two-0", lines[0].Line)
	assert.Equal(t, http.StatusBadRequest, getJSON("/api/v1/runs/"+run.ID+"/logs/range?offset=-1", &lines))
	assert.Equal(t, http.StatusBadRequest, getJSON("/api/v1/runs/"+run.ID+"/logs/range?steps=abc", &lines))

	var sr struct {
		Total   int64                  `json:"total"`
		Matches []store.LogSearchMatch `json:"matches"`
	}
	require.Equal(t, http.StatusOK, getJSON("/api/v1/runs/"+run.ID+"/logs/search?q=two-", &sr))
	assert.EqualValues(t, 5, sr.Total)
	assert.EqualValues(t, 3, sr.Matches[0].Row)
	assert.Equal(t, http.StatusBadRequest, getJSON("/api/v1/runs/"+run.ID+"/logs/search", &sr))

	// 旧エンドポイントは消えている
	code := getJSON("/api/v1/runs/"+run.ID+"/steps/0/logs", &lines)
	assert.Equal(t, http.StatusNotFound, code)
}
```

- [ ] **Step 2: RED 確認**(新ルート 404 / 旧ルートがまだ 200)
- [ ] **Step 3: 実装** — api_runs.go の `handleStepLogs` を削除し、以下に置換。server.go のルートを差し替え:

```go
r.With(view).Get("/runs/{id}/logs/stats", s.handleLogStats)
r.With(view).Get("/runs/{id}/logs/range", s.handleLogRange)
r.With(view).Get("/runs/{id}/logs/search", s.handleLogSearch)
// (旧 "/runs/{id}/steps/{stepIndex}/logs" 行は削除)
```

ハンドラ(api_runs.go):

```go
// parseStepsParam parses the optional comma-separated steps=0,2 view filter.
func parseStepsParam(r *http.Request) ([]int, error) {
	raw := r.URL.Query().Get("steps")
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("invalid steps value %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *Server) handleLogStats(w http.ResponseWriter, r *http.Request) {
	steps, err := parseStepsParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	count, minSeq, maxSeq, err := s.store.CountLogs(r.Context(), chi.URLParam(r, "id"), steps)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"count": count, "minSeq": minSeq, "maxSeq": maxSeq})
}

func (s *Server) handleLogRange(w http.ResponseWriter, r *http.Request) {
	steps, err := parseStepsParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, limit := 0, 1000
	if v := r.URL.Query().Get("offset"); v != "" {
		if offset, err = strconv.Atoi(v); err != nil || offset < 0 {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit <= 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
	}
	if limit > 10000 {
		limit = 10000
	}
	lines, err := s.store.ListLogsRange(r.Context(), chi.URLParam(r, "id"), steps, offset, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if lines == nil {
		lines = []api.LogLine{}
	}
	writeJSON(w, http.StatusOK, lines)
}

func (s *Server) handleLogSearch(w http.ResponseWriter, r *http.Request) {
	steps, err := parseStepsParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	total, matches, err := s.store.SearchLogs(r.Context(), chi.URLParam(r, "id"), steps, q, 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if matches == nil {
		matches = []store.LogSearchMatch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "matches": matches})
}
```

同時に store 側の `TailLogsRecentByStep`(postgres.go・store.go・postgres_taillogs_test.go の該当テスト)を削除(range+stats に包含)。
- [ ] **Step 4: GREEN + ゲート**(`grep -rn "TailLogsRecentByStep\|handleStepLogs" internal/` が空であること)
- [ ] **Step 5: Commit** — `feat(controller): windowed log endpoints (stats/range/search); retire per-step logs`

---

### Task 3: web — ウィンドウド・データモデル本体(all ビュー)

**Files:**
- Modify: `web/src/routes/RunDetail.svelte`(ログデータ層の書き換え — 本プラン最大)
- Modify: `web/src/routes/RunDetail.test.js`(全量配列前提のテストの改修+新テスト)

**Interfaces:**
- Consumes: Task 2 の stats/range HTTP 契約。
- Produces(Task 4/5 が依存する内部構造):

```js
let logView = { steps: null };  // null=all | number[](Task 4 で使用)
let logWindow = { startRow: 0, lines: [], totalCount: 0 };
let windowLoading = false;      // range 取得中(スピナー行の表示にも使用)
const WINDOW_MAX = 30000;
const FETCH_CHUNK = 5000;
async function refreshStats()                 // stats を叩き totalCount を更新
async function ensureRowsLoaded(firstRow, lastRow) // 可視域が窓外なら range で窓差し替え
function viewStepsQuery()                     // logView.steps → "&steps=0,2" | ""
```

**変更マップ(既存リアクティブ変数の意味変更)** — 実装者は現物を読み替えて適用:

| 旧 | 新 |
|---|---|
| `logLines`(全量) | `logWindow.lines`(窓)— SSE append・merge 箇所を全て置換 |
| `filteredLogs = logLines.filter(...)` | **廃止**(Task 4 でビュー切替に置換。本タスクでは `filteredLogs = logWindow.lines` の恒等に縮退させ、selectedStep 分岐は Task 4 まで既存クライアントフィルタを温存してよい — ただし窓が全量でない旨のテスト改修は本タスクで行う) |
| `logTotal = filteredLogs.length` | `logTotal = logWindow.totalCount` |
| `logStart/logEnd`(配列 index) | **絶対行番号**(0..totalCount)。`visibleLogs = logWindow.lines.slice(clamp(logStart - logWindow.startRow), clamp(logEnd - logWindow.startRow))` |
| `logTopPad = logStart * ROW_H` | 同じ式のまま(絶対行番号なので自然に全高を張る)。`logBotPad = (logTotal - logEnd) * ROW_H` も同じ |
| truncated バナー(`logTruncated`) | **表示廃止**(変数は SSE 初期窓の startRow 計算に利用: バックフィル前に `refreshStats()` を await し、`startRow = totalCount - backfillLines.length`。stats 失敗時は `totalCount = backfillLines.length`・`startRow = 0` にフォールバック — 既存テストが stats モック無しでも通る根拠) |
| wrap モードの `logOffsets`(全量の累積) | **窓内のみ**の累積オフセットに変更(`buildLogOffsets(logWindow.lines, cols)`)。窓外の全高・スクロール位置は `LOG_ROW_H` 近似で張り、range 取得後に再アンカー(spec の v1 既知制限)。topPad は `(logStart - logWindow.startRow)` 窓内オフセット+`logWindow.startRow * LOG_ROW_H` 近似の和 |

**新設関数の実装(このまま使用、変数配線は現物に合わせる):**

```js
async function refreshStats() {
  try {
    const s = await apiFetch(`/api/v1/runs/${runID}/logs/stats?_=${Date.now()}${viewStepsQuery()}`);
    logWindow = { ...logWindow, totalCount: s.count };
  } catch (e) { console.warn("log stats failed", e); }
}

let windowFetchToken = 0;
async function ensureRowsLoaded(firstRow, lastRow) {
  const w = logWindow;
  if (firstRow >= w.startRow && lastRow <= w.startRow + w.lines.length) return; // 窓内
  if (windowLoading) return; // 取得中の連打防止(完了後の再スクロールで再判定される)
  const token = ++windowFetchToken;
  windowLoading = true;
  try {
    const center = Math.floor((firstRow + lastRow) / 2);
    const start = Math.max(0, center - Math.floor(FETCH_CHUNK / 2));
    const lines = await apiFetch(
      `/api/v1/runs/${runID}/logs/range?offset=${start}&limit=${FETCH_CHUNK}${viewStepsQuery()}`);
    if (token !== windowFetchToken) return; // ビュー切替等で無効化
    logWindow = { ...logWindow, startRow: start,
      lines: lines.map((l) => ({ ...l, line: collapseCarriageReturns(l.line) })) };
  } catch (e) { console.warn("log range fetch failed", e);
  } finally { if (token === windowFetchToken) windowLoading = false; }
}
```

- スクロール連動: `onLogScroll`(または logStart/logEnd のリアクティブ)から `ensureRowsLoaded(logStart, logEnd)` を fire-and-forget で呼ぶ。
- SSE 側: `startSSE` 冒頭で `await refreshStats()`。バックフィルバッチ確定時に `logWindow = { startRow: Math.max(0, totalCount - batch.length), lines: batch, totalCount }`。ライブ行は「窓が末尾に接している(startRow+lines.length >= totalCount)」場合のみ append(+`WINDOW_MAX` 超過分を先頭 evict し startRow を進める)、それ以外は `totalCount++` のみ。
- 窓外スクロール中の表示: `visibleLogs` が空で `windowLoading` の間、log-box 内に `Loading…` 行(既存の空フィルタ文言ブロックを流用し `windowLoading` を優先)。

- [ ] **Step 1: 既存テスト改修方針の適用+新テスト(RED)** — RunDetail.test.js:
  - fetch モックに `/logs/stats`(`{count: N, minSeq:1, maxSeq:N}`)と `/logs/range` ハンドラを追加するヘルパー `statsAndRange(totalCount, makeLine)` を定義(既存 describe 群で再利用)。
  - 既存テスト(tail 系・filter 系・backfill 系)は stats を返さないとバックフィル処理が…実装で stats 失敗時は `totalCount = lines.length` へフォールバックさせるため、**既存テストは無改修で通ることを目標**にする(フォールバック実装が Global Constraints の既存挙動維持に対応)。
  - 新テスト1 `scrolling above the window fetches an earlier range`: stats count=50000、SSE バックフィルは末尾 200 行(seq/行番号 49800..)。マウント後 `box.scrollTop = 0` にして scroll イベントを dispatch → `/logs/range?offset=0` 近傍が fetch され、`logWindow` 差し替えで先頭行が描画される(`.log-row` の textContent に行 0 のマーカー)。
  - 新テスト2 `live lines while scrolled up only grow the total`: 実行中 run、非 stick 状態で SSE 追加行 → range fetch は発生せず、スクロールバー全高(スペーサー高さ or logTotal 反映)だけが伸びる。
- [ ] **Step 2: RED 確認**
- [ ] **Step 3: 実装**(上記変更マップ+新設関数)
- [ ] **Step 4: GREEN + `npx vitest run` 全体**
- [ ] **Step 5: Commit** — `feat(web): windowed log viewer core — ranged fetch over full scroll extent`

---

### Task 4: web — ステップ/グループ選択のビュー切替化

**Files:**
- Modify: `web/src/routes/RunDetail.svelte`
- Modify: `web/src/routes/RunDetail.test.js`

**Interfaces:**
- Consumes: Task 3 の `logView`/`refreshStats`/`ensureRowsLoaded`/`viewStepsQuery`。

**実装内容:**
- `selectedStep`/`selectedParallelGroup` の変更を `logView.steps` へ反映(`null`→all、単一→`[idx]`、グループ→indices)。切替時: `windowFetchToken++`(在庫 fetch 無効化)→ `refreshStats()` → 末尾窓を range で取得(`offset = max(0, count - FETCH_CHUNK)`)→ 末尾へジャンプ(31f2f67 の挙動維持)。
- `filteredLogs` のクライアントフィルタと `e424037` の per-step 補充(`backfillSelectedStepLogs`/`fetchedSteps`)を**削除**(ビュー切替が正: 窓の中身はサーバー側で既にフィルタ済み)。`stepLogsLoading` は `windowLoading` に統合。
- SSE ライブ行: ステップビュー中は `l.stepIndex` がビューに含まれる行のみ窓 append / totalCount++ 対象(含まれない行は無視 — all ビューの totalCount はビュー復帰時の refreshStats で追いつく)。
- 空フィルタ文言: `windowLoading` → Loading… / それ以外で 0 行 → No log lines for this selection.(既存文言踏襲)。
- テスト: 既存 `backfills a truncated-away step from the server when selected` を**ビュー切替版に書き換え**(step 選択 → `/logs/stats?steps=0` と `/logs/range?...steps=0` が呼ばれ、そのレスポンス行が描画される)。`jumps to the bottom when a step filter is applied` は維持(モックに stats/range for steps を追加)。

- [ ] **Step 1: テスト書き換え(RED)** → **Step 2: 実装** → **Step 3: GREEN+全体** → **Step 4: Commit** — `feat(web): step/group selection is a server-side log view`

---

### Task 5: web — サーバー検索統合

**Files:**
- Modify: `web/src/routes/RunDetail.svelte`(検索ブロック 171-213 行付近の置換)
- Modify: `web/src/routes/RunDetail.test.js`

**Interfaces:**
- Consumes: Task 2 の search 契約、Task 3 の `ensureRowsLoaded`。

**実装内容:**
- `logQuery` 入力を 300ms デバウンスして `GET /logs/search?q=...${viewStepsQuery()}` を実行。結果 `{total, matches}` を保持(`logMatches` は matches の row 配列に置換)。ビュー切替・SSE 再接続で再検索。
- n/N ジャンプ(`gotoMatch`): `matches[pos].row` へ `logBox.scrollTop = row * LOG_ROW_H - clientHeight/2`(logStick=false は現行維持)→ スクロール連動の `ensureRowsLoaded` が窓を取得。
- ハイライト: 現行の行内 `highlightSegments` は維持(窓内の行にのみ適用される — 意味は不変)。`curMatchRow` は絶対行番号比較(`logStart + i === curMatchRow` の式は絶対行番号化により `logWindow.startRow + slice index` と一致させる — Task 3 の visibleLogs 定義に合わせて添字を調整)。
- カウンタ表示: `{pos+1} / {total}`、`total > matches.length` のとき `1,000+ 件のうち先頭1,000件` 相当の注記(`{matches.length}+`)。
- 空クエリはサーバーを叩かず状態クリア。検索エラーは console.warn+カウンタ 0。
- テスト: (1) 入力→デバウンス後に `/logs/search` が正しい q・steps で呼ばれる、(2) Enter ジャンプで scrollTop が `match.row * LOG_ROW_H` 近傍になり range fetch が連鎖する、(3) cap 表示(`total=1500, matches=1000` モック)。

- [ ] **Step 1: テスト(RED)** → **Step 2: 実装** → **Step 3: GREEN+全体** → **Step 4: Commit** — `feat(web): server-side full-log search with jump-to-match`

---

### Task 6: 後始末 — truncated UI 残滓・docs・TODO

**Files:**
- Modify: `web/src/routes/RunDetail.svelte`(truncated バナー(709-714 行付近)と `logTruncated` の表示用途を削除 — SSE イベント自体は無視してよい)
- Modify: `internal/controller/sse.go`(`sseBackfillLimit` のコメントに「クライアントはウィンドウドビューアで全域閲覧可能、バックフィルは初期窓」と追記 — 挙動不変)
- Modify: `docs/operations.md` / `docs/cli.md` の「truncate されたら CLI で全量」記述を「UI で全域閲覧可能(スクロール)。一括取得は CLI」に更新(grep `truncated`/`unified-cli logs` で該当箇所を特定)
- Modify: `TODO.md` — 本機能の完了メモ(該当 TODO があれば。無ければ「検証済みで正常」節への追記は不要、変更履歴として spec 参照のみ)
- Modify: `docs/superpowers/specs/2026-07-07-windowed-log-viewer-design.md` — ステータス「実装済み」

- [ ] **Step 1: 実施+最終ゲート** — `go build ./... && go vet ./... && go test -count=1 ./internal/store/ ./internal/controller/ && cd web && npx vitest run` 全緑
- [ ] **Step 2: Commit** — `chore: retire truncation UX; docs for windowed log viewer`
