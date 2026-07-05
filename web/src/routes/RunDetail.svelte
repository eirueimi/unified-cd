<script>
  import { onDestroy, tick } from "svelte";
  import { push } from "svelte-spa-router";
  import { get } from "svelte/store";
  import AuthSetup from "../components/AuthSetup.svelte";
  import { apiFetch, token, serverURL } from "../lib/api.js";
  import { statusBadge, fmtTime } from "../lib/utils.js";

  export let params;
  $: runID = params.id;

  let run = null,
    logLines = [],
    steps = [],
    approvals = [],
    loading = true,
    error = "";
  let logBox,
    selectedStep = null,
    selectedParallelGroup = null,
    abortController = null,
    stepsTimer = null;
  let approvalComments = {};

  $: groupedStages = (() => {
    const map = new Map();
    for (const s of steps) {
      if (!map.has(s.stageIndex)) map.set(s.stageIndex, []);
      map.get(s.stageIndex).push(s);
    }
    return [...map.entries()]
      .sort(([a], [b]) => a - b)
      .map(([stageIndex, stageSteps]) => ({ stageIndex, steps: stageSteps }));
  })();

  $: filteredLogs =
    selectedStep !== null
      ? logLines.filter((l) => l.stepIndex === selectedStep)
      : selectedParallelGroup !== null
      ? logLines.filter((l) => selectedParallelGroup.includes(l.stepIndex))
      : logLines;

  // ---- Virtualized log rendering ----
  // Logs can reach tens of thousands of lines (e.g. Unity's `-logFile -`), which
  // freezes the browser if every line becomes a DOM node. We render only the
  // rows inside the scroll viewport (plus a small overscan) using fixed-height
  // rows and top/bottom spacer divs, so the scrollbar still reflects the full log.
  const LOG_ROW_H = 20; // px — must match .log-row height in <style>
  const LOG_OVERSCAN = 15; // extra rows rendered above and below the viewport
  let logScrollTop = 0;
  let logViewportH = 600;
  let logStick = true; // keep auto-scrolling to the bottom while the user is there
  $: logTotal = filteredLogs.length;
  $: logStart = Math.max(0, Math.floor(logScrollTop / LOG_ROW_H) - LOG_OVERSCAN);
  $: logEnd = Math.min(
    logTotal,
    Math.ceil((logScrollTop + logViewportH) / LOG_ROW_H) + LOG_OVERSCAN,
  );
  $: visibleLogs = filteredLogs.slice(logStart, logEnd);
  function measureLogViewport() {
    if (logBox) logViewportH = logBox.clientHeight;
  }
  function onLogScroll() {
    if (!logBox) return;
    logScrollTop = logBox.scrollTop;
    // Stick to the bottom only while the user is within ~2 rows of the end.
    logStick =
      logBox.scrollHeight - logBox.scrollTop - logBox.clientHeight <
      LOG_ROW_H * 2;
  }
  // Reset the scroll position when the step/parallel filter changes so the newly
  // filtered log starts from the top.
  $: resetLogScrollOnFilter(selectedStep, selectedParallelGroup);
  function resetLogScrollOnFilter() {
    logScrollTop = 0;
    logStick = true;
    tick().then(() => {
      if (logBox) logBox.scrollTop = 0;
    });
  }

  // ---- In-app log search ----
  // Native Ctrl+F only sees the virtualized (visible) rows, so we provide search
  // over the FULL log: it scans every line in memory, counts matches, jumps the
  // virtual list to each match, and highlights the hits.
  let logQuery = "";
  let logMatchPos = 0;
  $: logMatches = logQuery
    ? filteredLogs.reduce((acc, l, idx) => {
        if (l.line && l.line.toLowerCase().includes(logQuery.toLowerCase()))
          acc.push(idx);
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
      logBox.scrollTop = Math.max(
        0,
        rowIdx * LOG_ROW_H - logBox.clientHeight / 2,
      );
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
      await Promise.all([loadSteps(), loadRun(), loadApprovals()]);
      if (run && ["Succeeded", "Failed", "Cancelled"].includes(run.status))
        stopStepPolling();
    }, 3000);
  }
  async function startSSE() {
    if (abortController) {
      abortController.abort();
      abortController = null;
    }
    logLines = [];
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
        for (const part of parts) {
          const line = part.replace(/^data: /, "").trim();
          if (!line) continue;
          try {
            const data = JSON.parse(line);
            if (data.type === "log") {
              logLines = [...logLines, data];
              if (logStick && selectedStep === null) {
                await tick();
                if (logBox) {
                  logBox.scrollTop = logBox.scrollHeight;
                  logScrollTop = logBox.scrollTop;
                }
              }
            } else if (data.type === "status") {
              if (run) run = { ...run, status: data.status };
              if (["Succeeded", "Failed", "Cancelled"].includes(data.status)) {
                await loadSteps();
                stopStepPolling();
                abortController.abort();
                return;
              }
            }
          } catch {}
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
    await Promise.all([loadRun(), loadSteps(), loadApprovals()]);
    loading = false;
    await tick();
    measureLogViewport();
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
    window.addEventListener("resize", measureLogViewport);
  onDestroy(() => {
    if (abortController) abortController.abort();
    stopStepPolling();
    if (typeof window !== "undefined")
      window.removeEventListener("resize", measureLogViewport);
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
      <h2 style="margin-bottom:0.5rem">Steps</h2>
      <div class="step-list">
        {#each groupedStages as group (group.stageIndex)}
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
    <div class="log-box" bind:this={logBox} on:scroll={onLogScroll}>
      {#if !filteredLogs.length}
        <span style="color:var(--text-muted)">Waiting for logs…</span>
      {:else}
        <div style="height:{logStart * LOG_ROW_H}px" aria-hidden="true"></div>
        {#each visibleLogs as l, i (l.seq)}
          <div class="log-row" class:log-row-current={logStart + i === curMatchRow}>
            {#if selectedStep === null}<span class="meta log-step-label"
                >{stepName(l.stepIndex)}</span
              >{/if}<span
              class={l.stream === "stderr" ? "log-stderr" : "log-stdout"}
              >{#if logQuery}{#each highlightSegments(l.line, logQuery) as seg}{#if seg.hit}<mark class="log-hit" class:log-hit-current={logStart + i === curMatchRow}>{seg.t}</mark>{:else}{seg.t}{/if}{/each}{:else}{l.line}{/if}</span
            >
          </div>
        {/each}
        <div
          style="height:{(logTotal - logEnd) * LOG_ROW_H}px"
          aria-hidden="true"
        ></div>
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
