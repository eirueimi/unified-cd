<script>
  import { onDestroy, tick } from "svelte";
  import { push } from "svelte-spa-router";
  import { get } from "svelte/store";
  import AuthSetup from "../components/AuthSetup.svelte";
  import { apiFetch, token, serverURL, stderrPlain } from "../lib/api.js";
  import { statusBadge, fmtTime, collapseCarriageReturns } from "../lib/utils.js";

  export let params;
  $: runID = params.id;

  let run = null,
    steps = [],
    approvals = [],
    artifacts = [],
    loading = true,
    error = "";
  let logBox,
    selectedStep = null,
    selectedParallelGroup = null,
    abortController = null,
    stepsTimer = null;
  let approvalComments = {};

  // ---- Windowed log data model (Task 3) ----
  // The client no longer holds the full log in memory. `logWindow` is a
  // single contiguous slice [startRow, startRow+lines.length) of the current
  // view's rows (0-based, absolute row numbers within that view), fetched
  // on demand from the server as the user scrolls. `logView.steps` selects
  // which view (null = all steps; a single step or parallel group selects a
  // server-filtered view — see `switchLogView` below, driven by
  // selectedStep/selectedParallelGroup). `WINDOW_MAX`/`FETCH_CHUNK` bound how
  // much is held/fetched at once.
  let logView = { steps: null };
  let logWindow = { startRow: 0, lines: [], totalCount: 0 };
  let windowLoading = false; // range fetch in flight (drives the Loading… row)
  // True only while switchLogView's body (refreshStats + tail range fetch) is
  // in flight — NOT during an ordinary same-view ensureRowsLoaded fetch. The
  // SSE reader uses this (not windowLoading) to decide whether to drop a live
  // batch outright: during a view switch `logWindow` may still hold the OLD
  // view's contents while `logView.steps` already points at the new one, so
  // appending would corrupt it. A plain scroll-driven ensureRowsLoaded fetch
  // never changes logView, so live batches must keep counting into
  // totalCount while it's in flight (see the else-branch below).
  let viewSwitching = false;
  const WINDOW_MAX = 30000;
  const FETCH_CHUNK = 5000;

  // logView.steps → "&steps=0,2" (or "" for the all-steps view).
  function viewStepsQuery() {
    return logView.steps && logView.steps.length
      ? `&steps=${logView.steps.join(",")}`
      : "";
  }

  async function refreshStats() {
    try {
      const s = await apiFetch(
        `/api/v1/runs/${runID}/logs/stats?_=${Date.now()}${viewStepsQuery()}`,
      );
      // Test fixtures (and any other endpoint returning 200 with an
      // unrelated JSON body) may not have `.count` as a number — treat that
      // the same as a failed stats fetch so the SSE-backfill-length fallback
      // below still kicks in, instead of poisoning totalCount with NaN.
      if (typeof s?.count === "number" && Number.isFinite(s.count)) {
        logWindow = { ...logWindow, totalCount: s.count };
      }
    } catch (e) {
      console.warn("log stats failed", e);
    }
  }

  let windowFetchToken = 0;
  async function ensureRowsLoaded(firstRow, lastRow) {
    const w = logWindow;
    if (firstRow >= w.startRow && lastRow <= w.startRow + w.lines.length) return; // already in window
    if (windowLoading) return; // fetch already in flight; re-checked after it settles
    const token = ++windowFetchToken;
    windowLoading = true;
    const center = Math.floor((firstRow + lastRow) / 2);
    const start = Math.max(0, center - Math.floor(FETCH_CHUNK / 2));
    let settledOK = false; // only the SUCCESS path re-checks the viewport
    try {
      const lines = await apiFetch(
        `/api/v1/runs/${runID}/logs/range?offset=${start}&limit=${FETCH_CHUNK}${viewStepsQuery()}`,
      );
      if (token !== windowFetchToken) return; // superseded (view switch, etc.)
      logWindow = {
        ...logWindow,
        startRow: start,
        lines: lines.map((l) => ({ ...l, line: collapseCarriageReturns(l.line) })),
      };
      settledOK = true;
    } catch (e) {
      console.warn("log range fetch failed", e);
      // Do NOT re-check on the catch path. A failed fetch left the window
      // unchanged, so an unconditional re-check would recompute the identical
      // request and refire it immediately → throw → re-check… a tight,
      // zero-backoff request storm against a failing/offline server. Leaving
      // it be restores the pre-fix recovery semantics: a later user scroll
      // re-triggers the fetch via the reactive `$: ensureRowsLoaded`.
    } finally {
      if (token === windowFetchToken) {
        windowLoading = false;
        // Re-check the CURRENT viewport now that a SUCCESSFUL fetch has
        // settled: while it was in flight, a scroll may have moved
        // [logStart, logEnd) to a position the just-installed window doesn't
        // cover, and that scroll's own ensureRowsLoaded was early-returned by
        // the `windowLoading` guard above — nothing else will re-fire it (a
        // finished run has no SSE appends to rescue the viewport). Re-invoke
        // against the window as it stands NOW, but only if the recomputed
        // request would make FORWARD PROGRESS — a different `start` than the
        // one that just settled. If the viewport is still uncovered yet the
        // next request would be the IDENTICAL fetch that just landed (e.g. an
        // overstated totalCount whose range returns 0 rows so the window never
        // covers the viewport), refiring it changes nothing and would loop
        // forever; bail instead. When the viewport IS covered (the steady
        // state, e.g. a view smaller than FETCH_CHUNK the server returned
        // whole) the in-window early-return inside the recursive call
        // terminates it normally.
        if (settledOK) {
          const rc = Math.floor((logStart + logEnd) / 2);
          const nextStart = Math.max(0, rc - Math.floor(FETCH_CHUNK / 2));
          const covered =
            logStart >= logWindow.startRow &&
            logEnd <= logWindow.startRow + logWindow.lines.length;
          if (covered || nextStart !== start) ensureRowsLoaded(logStart, logEnd);
        }
      }
    }
  }

  $: stepSections = (() => {
    const bySection = { main: [], finally: [], sidecars: [] };
    for (const s of steps) {
      if (s.kind === "sidecar") bySection.sidecars.push(s);
      else (s.section === "finally" ? bySection.finally : bySection.main).push(s);
    }
    const group = (arr) => {
      const map = new Map();
      for (const s of arr) {
        if (!map.has(s.stageIndex)) map.set(s.stageIndex, []);
        map.get(s.stageIndex).push(s);
      }
      return [...map.entries()]
        .sort(([a], [b]) => a - b)
        .map(([stageIndex, stageSteps]) => ({ stageIndex, steps: stageSteps }));
    };
    const out = [{ section: "main", label: "Steps", groups: group(bySection.main) }];
    if (bySection.finally.length)
      out.push({ section: "finally", label: "Finally", groups: group(bySection.finally) });
    if (bySection.sidecars.length)
      out.push({ section: "sidecars", label: "Sidecars", groups: [{ stageIndex: 0, steps: bySection.sidecars }] });
    return out;
  })();

  // ---- Virtualized log rendering ----
  // Logs can reach tens of thousands of lines (e.g. Unity's `-logFile -`), which
  // freezes the browser if every line becomes a DOM node. We render only the
  // rows inside the scroll viewport (plus a small overscan) using fixed-height
  // rows and top/bottom spacer divs, so the scrollbar still reflects the full log.
  // Since Task 3, `logStart`/`logEnd` are ABSOLUTE row numbers (0..logTotal)
  // over the current view, not indices into an in-memory array — the actual
  // rows come from `logWindow`, a server-fetched slice that may not cover
  // [logStart, logEnd) yet (see `ensureRowsLoaded`).
  const LOG_ROW_H = 20; // px — must match .log-row height in <style>
  const LOG_OVERSCAN = 15; // extra rows rendered above and below the viewport
  let logScrollTop = 0;
  let logViewportH = 600;
  let logStick = true; // keep auto-scrolling to the bottom while the user is there

  // Wrap long lines instead of scrolling horizontally. Wrapping makes rows a
  // VARIABLE height, so the virtual scroller switches from a fixed row height to
  // per-line estimated heights (monospace char count / columns) with cumulative
  // offsets. The choice is persisted so it sticks across runs.
  let logWrap = false;
  try {
    logWrap = localStorage.getItem("ecd_log_wrap") === "1";
  } catch {}
  $: persistLogWrap(logWrap);
  function persistLogWrap(w) {
    try {
      localStorage.setItem("ecd_log_wrap", w ? "1" : "0");
    } catch {}
  }
  let logCharW = 7.7; // measured monospace char width (px)
  let logInnerW = 800; // log-box content width (px), minus padding
  $: logCols = Math.max(20, Math.floor(logInnerW / logCharW));

  $: logTotal = logWindow.totalCount;
  // In wrap mode: cumulative pixel offsets WITHIN THE WINDOW ONLY —
  // offs[i] = top of window-relative line i (null otherwise). The full
  // scroll extent outside the window is approximated with LOG_ROW_H (v1
  // known limitation, per spec); a range fetch re-anchors it once it lands.
  $: logOffsets = logWrap ? buildLogOffsets(logWindow.lines, logCols) : null;
  $: logContentH = logTotal * LOG_ROW_H;
  $: logStart = logWrap
    ? Math.max(0, logWindow.startRow + offsetIndex(logOffsets, logScrollTop - logWindow.startRow * LOG_ROW_H) - LOG_OVERSCAN)
    : Math.max(0, Math.floor(logScrollTop / LOG_ROW_H) - LOG_OVERSCAN);
  $: logEnd = logWrap
    ? Math.min(
        logTotal,
        logWindow.startRow + offsetIndex(logOffsets, logScrollTop + logViewportH - logWindow.startRow * LOG_ROW_H) + 1 + LOG_OVERSCAN,
      )
    : Math.min(
        logTotal,
        Math.ceil((logScrollTop + logViewportH) / LOG_ROW_H) + LOG_OVERSCAN,
      );
  // Clamp the absolute [logStart, logEnd) request down to what's actually
  // materialized in `logWindow.lines` — the window may not cover the full
  // visible range yet (a range fetch is in flight, fired below).
  // `visibleLogsSliceStart` is the actual slice() start index used below; the
  // template derives each rendered line's ABSOLUTE row number from
  // `logWindow.startRow + visibleLogsSliceStart + i` (NOT `logStart + i`,
  // which is only correct once logStart >= logWindow.startRow — the steady
  // state, but not the transient case right after a window jump/reload where
  // it's clamped to 0).
  $: visibleLogsSliceStart = Math.max(0, logStart - logWindow.startRow);
  $: visibleLogs = logWindow.lines.slice(
    visibleLogsSliceStart,
    Math.max(0, logEnd - logWindow.startRow),
  );
  $: logTopPad = logWrap
    ? (logOffsets ? logOffsets[Math.max(0, logStart - logWindow.startRow)] : 0) +
      logWindow.startRow * LOG_ROW_H
    : logStart * LOG_ROW_H;
  $: logBotPad = logWrap
    ? logContentH - logTopPad - visibleLogs.reduce((h, l) => h + (l.line && l.line.length > logCols ? Math.ceil(l.line.length / logCols) : 1) * LOG_ROW_H, 0)
    : (logTotal - logEnd) * LOG_ROW_H;

  // Scroll-driven (or filter-driven) window refill: fire-and-forget, and
  // re-checked whenever the visible absolute range changes.
  $: ensureRowsLoaded(logStart, logEnd);

  function buildLogOffsets(lines, cols) {
    const n = lines.length;
    const offs = new Array(n + 1);
    offs[0] = 0;
    for (let i = 0; i < n; i++) {
      const len = lines[i].line ? lines[i].line.length : 0;
      const span = len > cols ? Math.ceil(len / cols) : 1;
      offs[i + 1] = offs[i] + span * LOG_ROW_H;
    }
    return offs;
  }
  // Largest index i with offs[i] <= y (binary search over cumulative offsets).
  function offsetIndex(offs, y) {
    if (!offs || offs.length <= 1) return 0;
    let lo = 0,
      hi = offs.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (offs[mid] <= y) lo = mid;
      else hi = mid - 1;
    }
    return lo;
  }
  function measureLogMetrics() {
    if (!logBox) return;
    logViewportH = logBox.clientHeight;
    logInnerW = Math.max(50, logBox.clientWidth - 32); // minus 1rem padding each side
    try {
      const cs = getComputedStyle(logBox);
      const canvas = (measureLogMetrics._c ||= document.createElement("canvas"));
      const ctx = canvas.getContext("2d");
      if (ctx) {
        ctx.font = `${cs.fontSize} ${cs.fontFamily}`;
        const w = ctx.measureText("0".repeat(200)).width / 200;
        if (w > 0) logCharW = w;
      }
    } catch {}
  }
  function onLogScroll() {
    if (!logBox) return;
    logScrollTop = logBox.scrollTop;
    // Stick to the bottom only while the user is within ~2 rows of the end.
    logStick =
      logBox.scrollHeight - logBox.scrollTop - logBox.clientHeight <
      LOG_ROW_H * 2;
  }
  // selectedStep/selectedParallelGroup select which server-side VIEW is
  // active (Task 4): null → all steps, a single step → [idx], a parallel
  // group → its indices. Switching views re-fetches stats + a fresh tail
  // range from the server — the window's contents are never client-filtered.
  let viewInitialized = false; // skip the initial mount value: startSSE() already
  // establishes the all-steps window/tail, so firing here too would just be a
  // redundant (and racy) duplicate fetch before the SSE connection even opens.
  $: viewSteps = selectedParallelGroup !== null
    ? selectedParallelGroup
    : selectedStep !== null
      ? [selectedStep]
      : null;
  $: {
    if (viewInitialized) switchLogView(viewSteps);
    viewInitialized = true;
  }
  async function switchLogView(stepsSel) {
    logView = { steps: stepsSel };
    logStick = true;
    const token = ++windowFetchToken; // invalidate any in-flight range fetch for the old view
    // Set BEFORE the first await so ensureRowsLoaded's `if (windowLoading)
    // return` guard covers the ENTIRE switch (refreshStats + tail range
    // fetch), not just the range-fetch leg. Otherwise a scroll-driven
    // ensureRowsLoaded can fire during `await refreshStats()` below, bump
    // windowFetchToken itself, and cause this switch's token checks to
    // observe a mismatch and silently abort (no tail fetch, no jump to
    // bottom) — see the "log view switch atomicity" tests.
    windowLoading = true;
    // Also set before the first await, alongside windowLoading: this is what
    // the SSE reader actually checks to decide whether to drop a live batch
    // outright (see startSSE below) — narrower than windowLoading, which is
    // also true during a plain same-view ensureRowsLoaded fetch where drops
    // would lose totalCount growth permanently.
    viewSwitching = true;
    try {
      await refreshStats();
      if (token !== windowFetchToken) return; // superseded by a newer view switch
      const count = logWindow.totalCount;
      const start = Math.max(0, count - FETCH_CHUNK);
      const lines = await apiFetch(
        `/api/v1/runs/${runID}/logs/range?offset=${start}&limit=${FETCH_CHUNK}${viewStepsQuery()}`,
      );
      if (token !== windowFetchToken) return; // superseded (another view switch, etc.)
      logWindow = {
        ...logWindow,
        startRow: start,
        lines: lines.map((l) => ({ ...l, line: collapseCarriageReturns(l.line) })),
      };
    } catch (e) {
      console.warn("log view range fetch failed", e);
      // The switch already replaced logView (and, via refreshStats, may have
      // replaced totalCount) with the NEW view's, but the tail range fetch
      // failed — so logWindow.lines still holds the OLD view's rows. Leaving
      // them would render the wrong view's logs addressed as the new view's
      // rows, and because the window would "cover" [0, newCount) the scroll-
      // driven ensureRowsLoaded would never refetch. Install an empty window
      // instead: the view degrades to empty (the pre-branch failure mode) and,
      // with no rows covering the viewport, a later scroll's ensureRowsLoaded
      // sees it as uncovered and recovers. Keep whatever totalCount stands so
      // the scrollbar spans the new view (0 if refreshStats also failed).
      if (token === windowFetchToken) {
        logWindow = { startRow: 0, lines: [], totalCount: logWindow.totalCount };
      }
    } finally {
      if (token === windowFetchToken) {
        windowLoading = false;
        viewSwitching = false;
      }
    }
    if (token !== windowFetchToken) return;
    await tick();
    if (logQuery) runSearch(); // re-run over the new view (Task 5)
    if (!logBox) return;
    logBox.scrollTop = logBox.scrollHeight;
    logScrollTop = logBox.scrollTop;
  }

  // ---- Server-side log search (Task 5) ----
  // Native Ctrl+F only sees the virtualized (visible) rows, so we provide search
  // over the FULL log via the server: `GET /logs/search?q=...${viewStepsQuery()}`
  // returns `{total, matches: [{row, seq, stepIndex}, ...]}` over the CURRENT
  // view (all-steps or a step/group filter — the same view range/stats fetches
  // use), with `matches` capped at 1000 while `total` reflects the true
  // (uncapped) count. `logMatches` holds just the `row` numbers (ABSOLUTE row
  // numbers within the current view, directly usable by the virtual scroller).
  // Jumping to a match only moves `logBox.scrollTop` — it does not fetch a
  // range itself; the existing scroll handler's `ensureRowsLoaded` (fired
  // reactively off `logStart`/`logEnd`) brings the row's window in, so the
  // jump doesn't fight the race discipline (windowFetchToken/windowLoading/
  // viewSwitching) already governing scroll/view-switch/SSE-reconnect fetches.
  let logQuery = "";
  let logMatchPos = 0;
  let logMatches = []; // absolute row numbers, from the server
  let logSearchTotal = 0; // true (uncapped) match count reported by the server
  let logSearchToken = 0; // guards against out-of-order debounced responses
  let logSearchDebounce = null;
  const LOG_SEARCH_DEBOUNCE_MS = 300;

  // Keep the cursor in range when the match set changes (new search, filter, edit).
  $: if (logMatchPos >= logMatches.length) logMatchPos = 0;
  $: curMatchRow = logMatches.length ? logMatches[logMatchPos] : -1;
  $: logQuery, scheduleSearch();
  function scheduleSearch() {
    if (logSearchDebounce) clearTimeout(logSearchDebounce);
    if (!logQuery) {
      // Empty query: clear state locally, never call the server (it 400s on
      // an empty q anyway).
      logSearchToken++; // invalidate any in-flight search response
      logMatches = [];
      logSearchTotal = 0;
      logMatchPos = 0;
      return;
    }
    logSearchDebounce = setTimeout(runSearch, LOG_SEARCH_DEBOUNCE_MS);
  }
  async function runSearch() {
    const q = logQuery;
    if (!q) return;
    const token = ++logSearchToken;
    try {
      const resp = await apiFetch(
        `/api/v1/runs/${runID}/logs/search?q=${encodeURIComponent(q)}${viewStepsQuery()}`,
      );
      if (token !== logSearchToken) return; // superseded by a newer query/view/reconnect
      logMatches = (resp?.matches || []).map((m) => m.row);
      logSearchTotal = typeof resp?.total === "number" ? resp.total : logMatches.length;
      logMatchPos = 0;
      if (logMatches.length) gotoMatch(0);
    } catch (e) {
      console.warn("log search failed", e);
      if (token !== logSearchToken) return;
      logMatches = [];
      logSearchTotal = 0;
      logMatchPos = 0;
    }
  }
  function gotoMatch(pos) {
    if (!logMatches.length) return;
    const n = logMatches.length;
    logMatchPos = ((pos % n) + n) % n; // wrap around
    const rowIdx = logMatches[logMatchPos];
    logStick = false; // don't fight the jump with auto-scroll
    tick().then(() => {
      if (!logBox) return;
      // Jump by absolute row * LOG_ROW_H — same fixed-row-height addressing
      // ensureRowsLoaded/logStart/logEnd use outside wrap mode. (Wrap mode's
      // per-line offsets are only known for rows already in the window, which
      // an off-window match generally isn't yet — this is a known v1
      // approximation, consistent with logContentH's own wrap-mode note.)
      const targetY = rowIdx * LOG_ROW_H;
      logBox.scrollTop = Math.max(0, targetY - logBox.clientHeight / 2);
      logScrollTop = logBox.scrollTop;
    });
  }
  function onSearchKey(e) {
    if (e.key === "Enter") {
      e.preventDefault();
      gotoMatch(logMatchPos + (e.shiftKey ? -1 : 1));
    } else if (e.key === "Escape") {
      logQuery = "";
    }
  }
  // Split a line into {t, hit} segments around case-insensitive matches of q.
  function highlightSegments(line, q) {
    if (!q) return [{ t: line, hit: false }];
    const segs = [];
    const lc = line.toLowerCase();
    const qc = q.toLowerCase();
    let i = 0;
    while (i < line.length) {
      const idx = lc.indexOf(qc, i);
      if (idx === -1) {
        segs.push({ t: line.slice(i), hit: false });
        break;
      }
      if (idx > i) segs.push({ t: line.slice(i, idx), hit: false });
      segs.push({ t: line.slice(idx, idx + q.length), hit: true });
      i = idx + q.length;
    }
    return segs;
  }

  // Reactive so the log labels re-render when steps load after SSE starts.
  // Logs are shared across all matrix variants of a step index (there is no
  // per-line variant tag), so when multiple rows share `idx` we just take the
  // first one for the label rather than trying to represent every variant.
  $: stepName = (idx) => {
    if (idx === -1) return "System";
    const s = steps.find((s) => s.index === idx);
    return s ? s.name : "step " + idx;
  };
  // Sidecar rows carry their own running/exited status (not one of the run
  // step statuses statusBadge() understands), so map it to the existing
  // badge color tokens directly: running → success/green, exited with a
  // null/undefined exit code (best-effort outcome: host inspect error, or
  // k8s container not yet terminated / Get failed) → muted (same as exited
  // 0 — not an error signal), exited non-zero → danger/red.
  function sidecarDotClass(s) {
    if (s.status === "exited") return s.exitCode ? "dot-danger" : "dot-muted";
    return "dot-success"; // "running" / "Running"
  }
  function sidecarStatusLabel(s) {
    if (s.status !== "exited") return "running";
    return s.exitCode == null ? "exited" : `exited ${s.exitCode}`;
  }
  function stepDuration(s) {
    if (!s.startedAt) return "";
    const start = new Date(s.startedAt).getTime();
    const end = s.endedAt ? new Date(s.endedAt).getTime() : Date.now();
    const ms = end - start;
    if (ms < 1000) return ms + "ms";
    if (ms < 60000) return (ms / 1000).toFixed(1) + "s";
    return (
      Math.floor(ms / 60000) + "m " + Math.floor((ms % 60000) / 1000) + "s"
    );
  }
  async function loadRun() {
    try {
      run = await apiFetch("/api/v1/runs/" + runID);
    } catch (e) {
      error = e.message;
    }
  }
  async function loadSteps() {
    try {
      steps = await apiFetch("/api/v1/runs/" + runID + "/steps");
    } catch {}
  }
  async function loadApprovals() {
    try {
      approvals = await apiFetch("/api/v1/runs/" + runID + "/approvals");
    } catch {}
  }
  async function loadArtifacts() {
    try {
      artifacts = (await apiFetch("/api/v1/runs/" + runID + "/artifacts")) || [];
    } catch {
      artifacts = [];
    }
  }
  // The artifact endpoint streams a binary tar.gz and needs the auth header, so a
  // plain <a href> won't do for token auth — fetch it and save via a blob URL.
  async function downloadArtifact(name) {
    try {
      const headers = {};
      const t = get(token);
      if (t) headers["Authorization"] = "Bearer " + t;
      const resp = await fetch(
        get(serverURL) +
          "/api/v1/runs/" +
          runID +
          "/artifacts/" +
          encodeURIComponent(name),
        { credentials: "include", headers },
      );
      if (!resp.ok) {
        error = "Artifact download failed (" + resp.status + ")";
        return;
      }
      const blob = await resp.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = name + ".tar.gz";
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
    } catch (e) {
      error = e.message;
    }
  }
  async function decideApproval(stepIndex, decision, comment) {
    try {
      await apiFetch("/api/v1/runs/" + runID + "/approvals/" + stepIndex, {
        method: "POST",
        body: JSON.stringify({ decision, comment: comment || "" }),
      });
      await Promise.all([loadSteps(), loadApprovals()]);
    } catch (e) {
      error = e.message;
    }
  }
  function stopStepPolling() {
    if (stepsTimer) {
      clearInterval(stepsTimer);
      stepsTimer = null;
    }
  }
  function startStepPolling() {
    stopStepPolling();
    stepsTimer = setInterval(async () => {
      await Promise.all([loadSteps(), loadRun(), loadApprovals(), loadArtifacts()]);
      if (run && ["Succeeded", "Failed", "Cancelled"].includes(run.status))
        stopStepPolling();
    }, 3000);
  }
  async function startSSE() {
    if (abortController) {
      abortController.abort();
      abortController = null;
    }
    logWindow = { startRow: 0, lines: [], totalCount: 0 };
    windowFetchToken++; // invalidate any in-flight range fetch from the old connection
    // startSSE is now the sole owner of the (new) token, so it must also
    // reset the flags a superseded switchLogView would otherwise have reset
    // itself: that switch's `finally` is gated on `token === windowFetchToken`,
    // which can never pass again once we've bumped past its token here — left
    // alone, windowLoading/viewSwitching would stay stuck true forever (SSE
    // batches for THIS run dropped permanently, ensureRowsLoaded permanently
    // blocked). Reset synchronously, before the await below, so nothing else
    // can observe a mismatched state in between.
    windowLoading = false;
    viewSwitching = false;
    await refreshStats();
    if (logQuery) runSearch(); // re-run over the reconnected run (Task 5)
    let backfilled = false; // first non-empty batch is the SSE backfill, not a live append
    abortController = new AbortController();
    const headers = {};
    const t = get(token);
    if (t) headers["Authorization"] = "Bearer " + t;
    try {
      const resp = await fetch(
        get(serverURL) + "/api/v1/runs/" + runID + "/events",
        {
          credentials: "include",
          headers,
          signal: abortController.signal,
        },
      );
      if (!resp.ok || !resp.body) return;
      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buf = "";
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        const parts = buf.split("\n\n");
        buf = parts.pop();
        // Collect this chunk's log lines and flush them in ONE reactive update.
        // Appending per line plus a per-line tick + scrollHeight read is
        // O(n^2) array copies and O(n) forced layout reflows — a 10k-line
        // backfill (one chunk) would freeze the tab. The backfill usually
        // arrives in a single read, so this is one flush.
        const batch = [];
        let terminalStatus = null;
        for (const part of parts) {
          const line = part.replace(/^data: /, "").trim();
          if (!line) continue;
          try {
            const data = JSON.parse(line);
            if (data.type === "log") {
              // \r-progress output (git clone etc.) arrives as one long
              // line; keep only its final redraw, like a terminal would.
              batch.push({ ...data, line: collapseCarriageReturns(data.line) });
            } else if (data.type === "truncated") {
              // No longer surfaced as a banner: the scrollbar itself spans
              // the full log via logWindow.totalCount, and older lines are
              // reachable by scrolling up (ensureRowsLoaded fetches them),
              // or via a step view's own server-side range (switchLogView).
            } else if (data.type === "status") {
              if (run) run = { ...run, status: data.status };
              if (["Succeeded", "Failed", "Cancelled"].includes(data.status)) {
                terminalStatus = data.status;
              }
            }
          } catch {}
        }
        if (batch.length) {
          if (!backfilled && viewSwitching) {
            // A user-initiated switchLogView is ALREADY in flight when the
            // initial SSE backfill lands (user clicked a step after steps
            // rendered but before the backfill's first batch arrived). Do NOT
            // bump the token or install this all-steps backfill: the bump
            // would cancel the switch, and installing would render the
            // all-steps tail under the step filter (logView.steps already
            // points at the new view) with no self-correction. Just mark the
            // backfill consumed and let the switch's own refreshStats()+range
            // fetch install the window; later batches then take the
            // live-append path below, which correctly filters by
            // logView.steps. (The head-fetch race the bump was added for has
            // NO switch in flight, so the bump still applies in that case —
            // the `!backfilled` branch below.)
            backfilled = true;
          } else if (!backfilled) {
            // Initial SSE backfill becomes the initial window. totalCount
            // comes from refreshStats(); if that failed (or under-counts vs.
            // what the backfill itself delivered), fall back to the batch
            // length so tests/environments without a stats mock still work.
            backfilled = true;
            // startSSE()'s `await refreshStats()` set totalCount while the
            // window was still empty and logScrollTop was 0, so the reactive
            // `$: ensureRowsLoaded(logStart, logEnd)` may have already fired a
            // HEAD range fetch (offset 0) that is still in flight. Left alone,
            // its token is still valid, so when it lands it would replace this
            // tail window with rows 0..FETCH_CHUNK — but stick-scroll has
            // already parked the viewport at the bottom, leaving a blank
            // "Loading…" on open. Invalidate any such in-flight range fetch
            // the same way startSSE does (token bump + synchronous flag
            // reset), so this tail install wins and the head fetch no-ops.
            windowFetchToken++;
            windowLoading = false;
            viewSwitching = false;
            const totalCount = Math.max(logWindow.totalCount, batch.length);
            logWindow = {
              startRow: Math.max(0, totalCount - batch.length),
              lines: batch,
              totalCount,
            };
          } else if (viewSwitching) {
            // A view switch (switchLogView) is in flight: `logWindow` may
            // still hold the OLD view's contents while `logView.steps`
            // already points at the NEW view (e.g. mid-switchLogView, before
            // its refreshStats/tail range fetch have landed). Appending here
            // would mix the two and inflate totalCount transiently — and
            // permanently if this fetch turns out to be superseded. Drop the
            // batch instead: the switch's own refreshStats() + range fetch
            // supersede it and totalCount catches up.
          } else {
            // While a step/group view is active, only lines that belong to
            // the view are appended to the window; the rest are ignored here
            // (the all-steps view's totalCount catches up via refreshStats()
            // when the user switches back to it — see switchLogView).
            const inView = logView.steps
              ? batch.filter((l) => logView.steps.includes(l.stepIndex))
              : batch;
            const atTail = logWindow.startRow + logWindow.lines.length >= logWindow.totalCount;
            // A plain scroll-driven ensureRowsLoaded fetch (windowLoading
            // true, but NOT a view switch) is in flight: `lines`/`startRow`
            // are about to be replaced wholesale by that fetch's own result
            // (which doesn't touch totalCount), so appending to `lines` here
            // would either be clobbered a moment later or, worse, get
            // stitched onto a window that fetch is about to discard. Only
            // totalCount is safe to grow — same as the "not at tail" case
            // below, which this reuses.
            if (atTail && !windowLoading) {
              let lines = logWindow.lines.concat(inView);
              let startRow = logWindow.startRow;
              if (lines.length > WINDOW_MAX) {
                const evict = lines.length - WINDOW_MAX;
                lines = lines.slice(evict);
                startRow += evict;
              }
              logWindow = { ...logWindow, startRow, lines, totalCount: logWindow.totalCount + inView.length };
            } else {
              logWindow = { ...logWindow, totalCount: logWindow.totalCount + inView.length };
            }
          }
          // Stick-scroll applies to filtered views too: if the incoming
          // batch is filtered out, scrollHeight is unchanged and the
          // assignment is a no-op.
          if (logStick) {
            await tick();
            if (logBox) {
              logBox.scrollTop = logBox.scrollHeight;
              logScrollTop = logBox.scrollTop;
            }
          }
        }
        if (terminalStatus) {
          await loadSteps();
          await loadArtifacts();
          stopStepPolling();
          abortController.abort();
          return;
        }
      }
    } catch (e) {
      if (e.name !== "AbortError") error = e.message;
    }
  }
  async function cancelRun() {
    try {
      await apiFetch("/api/v1/runs/" + runID + "/cancel", { method: "POST" });
      await loadRun();
    } catch (e) {
      error = e.message;
    }
  }
  async function rebuild() {
    try {
      const newRun = await apiFetch("/api/v1/runs", {
        method: "POST",
        body: JSON.stringify({ jobName: run.jobName, params: run.params }),
      });
      push("/runs/" + newRun.id);
    } catch (e) {
      error = e.message;
    }
  }
  function selectStep(idx) {
    selectedParallelGroup = null;
    selectedStep = selectedStep === idx ? null : idx;
  }

  async function init() {
    // Reset the per-run log-view selection: step indices are per-run, so a
    // selection carried over from the previously-viewed run (this component
    // instance is REUSED across runID changes — see the `$: runID, init()`
    // note below) is meaningless and, left set, renders run B's all-steps
    // backfill under run A's step filter (labels hidden, inconsistent
    // totalCount). On the INITIAL mount both are already null, so this is a
    // no-op and the view-switch reactive stays dormant (guarded by
    // viewInitialized). On a cross-run navigation with a step selected,
    // clearing it here flips viewSteps back to null and lets the view-switch
    // reactive fire switchLogView(null) — which is benign: it fetches the NEW
    // run's ALL-steps view (runID already points at run B by now), the same
    // view startSSE() below installs, so at worst it is a redundant same-view
    // fetch, never a cross-view mix.
    selectedStep = null;
    selectedParallelGroup = null;
    loading = true;
    await Promise.all([loadRun(), loadSteps(), loadApprovals(), loadArtifacts()]);
    loading = false;
    await tick();
    measureLogMetrics();
    if (run && !["Succeeded", "Failed", "Cancelled"].includes(run.status))
      startStepPolling();
    startSSE();
  }

  // Reactive statements run once during component initialization (covering the
  // initial mount) and again whenever runID changes (e.g. navigating between
  // runs via "Rerun", which svelte-spa-router handles by reusing this component
  // instance rather than recreating it). Do NOT also call init() from onMount —
  // that caused a duplicate concurrent SSE connection/log fetch on first load,
  // where the second connection's `logWindow` reset could wipe out or race
  // with logs already delivered by the first, leaving the panel stuck empty.
  if (typeof window !== "undefined")
    window.addEventListener("resize", measureLogMetrics);
  onDestroy(() => {
    if (abortController) abortController.abort();
    stopStepPolling();
    if (logSearchDebounce) clearTimeout(logSearchDebounce);
    logSearchToken++; // invalidate any in-flight search response
    // Invalidate any in-flight range fetch and reset the window flags, so a
    // settle re-check cannot outlive the component: each ensureRowsLoaded
    // iteration takes a fresh token, so without a bump here a fetch loop
    // survives destruction (only startSSE/switchLogView bump otherwise, and
    // neither runs after unmount). Reset the flags synchronously for the same
    // reason startSSE does — a superseded fetch's finally is gated on the
    // now-stale token and will never reset them itself.
    windowFetchToken++;
    windowLoading = false;
    viewSwitching = false;
    if (typeof window !== "undefined")
      window.removeEventListener("resize", measureLogMetrics);
  });
  $: runID, init();
</script>

<div class="container">
  <AuthSetup />
  <div style="display:flex;align-items:center;gap:1rem;margin-bottom:1rem">
    {#if run}<a href="#/jobs/{encodeURIComponent(run.jobName)}">← {run.jobName}</a>{/if}
    <h1>Run Detail</h1>
    {#if run && ["Running", "Queued", "Pending"].includes(run.status)}
      <button class="btn btn-danger" on:click={cancelRun}>Cancel</button>
    {/if}
    {#if run && ["Succeeded", "Failed", "Cancelled"].includes(run.status)}
      <button class="btn" on:click={rebuild}>↺ Rerun</button>
    {/if}
  </div>
  <div style="border-bottom:1px solid var(--border);margin-bottom:1.5rem">
    <a href="#/runs/{runID}" class="tab-link tab-active">Logs</a>
    <a href="#/runs/{runID}/yaml" class="tab-link">YAML</a>
  </div>
  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if run}
    {#if run.calledBy}
      <div class="called-by meta" style="margin-bottom:0.75rem">
        Called by <a href="#/runs/{run.calledBy.parentRunId}" title="Caller step: {run.calledBy.stepName}">{run.calledBy.parentJobName} ↗</a>
      </div>
    {/if}
    <div class="card grid-2" style="margin-bottom:1rem">
      <div>
        <div class="meta">Status</div>
        <span class={statusBadge(run.status)}>{run.status}</span>
      </div>
      <div>
        <div class="meta">Triggered by</div>
        <div>{run.triggeredBy}</div>
      </div>
      {#if run.claimedBy}
        <div class="run-agent">
          <div class="meta">Agent</div>
          <div><a href="#/agents/{run.claimedBy}">{run.claimedBy} ↗</a></div>
        </div>
      {/if}
      <div>
        <div class="meta">Created</div>
        <div>{fmtTime(run.createdAt)}</div>
      </div>
      {#if run.params && Object.keys(run.params).length}
        <div style="grid-column:1/-1">
          <div class="meta" style="margin-bottom:0.35rem">Params</div>
          <div class="params-grid">
            {#each Object.entries(run.params) as [k, v]}
              <span class="param-k">{k}</span>
              <span class="param-v">{v}</span>
            {/each}
          </div>
        </div>
      {/if}
    </div>
    {#if steps.length}
      {#each stepSections as sec (sec.section)}
        <h2 style="margin-bottom:0.5rem">{sec.label}</h2>
        <div class="step-list">
          {#if sec.section === "sidecars"}
            {#each sec.groups[0].steps as s (s.index)}
              <div
                class="step-row {selectedStep === s.index ? 'active' : ''}"
                on:click={() => selectStep(s.index)}
                role="button"
                tabindex="0"
                on:keydown={(e) => e.key === 'Enter' && selectStep(s.index)}
              >
                {#if s.status}
                  <span class="sidecar-dot {sidecarDotClass(s)}" title={s.status}></span>
                {/if}
                <span class="step-name">{s.name}</span>
                {#if s.status}
                  <span class="step-exit meta">{sidecarStatusLabel(s)}</span>
                {/if}
              </div>
            {/each}
          {:else}
          {#each sec.groups as group (group.stageIndex)}
            {#if group.steps.length > 1}
              <!-- Parallel group header -->
              <div
                class="stage-group-header {group.steps.some(s => selectedStep === s.index) ? 'active' : ''}"
                on:click={() => {
                  const indices = group.steps.map(s => s.index);
                  const allSelected = selectedParallelGroup !== null && indices.every(i => selectedParallelGroup.includes(i));
                  selectedStep = allSelected ? null : indices[0];
                  selectedParallelGroup = allSelected ? null : indices;
                }}
                role="button"
                tabindex="0"
                on:keydown={(e) => e.key === 'Enter' && (() => {
                  const indices = group.steps.map(s => s.index);
                  selectedParallelGroup = selectedParallelGroup?.join() === indices.join() ? null : indices;
                  selectedStep = null;
                })()}
              >
                <span class="parallel-label">parallel</span>
                <span class="meta" style="font-size:0.75rem">
                  {group.steps.map(s => s.name).join(' · ')}
                </span>
              </div>
              {#each group.steps as s (`${s.index}/${s.variant ?? ""}`)}
                {@const approval = approvals.find(a => a.stepIndex === s.index)}
                <div
                  class="step-row step-row-indented {selectedStep === s.index ? 'active' : ''}"
                  on:click={() => selectStep(s.index)}
                  role="button"
                  tabindex="0"
                  on:keydown={(e) => e.key === 'Enter' && selectStep(s.index)}
                >
                  <span class={statusBadge(s.status)}>{s.status}</span>
                  <span class="step-name">{s.name}</span>
                  {#if s.kind}<span class="step-kind meta">[{s.kind}]{#if s.matrix} matrix{/if}</span>{/if}
                  {#if s.childRunId}
                    <a class="call-link" href="#/runs/{s.childRunId}" title="Called job run">{s.callJobName || 'child run'} ↗</a>
                  {/if}
                  <span class="step-duration">{stepDuration(s)}</span>
                  {#if s.exitCode != null}<span class="step-exit">exit {s.exitCode}</span>{/if}
                </div>
                {#if s.status === 'WaitingApproval'}
                  <div class="approval-panel">
                    {#if approval?.message}
                      <div class="approval-message">{approval.message}</div>
                    {/if}
                    <textarea
                      class="approval-comment"
                      placeholder="Comment (optional)"
                      bind:value={approvalComments[s.index]}
                    ></textarea>
                    <div class="approval-actions">
                      <button
                        class="btn btn-success"
                        on:click|stopPropagation={() => decideApproval(s.index, 'approve', approvalComments[s.index])}
                      >Approve</button>
                      <button
                        class="btn btn-danger"
                        on:click|stopPropagation={() => decideApproval(s.index, 'reject', approvalComments[s.index])}
                      >Reject</button>
                    </div>
                  </div>
                {:else if approval?.decidedBy}
                  <div class="approval-decision">
                    Decided by <strong>{approval.decidedBy}</strong>
                    {#if approval.comment} — {approval.comment}{/if}
                  </div>
                {/if}
              {/each}
            {:else}
              <!-- Single sequential step -->
              {@const s0 = group.steps[0]}
              {@const approval0 = approvals.find(a => a.stepIndex === s0.index)}
              <div
                class="step-row {selectedStep === s0.index ? 'active' : ''}"
                on:click={() => selectStep(s0.index)}
                role="button"
                tabindex="0"
                on:keydown={(e) => e.key === 'Enter' && selectStep(s0.index)}
              >
                <span class={statusBadge(s0.status)}>{s0.status}</span>
                <span class="step-name">{s0.name}</span>
                {#if s0.kind}<span class="step-kind meta">[{s0.kind}]{#if s0.matrix} matrix{/if}</span>{/if}
                {#if s0.childRunId}
                  <a class="call-link" href="#/runs/{s0.childRunId}" title="Called job run">{s0.callJobName || 'child run'} ↗</a>
                {/if}
                <span class="step-duration">{stepDuration(s0)}</span>
                {#if s0.exitCode != null}<span class="step-exit">exit {s0.exitCode}</span>{/if}
              </div>
              {#if s0.status === 'WaitingApproval'}
                <div class="approval-panel">
                  {#if approval0?.message}
                    <div class="approval-message">{approval0.message}</div>
                  {/if}
                  <textarea
                    class="approval-comment"
                    placeholder="Comment (optional)"
                    bind:value={approvalComments[s0.index]}
                  ></textarea>
                  <div class="approval-actions">
                    <button
                      class="btn btn-success"
                      on:click|stopPropagation={() => decideApproval(s0.index, 'approve', approvalComments[s0.index])}
                    >Approve</button>
                    <button
                      class="btn btn-danger"
                      on:click|stopPropagation={() => decideApproval(s0.index, 'reject', approvalComments[s0.index])}
                    >Reject</button>
                  </div>
                </div>
              {:else if approval0?.decidedBy}
                <div class="approval-decision">
                  Decided by <strong>{approval0.decidedBy}</strong>
                  {#if approval0.comment} — {approval0.comment}{/if}
                </div>
              {/if}
            {/if}
          {/each}
          {/if}
        </div>
      {/each}
    {/if}
    {#if artifacts.length}
      <div class="artifacts">
        <h2 style="margin-bottom:0.5rem">Artifacts</h2>
        <div class="artifact-list">
          {#each artifacts as a (a.name)}
            <button
              class="btn artifact-item"
              on:click={() => downloadArtifact(a.name)}
              title="Download {a.name}.tar.gz"
            >⬇ {a.name}<span class="meta artifact-ext">.tar.gz</span></button>
          {/each}
        </div>
      </div>
    {/if}
    <div class="log-header">
      <h2>Logs</h2>
      {#if selectedStep !== null}
        <span class="meta" style="font-size:0.8rem"
          >— {stepName(selectedStep)}</span
        >
        <button
          class="btn"
          style="padding:0.2rem 0.5rem;font-size:0.75rem"
          on:click={() => (selectedStep = null)}>All Steps</button
        >
      {/if}
      {#if logTotal}
        <span class="meta" style="font-size:0.75rem"
          >{logTotal.toLocaleString()} lines</span
        >
      {/if}
      <button
        class="btn log-wrap-btn"
        class:active={logWrap}
        title="Wrap long lines"
        aria-pressed={logWrap}
        on:click={() => (logWrap = !logWrap)}>⤶ Wrap</button
      >
      <div class="log-search">
        <input
          class="log-search-input"
          type="search"
          placeholder="Search logs…"
          bind:value={logQuery}
          on:keydown={onSearchKey}
        />
        {#if logQuery}
          <span class="meta log-search-count"
            >{logMatches.length ? logMatchPos + 1 : 0} / {logSearchTotal > logMatches.length
              ? `${logMatches.length.toLocaleString()}+`
              : logSearchTotal.toLocaleString()}</span
          >
          <button
            class="btn log-search-btn"
            title="Previous match (Shift+Enter)"
            on:click={() => gotoMatch(logMatchPos - 1)}
            disabled={!logMatches.length}>‹</button
          >
          <button
            class="btn log-search-btn"
            title="Next match (Enter)"
            on:click={() => gotoMatch(logMatchPos + 1)}
            disabled={!logMatches.length}>›</button
          >
        {/if}
      </div>
      <span class="meta" style="font-size:0.75rem">SSE</span>
    </div>
    <div
      class="log-box"
      class:wrap={logWrap}
      bind:this={logBox}
      on:scroll={onLogScroll}
    >
      {#if !visibleLogs.length}
        {#if windowLoading}
          <span style="color:var(--text-muted)">Loading…</span>
        {:else if selectedStep !== null || selectedParallelGroup !== null}
          <span style="color:var(--text-muted)">No log lines for this selection.</span>
        {:else if logTotal === 0}
          <span style="color:var(--text-muted)">Waiting for logs…</span>
        {:else}
          <span style="color:var(--text-muted)">Loading…</span>
        {/if}
      {:else}
        <div style="height:{logTopPad}px" aria-hidden="true"></div>
        {#each visibleLogs as l, i (l.seq)}
          {@const absRow = logWindow.startRow + visibleLogsSliceStart + i}
          <div
            class="log-row"
            class:log-row-wrap={logWrap}
            class:log-row-current={absRow === curMatchRow}
          >
            {#if selectedStep === null}<span class="meta log-step-label"
                >{stepName(l.stepIndex)}</span
              >{/if}<span
              class={l.stream === "stderr" && !$stderrPlain
                ? "log-stderr"
                : "log-stdout"}
              >{#if logQuery}{#each highlightSegments(l.line, logQuery) as seg}{#if seg.hit}<mark class="log-hit" class:log-hit-current={absRow === curMatchRow}>{seg.t}</mark>{:else}{seg.t}{/if}{/each}{:else}{l.line}{/if}</span
            >
          </div>
        {/each}
        <div style="height:{logBotPad}px" aria-hidden="true"></div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .params-grid {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.2rem 0.75rem;
    font-size: 0.8rem;
    font-family: monospace;
  }
  .param-k {
    color: var(--text-muted);
    white-space: nowrap;
  }
  /* Fixed-height rows are required by the log virtual scroller (LOG_ROW_H). */
  .log-row {
    height: 20px;
    line-height: 20px;
    white-space: pre;
  }
  /* Wrap mode: variable-height rows (the virtual scroller estimates each row's
     wrapped height). min-height keeps the estimate honest without clipping. */
  .log-row-wrap {
    height: auto;
    min-height: 20px;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    word-break: break-all;
  }
  .log-box.wrap {
    overflow-x: hidden;
  }
  .log-step-label {
    font-size: 0.7rem;
    margin-right: 0.4rem;
  }
  .log-search {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 0.3rem;
  }
  .log-search-input {
    font-size: 0.75rem;
    padding: 0.2rem 0.45rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    background: var(--surface);
    color: var(--text);
    width: 12rem;
    max-width: 40vw;
  }
  .log-search-count {
    font-size: 0.72rem;
    min-width: 3.2rem;
    text-align: right;
    white-space: nowrap;
  }
  .log-search-btn {
    padding: 0.05rem 0.45rem;
    font-size: 1rem;
    line-height: 1.2;
  }
  .log-wrap-btn {
    margin-left: auto;
    padding: 0.15rem 0.5rem;
    font-size: 0.75rem;
    white-space: nowrap;
  }
  .log-wrap-btn.active {
    background: var(--primary-light, #e8f0fe);
    border-color: var(--primary, #4285f4);
  }
  .log-row-current {
    background: rgba(255, 150, 50, 0.12);
  }
  mark.log-hit {
    padding: 0;
    background: rgba(255, 214, 0, 0.4);
    color: inherit;
    border-radius: 2px;
  }
  mark.log-hit-current {
    background: #ff9632;
    color: #1a1a1a;
  }
  .param-v {
    color: var(--text);
    font-weight: 500;
    word-break: break-all;
  }
  .artifacts {
    margin-bottom: 1rem;
  }
  .artifact-list {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem;
  }
  .artifact-item {
    font-family: monospace;
    font-size: 0.8rem;
    padding: 0.25rem 0.55rem;
  }
  .artifact-ext {
    margin-left: 0.15rem;
  }
  .stage-group-header {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.3rem 0.5rem;
    background: var(--surface-alt, var(--surface));
    border-radius: 4px;
    cursor: pointer;
    margin-bottom: 0.1rem;
  }
  .stage-group-header:hover,
  .stage-group-header.active {
    background: var(--primary-light, #e8f0fe);
  }
  .parallel-label {
    font-size: 0.65rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted);
    border: 1px solid var(--border);
    border-radius: 3px;
    padding: 0.1rem 0.3rem;
  }
  .step-row-indented {
    margin-left: 1.2rem;
  }
  /* Sidecar rows show a small status dot instead of the full status badge
     text (no run-step status applies); reuse the existing badge color
     tokens (success/pending/failed) so the palette stays consistent with
     the rest of the step list. */
  .sidecar-dot {
    display: inline-block;
    width: 8px;
    height: 8px;
    border-radius: 50%;
    flex-shrink: 0;
  }
  .sidecar-dot.dot-success {
    background: var(--badge-success-fg);
  }
  .sidecar-dot.dot-muted {
    background: var(--badge-pending-fg);
  }
  .sidecar-dot.dot-danger {
    background: var(--badge-failed-fg);
  }
  .approval-panel {
    padding: 0.6rem 0.75rem;
    background: var(--surface-alt, var(--surface));
    border: 1px solid var(--border);
    border-radius: 4px;
    margin-top: 0.25rem;
    margin-bottom: 0.25rem;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .approval-message {
    font-size: 0.875rem;
    color: var(--text);
  }
  .approval-comment {
    width: 100%;
    min-height: 3.5rem;
    padding: 0.35rem 0.5rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    background: var(--surface);
    color: var(--text);
    font-size: 0.85rem;
    resize: vertical;
    box-sizing: border-box;
  }
  .approval-actions {
    display: flex;
    gap: 0.5rem;
  }
  .btn-success {
    background: #22863a;
    color: #fff;
    border: none;
  }
  .btn-success:hover {
    background: #1a6b2c;
  }
  .approval-decision {
    font-size: 0.8rem;
    color: var(--text-muted);
    padding: 0.25rem 0.75rem;
    margin-bottom: 0.25rem;
  }
</style>
