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
    try {
      const center = Math.floor((firstRow + lastRow) / 2);
      const start = Math.max(0, center - Math.floor(FETCH_CHUNK / 2));
      const lines = await apiFetch(
        `/api/v1/runs/${runID}/logs/range?offset=${start}&limit=${FETCH_CHUNK}${viewStepsQuery()}`,
      );
      if (token !== windowFetchToken) return; // superseded (view switch, etc.)
      logWindow = {
        ...logWindow,
        startRow: start,
        lines: lines.map((l) => ({ ...l, line: collapseCarriageReturns(l.line) })),
      };
    } catch (e) {
      console.warn("log range fetch failed", e);
    } finally {
      if (token === windowFetchToken) windowLoading = false;
    }
  }

  $: stepSections = (() => {
    const bySection = { main: [], finally: [] };
    for (const s of steps) {
      (s.section === "finally" ? bySection.finally : bySection.main).push(s);
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
  $: visibleLogs = logWindow.lines.slice(
    Math.max(0, logStart - logWindow.startRow),
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
    await refreshStats();
    if (token !== windowFetchToken) return; // superseded by a newer view switch
    windowLoading = true;
    try {
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
    } finally {
      if (token === windowFetchToken) windowLoading = false;
    }
    if (token !== windowFetchToken) return;
    await tick();
    if (!logBox) return;
    logBox.scrollTop = logBox.scrollHeight;
    logScrollTop = logBox.scrollTop;
  }

  // ---- In-app log search ----
  // Native Ctrl+F only sees the virtualized (visible) rows, so we provide search
  // over the FULL log: it scans every line in memory, counts matches, jumps the
  // virtual list to each match, and highlights the hits.
  // NOTE: this scans only the currently-loaded WINDOW, not the full server-side
  // log (Task 5 replaces it with a server /logs/search call over the whole
  // view). Match positions are ABSOLUTE row numbers (window-relative index +
  // logWindow.startRow), matching the absolute addressing used by
  // logStart/logEnd/curMatchRow elsewhere.
  let logQuery = "";
  let logMatchPos = 0;
  $: logMatches = logQuery
    ? logWindow.lines.reduce((acc, l, idx) => {
        if (l.line && l.line.toLowerCase().includes(logQuery.toLowerCase()))
          acc.push(idx + logWindow.startRow);
        return acc;
      }, [])
    : [];
  // Keep the cursor in range when the match set changes (new logs, filter, edit).
  $: if (logMatchPos >= logMatches.length) logMatchPos = 0;
  $: curMatchRow = logMatches.length ? logMatches[logMatchPos] : -1;
  $: logQuery, resetMatchPos();
  function resetMatchPos() {
    logMatchPos = 0;
    tick().then(() => {
      if (logQuery && logMatches.length) gotoMatch(0);
    });
  }
  function gotoMatch(pos) {
    if (!logMatches.length) return;
    const n = logMatches.length;
    logMatchPos = ((pos % n) + n) % n; // wrap around
    const rowIdx = logMatches[logMatchPos];
    logStick = false; // don't fight the jump with auto-scroll
    tick().then(() => {
      if (!logBox) return;
      const rel = rowIdx - logWindow.startRow;
      const targetY =
        logWrap && logOffsets && rel >= 0 && rel < logOffsets.length
          ? logOffsets[rel] + logWindow.startRow * LOG_ROW_H
          : rowIdx * LOG_ROW_H;
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
    await refreshStats();
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
          if (!backfilled) {
            // Initial SSE backfill becomes the initial window. totalCount
            // comes from refreshStats(); if that failed (or under-counts vs.
            // what the backfill itself delivered), fall back to the batch
            // length so tests/environments without a stats mock still work.
            backfilled = true;
            const totalCount = Math.max(logWindow.totalCount, batch.length);
            logWindow = {
              startRow: Math.max(0, totalCount - batch.length),
              lines: batch,
              totalCount,
            };
          } else {
            // While a step/group view is active, only lines that belong to
            // the view are appended to the window; the rest are ignored here
            // (the all-steps view's totalCount catches up via refreshStats()
            // when the user switches back to it — see switchLogView).
            const inView = logView.steps
              ? batch.filter((l) => logView.steps.includes(l.stepIndex))
              : batch;
            const atTail = logWindow.startRow + logWindow.lines.length >= logWindow.totalCount;
            if (atTail) {
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
  // where the second connection's `logLines = []` reset could wipe out or race
  // with logs already delivered by the first, leaving the panel stuck empty.
  if (typeof window !== "undefined")
    window.addEventListener("resize", measureLogMetrics);
  onDestroy(() => {
    if (abortController) abortController.abort();
    stopStepPolling();
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
            >{logMatches.length ? logMatchPos + 1 : 0} / {logMatches.length}</span
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
          <div
            class="log-row"
            class:log-row-wrap={logWrap}
            class:log-row-current={logStart + i === curMatchRow}
          >
            {#if selectedStep === null}<span class="meta log-step-label"
                >{stepName(l.stepIndex)}</span
              >{/if}<span
              class={l.stream === "stderr" && !$stderrPlain
                ? "log-stderr"
                : "log-stdout"}
              >{#if logQuery}{#each highlightSegments(l.line, logQuery) as seg}{#if seg.hit}<mark class="log-hit" class:log-hit-current={logStart + i === curMatchRow}>{seg.t}</mark>{:else}{seg.t}{/if}{/each}{:else}{l.line}{/if}</span
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
  .log-truncated {
    font-size: 0.75rem;
    color: var(--text-muted);
    background: var(--surface-alt, var(--surface));
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 0.3rem 0.6rem;
    margin-bottom: 0.4rem;
  }
  .log-truncated code {
    font-size: 0.72rem;
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
